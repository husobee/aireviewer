// SPDX-License-Identifier: MIT

package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/husobee/aireviewer/internal/config"
	"github.com/husobee/aireviewer/internal/diffmap"
	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/history"
	"github.com/husobee/aireviewer/internal/reviewers"
)

func registerAugmentTools(s *mcp.Server, h *handlers) {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	local := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true}
	mcp.AddTool(s, &mcp.Tool{Name: "pr_issues", Annotations: local,
		Description: "Fetch the issues linked to the PR/MR (the forge's closing references plus closing keywords in the description) and save " +
			"them as issues.json. Required before issue-quote evidence can be validated. Issue text is untrusted data."},
		h.prIssues)
	mcp.AddTool(s, &mcp.Tool{Name: "repo_metrics", Annotations: local,
		Description: "Deterministic change-history signals for each changed file at the merge base (change frequency, bug-fix commits, " +
			"authors, hotspot percentile) and a reproducible risk score with its formula and an explicit confidence. Values are recorded as " +
			"metrics (source repo_metrics) that reports may cite."},
		h.repoMetrics)
	mcp.AddTool(s, &mcp.Tool{Name: "symbol_refs", Annotations: local,
		Description: "Find references at head to the symbols defined by modified or deleted diff-map units (callers, users), with per-symbol " +
			"file fan-in (centrality) and dangling references to deleted symbols. Also lists changed interface files (API, schema, migrations). " +
			"Counts are recorded as metrics (source symbol_refs)."},
		h.symbolRefs)
	mcp.AddTool(s, &mcp.Tool{Name: "reviewers_suggest", Annotations: ro,
		Description: "Deterministic reviewer suggestions (no LLM): CODEOWNERS at the merge base plus git expertise on the changed files, " +
			"scored on expertise, experience, ownership, workload and knowledge sharing with configurable weights. Suggestion only; " +
			"requesting reviewers is a gated forge write (M3)."},
		h.reviewersSuggest)
}

func (h *handlers) loadConfig(ctx context.Context, st *checkoutState) (config.Config, string) {
	b, err := h.git(st.Repo).Show(ctx, st.MergeBase, ".aireviewer.yml")
	if err != nil {
		return config.Default(), ""
	}
	c, err := config.Parse(b)
	if err != nil {
		return c, err.Error() + " (using defaults)"
	}
	return c, ""
}

func (h *handlers) loadPR(runID string) (*forge.PR, error) {
	raw, err := h.cfg.Store.Get(runID, "context.json")
	if err != nil {
		return nil, fmt.Errorf("run %q has no PR context; call pr_checkout first", runID)
	}
	var pr forge.PR
	return &pr, json.Unmarshal(raw, &pr)
}

func (h *handlers) loadIssues(runID string) ([]forge.Issue, bool) {
	raw, err := h.cfg.Store.Get(runID, "issues.json")
	if err != nil {
		return nil, false
	}
	var is []forge.Issue
	if json.Unmarshal(raw, &is) != nil {
		return nil, false
	}
	return is, true
}

// ---- pr_issues ----

type IssuesOut struct {
	Issues  []forge.Issue `json:"issues"`
	Note    string        `json:"note,omitempty"`
	Path    string        `json:"path"`
	Linkage string        `json:"linkage" jsonschema:"linked, or none: alignment analysis is then not applicable"`
}

func (h *handlers) prIssues(ctx context.Context, _ *mcp.CallToolRequest, in RunIn) (*mcp.CallToolResult, IssuesOut, error) {
	if st, err := h.loadCheckout(in.RunID); err == nil && st.isBranch() {
		// Before the PR exists, the linked issues are the author's choices
		// in the draft (create_draft_update keeps issues.json current).
		is, _ := h.loadIssues(in.RunID)
		if is == nil {
			is = []forge.Issue{}
		}
		out := IssuesOut{Issues: is, Linkage: "linked", Note: "branch run: issues linked in the PR draft"}
		if len(is) == 0 {
			out.Linkage = "none"
		}
		return nil, out, nil
	}
	pr, err := h.loadPR(in.RunID)
	if err != nil {
		return nil, IssuesOut{}, err
	}
	p, err := h.cfg.Providers.Get(ctx, pr.Ref.Repo)
	if err != nil {
		return nil, IssuesOut{}, err
	}
	out := IssuesOut{Issues: []forge.Issue{}}
	if il, ok := p.(forge.IssueLinker); ok {
		is, err := il.LinkedIssues(ctx, pr)
		if err != nil {
			out.Note = "could not list closing references: " + err.Error()
		} else {
			out.Issues = is
		}
	} else {
		out.Note = "this forge adapter cannot list linked issues"
	}
	for i := range out.Issues {
		if len(out.Issues[i].Body) > 20000 {
			out.Issues[i].Body = out.Issues[i].Body[:20000] + "\n… [truncated]"
		}
	}
	data, _ := json.MarshalIndent(out.Issues, "", "  ")
	if out.Path, err = h.cfg.Store.Put(in.RunID, "issues.json", data); err != nil {
		return nil, IssuesOut{}, err
	}
	out.Linkage = "linked"
	if len(out.Issues) == 0 {
		out.Linkage = "none"
	}
	return nil, out, nil
}

// ---- repo_metrics ----

type RepoMetricsOut struct {
	Risk             history.Risk                  `json:"risk"`
	Files            map[string]*history.FileStats `json:"files"`
	RepoCommits365   int                           `json:"repo_commits_365d"`
	RepoCommitsTotal int                           `json:"repo_commits_total" jsonschema:"capped at 100000"`
	AnchoredAt       time.Time                     `json:"anchored_at" jsonschema:"merge-base commit time all windows are relative to"`
	MetricNames      []string                      `json:"metric_names" jsonschema:"names recorded under source repo_metrics that reports may cite"`
	ConfigWarning    string                        `json:"config_warning,omitempty"`
	HistoryTruncated bool                          `json:"history_truncated"`
}

func (h *handlers) historyFor(ctx context.Context, st *checkoutState) (time.Time, []history.Commit, int, error) {
	g := h.git(st.Repo)
	return history.Load(ctx, func(ctx context.Context, args ...string) (string, error) { return g.Run(ctx, "", args...) },
		st.MergeBase, 365*24*time.Hour)
}

// historyPaths maps each changed file (diff-map path) to the path whose
// history matters (the old path for renames and deletions).
func historyPaths(m *diffmap.Map) (map[string]string, map[string]int) {
	hp, changed := map[string]string{}, map[string]int{}
	for _, f := range m.Files {
		p := f.Path
		if f.OldPath != "" {
			p = f.OldPath
		}
		hp[f.Path] = p
		changed[f.Path] = f.Additions + f.Deletions
	}
	return hp, changed
}

func (h *handlers) repoMetrics(ctx context.Context, _ *mcp.CallToolRequest, in RunIn) (*mcp.CallToolResult, RepoMetricsOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, RepoMetricsOut{}, err
	}
	m, err := h.loadMap(in.RunID)
	if err != nil {
		return nil, RepoMetricsOut{}, err
	}
	cfg, warn := h.loadConfig(ctx, st)
	now, commits, total, err := h.historyFor(ctx, st)
	if err != nil {
		return nil, RepoMetricsOut{}, err
	}
	hp, changed := historyPaths(m)
	var hist []string
	for _, p := range hp {
		hist = append(hist, p)
	}
	raw := history.Stats(now, commits, hist, history.BugfixRE(cfg.Risk.BugfixKeywords))
	stats := map[string]*history.FileStats{}
	for path, hpath := range hp {
		s := *raw[hpath]
		s.Path = path
		stats[path] = &s
	}
	risk := history.Score(stats, changed, len(commits), total, cfg.Risk.MinHistoryCommits)

	kv := map[string]any{"pr_risk": risk.Score, "repo.commits_365d": len(commits), "repo.commits_total": total}
	for _, fr := range risk.Files {
		kv[fr.Path+".risk"] = fr.Score
	}
	for p, s := range stats {
		kv[p+".commits_365d"] = s.Commits365
		kv[p+".commits_90d"] = s.Commits90
		kv[p+".commits_30d"] = s.Commits30
		kv[p+".bugfix_365d"] = s.Bugfix365
		kv[p+".authors_365d"] = s.Authors365
		kv[p+".hotspot_percentile"] = s.HotspotPercentile
	}
	if err := h.recordMetrics(in.RunID, "repo_metrics", kv); err != nil {
		return nil, RepoMetricsOut{}, err
	}
	out := RepoMetricsOut{Risk: risk, Files: stats, RepoCommits365: len(commits), RepoCommitsTotal: total, AnchoredAt: now,
		ConfigWarning: warn, HistoryTruncated: len(commits) >= history.MaxCommits}
	for k := range kv {
		out.MetricNames = append(out.MetricNames, k)
	}
	sort.Strings(out.MetricNames)
	data, _ := json.MarshalIndent(out, "", "  ")
	if _, err := h.cfg.Store.Put(in.RunID, "repo_metrics.json", data); err != nil {
		return nil, RepoMetricsOut{}, err
	}
	return nil, out, nil
}

// ---- symbol_refs ----

type SymbolRefsIn struct {
	RunID string   `json:"run_id"`
	Units []string `json:"units,omitempty" jsonschema:"diff-map unit ids to analyse (default: modified/deleted function, method and class units outside tests, max 40)"`
}

type SymbolRef struct {
	UnitID        string   `json:"unit_id"`
	Symbol        string   `json:"symbol"`
	Change        string   `json:"change"`
	RefFiles      int      `json:"ref_files" jsonschema:"distinct other files referencing the symbol at head (fan-in / centrality)"`
	RefCount      int      `json:"ref_count"`
	RefsInPRFiles int      `json:"refs_in_pr_files" jsonschema:"references in files this PR also changes"`
	Dangling      bool     `json:"dangling" jsonschema:"deleted symbol that is still referenced at head"`
	Sample        []string `json:"sample" jsonschema:"up to 12 path:line:text references (untrusted)"`
	Note          string   `json:"note,omitempty"`
}

type SymbolRefsOut struct {
	Symbols        []SymbolRef `json:"symbols"`
	InterfaceFiles []string    `json:"interface_files" jsonschema:"changed API/schema/migration/config-contract files"`
	Method         string      `json:"method"`
}

var interfaceFile = regexp.MustCompile(`(?i)(\.proto|\.graphql|\.gql|openapi[^/]*\.(ya?ml|json)|swagger[^/]*\.(ya?ml|json)|\.avsc|schema\.(sql|json|prisma)|(^|/)migrations?/|\.thrift)$|(^|/)migrations?/`)

func symbolName(u *diffmap.Unit) string {
	n := u.Name
	if i := strings.Index(n, " ("); i >= 0 {
		n = n[:i]
	}
	if i := strings.LastIndex(n, "."); i >= 0 {
		n = n[i+1:]
	}
	return n
}

func (h *handlers) symbolRefs(ctx context.Context, _ *mcp.CallToolRequest, in SymbolRefsIn) (*mcp.CallToolResult, SymbolRefsOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, SymbolRefsOut{}, err
	}
	m, err := h.loadMap(in.RunID)
	if err != nil {
		return nil, SymbolRefsOut{}, err
	}
	out := SymbolRefsOut{Symbols: []SymbolRef{}, InterfaceFiles: []string{},
		Method: "word-boundary git grep at head, excluding the symbol's own definition lines; textual, so same-named symbols are counted together"}
	for _, f := range m.Files {
		if interfaceFile.MatchString(f.Path) {
			out.InterfaceFiles = append(out.InterfaceFiles, f.Path)
		}
	}
	var units []*diffmap.Unit
	if len(in.Units) > 0 {
		for _, id := range in.Units {
			if u, ok := m.Unit(id); ok {
				units = append(units, u)
			}
		}
	} else {
		for _, id := range m.Order {
			u, _ := m.Unit(id)
			if u.IsTest || (u.Change != diffmap.StatusModified && u.Change != diffmap.StatusDeleted) {
				continue
			}
			if u.Kind == diffmap.KindFunction || u.Kind == diffmap.KindMethod || u.Kind == diffmap.KindClass {
				units = append(units, u)
			}
		}
	}
	if len(units) > 40 {
		units = units[:40]
	}
	g := h.git(st.Repo)
	kv := map[string]any{}
	for _, u := range units {
		sym := symbolName(u)
		sr := SymbolRef{UnitID: u.ID, Symbol: sym, Change: u.Change, Sample: []string{}}
		if len(sym) < 3 || !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(sym) {
			sr.Note = "symbol too short or not an identifier; skipped"
			out.Symbols = append(out.Symbols, sr)
			continue
		}
		// Unexported Go identifiers are package-private: only search the
		// unit's own package directory.
		var specs []string
		if u.Language == "go" && sym[0] >= 'a' && sym[0] <= 'z' {
			dir := path.Dir(u.File)
			if dir == "." {
				specs = []string{":(glob)*.go"}
			} else {
				specs = []string{":(glob)" + dir + "/*.go"}
			}
			sr.Note = "package-scoped search (unexported Go identifier)"
		}
		matches, err := g.Grep(ctx, st.HeadSHA, `(^|[^A-Za-z0-9_])`+sym+`([^A-Za-z0-9_]|$)`, specs, false)
		if err != nil {
			return nil, SymbolRefsOut{}, err
		}
		files := map[string]bool{}
		for _, mt := range matches {
			parts := strings.SplitN(mt, ":", 3)
			if len(parts) < 3 {
				continue
			}
			path := parts[0]
			var line int
			_, _ = fmt.Sscanf(parts[1], "%d", &line)
			if path == u.File && len(u.NewRange) == 2 && line >= u.NewRange[0] && line <= u.NewRange[1] {
				continue // the definition itself
			}
			sr.RefCount++
			if path != u.File {
				files[path] = true
			}
			if m.HasFile(path) {
				sr.RefsInPRFiles++
			}
			if len(sr.Sample) < 12 {
				if r := []rune(mt); len(r) > 200 {
					mt = string(r[:200]) + "…"
				}
				sr.Sample = append(sr.Sample, mt)
			}
		}
		sr.RefFiles = len(files)
		if sr.RefFiles > 50 {
			sr.Note = strings.TrimPrefix(sr.Note+"; very common identifier: textual matching likely over-counts", "; ")
		}
		sr.Dangling = u.Change == diffmap.StatusDeleted && sr.RefCount > 0
		kv[u.ID+".ref_files"] = sr.RefFiles
		kv[u.ID+".ref_count"] = sr.RefCount
		out.Symbols = append(out.Symbols, sr)
	}
	sort.SliceStable(out.Symbols, func(i, j int) bool {
		if out.Symbols[i].Dangling != out.Symbols[j].Dangling {
			return out.Symbols[i].Dangling
		}
		return out.Symbols[i].RefFiles > out.Symbols[j].RefFiles
	})
	if err := h.recordMetrics(in.RunID, "symbol_refs", kv); err != nil {
		return nil, SymbolRefsOut{}, err
	}
	return nil, out, nil
}

// ---- reviewers_suggest ----

type ReviewersOut struct {
	AlreadyAssigned []string              `json:"already_assigned" jsonschema:"if non-empty, the paper skips Reviewer Selection"`
	Mode            reviewers.Mode        `json:"mode" jsonschema:"expertise_first for high-risk changes (knowledge-sharing weight dropped), else balanced"`
	Weights         config.Weights        `json:"weights"`
	RiskScore       int                   `json:"risk_score" jsonschema:"-1 if repo_metrics has not run"`
	Candidates      []reviewers.Candidate `json:"candidates"`
	Notes           []string              `json:"notes"`
}

func (h *handlers) reviewersSuggest(ctx context.Context, _ *mcp.CallToolRequest, in RunIn) (*mcp.CallToolResult, ReviewersOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, ReviewersOut{}, err
	}
	m, err := h.loadMap(in.RunID)
	if err != nil {
		return nil, ReviewersOut{}, err
	}
	pr, err := h.loadPR(in.RunID)
	if err != nil {
		return nil, ReviewersOut{}, err
	}
	cfg, warn := h.loadConfig(ctx, st)
	out := ReviewersOut{AlreadyAssigned: append([]string{}, pr.Reviewers...), RiskScore: -1, Notes: []string{}}
	if warn != "" {
		out.Notes = append(out.Notes, warn)
	}
	if raw, err := h.cfg.Store.Get(in.RunID, "repo_metrics.json"); err == nil {
		var rm RepoMetricsOut
		if json.Unmarshal(raw, &rm) == nil {
			out.RiskScore = rm.Risk.Score
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		out.Notes = append(out.Notes, "repo_metrics has not run; risk-aware weighting uses the balanced mode")
	}
	now, commits, _, err := h.historyFor(ctx, st)
	if err != nil {
		return nil, ReviewersOut{}, err
	}
	hp, _ := historyPaths(m)
	var changed, hist []string
	for p, hpath := range hp {
		changed = append(changed, p)
		hist = append(hist, hpath)
	}
	sort.Strings(changed)
	authors := history.Expertise(now, commits, hist)

	g := h.git(st.Repo)
	var rules []reviewers.Rule
	for _, p := range reviewers.CodeownersPaths {
		if b, err := g.Show(ctx, st.MergeBase, p); err == nil {
			rules = reviewers.ParseCodeowners(string(b))
			out.Notes = append(out.Notes, "CODEOWNERS read from "+p+" at the merge base")
			break
		}
	}
	prov, err := h.cfg.Providers.Get(ctx, st.Repo)
	if err != nil {
		return nil, ReviewersOut{}, err
	}
	emailLogin := map[string]string{}
	if am, ok := prov.(forge.AuthorMapper); ok {
		for i, p := range hist {
			if i >= 10 {
				out.Notes = append(out.Notes, "author mapping limited to the first 10 changed files")
				break
			}
			mp, err := am.CommitAuthorLogins(ctx, st.Repo, p, now.AddDate(-1, 0, 0))
			if err != nil {
				out.Notes = append(out.Notes, "author mapping failed: "+err.Error())
				break
			}
			for e, l := range mp {
				emailLogin[e] = l
			}
		}
	} else {
		out.Notes = append(out.Notes, "this forge cannot map git authors to usernames; history-based candidates are shown as git identities")
	}
	in1 := reviewers.Input{ChangedFiles: changed, Authors: authors, EmailLogin: emailLogin, Rules: rules,
		Workload: map[string]int{}, PRAuthor: pr.Author, Assigned: pr.Reviewers, RiskScore: out.RiskScore, Config: cfg}
	// First pass to pick whom to query for workload, then score with it.
	first, _ := reviewers.Suggest(in1)
	if wr, ok := prov.(forge.WorkloadReporter); ok {
		queried := 0
		for _, c := range first {
			if !c.Mapped || strings.Contains(c.Identity, "/") || queried >= 8 {
				continue
			}
			if n, err := wr.OpenReviewRequests(ctx, st.Repo, strings.TrimPrefix(c.Identity, "@")); err == nil {
				in1.Workload[strings.TrimPrefix(c.Identity, "@")] = n
			}
			queried++
		}
	}
	out.Candidates, out.Mode = reviewers.Suggest(in1)
	out.Weights = cfg.Reviewers.Weights
	if out.Mode == reviewers.ExpertiseFirst {
		out.Weights.KnowledgeShare = 0
	}
	if out.Candidates == nil {
		out.Candidates = []reviewers.Candidate{}
	}
	return nil, out, nil
}
