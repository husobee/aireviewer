// SPDX-License-Identifier: MIT

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/husobee/aireviewer/internal/diffmap"
	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/gitrepo"
	"github.com/husobee/aireviewer/internal/schema"
)

func registerReviewTools(s *mcp.Server, h *handlers) {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	local := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true}

	mcp.AddTool(s, &mcp.Tool{Name: "pr_checkout", Annotations: local,
		Description: "Fetch the PR/MR head into a cached local mirror (outside the workspace) and compute the merge base. " +
			"Read PR code with code_read / code_search / code_list, never directly from disk. Never pushes or writes to the forge."},
		h.prCheckout)
	mcp.AddTool(s, &mcp.Tool{Name: "diffmap_build", Annotations: local,
		Description: "Build the semantic diff-map for a checked-out run: the diff (merge base → head) grouped into logical units " +
			"(functions, methods, classes, config files) with stable unit ids. Saves diffmap.json and returns the unit summary and review order."},
		h.diffmapBuild)
	mcp.AddTool(s, &mcp.Tool{Name: "diffmap_get", Annotations: ro,
		Description: "Return the diff-map summary (files, units, review order) with per-unit counts of accepted findings."},
		h.diffmapGet)
	mcp.AddTool(s, &mcp.Tool{Name: "diffmap_unit", Annotations: ro,
		Description: "Return one diff-map unit with numbered diff lines (old/new line numbers) and every accepted finding anchored to it."},
		h.diffmapUnit)
	mcp.AddTool(s, &mcp.Tool{Name: "finding_get", Annotations: ro,
		Description: "Return one accepted finding rendered for the reviewer: header, proposal, and a highlighted Markdown diff excerpt " +
			"(cited lines marked ▶, the finding shown inline as an @@ ▲ line) for each piece of code evidence. Print `rendered` verbatim."},
		h.findingGet)
	mcp.AddTool(s, &mcp.Tool{Name: "findings_list", Annotations: ro,
		Description: "List accepted reviewer findings for the current head, most severe first, for walking through them one by one."},
		h.findingsList)
	mcp.AddTool(s, &mcp.Tool{Name: "report_slot", Annotations: local,
		Description: "Orchestrator only: reserve a report slot for one subagent dispatch and get its submit token. " +
			"Pass agent_name and token to exactly that subagent; report_submit rejects reports without the matching token."},
		h.reportSlot)
	mcp.AddTool(s, &mcp.Tool{Name: "report_submit", Annotations: local,
		Description: "Submit an agent report (JSON string conforming to the aireviewer report schema v1). It is validated: schema errors reject " +
			"the whole report; claims whose evidence cites non-existent files/lines, metrics not produced by a tool, or missing artifacts are dropped. " +
			"Requires the submit token your orchestrator gave you. Returns what was kept and why anything was dropped so you can fix and resubmit. " +
			"Saves to the local run context only."},
		h.reportSubmit)
	mcp.AddTool(s, &mcp.Tool{Name: "reports_get", Annotations: ro,
		Description: "Return all accepted reports for the current PR head in a stage (default review)."},
		h.reportsGet)
	mcp.AddTool(s, &mcp.Tool{Name: "evidence_check", Annotations: ro,
		Description: "Check evidence items (code/metric/trace) against the checked-out PR without submitting a report. Use before citing code in an answer."},
		h.evidenceCheck)
	mcp.AddTool(s, &mcp.Tool{Name: "report_schema", Annotations: ro,
		Description: "Return the JSON Schema (v1) that agent reports must conform to."},
		h.reportSchema)
}

// checkoutState is persisted as checkout.json in the run context.
type checkoutState struct {
	Repo       forge.Repo `json:"repo"`
	Number     int        `json:"number"`
	BaseBranch string     `json:"base_branch"`
	// Branch runs (/pr-create, M4): a local branch with no PR yet.
	Mode       string `json:"mode,omitempty" jsonschema:"empty for a PR run, branch for /pr-create"`
	HeadBranch string `json:"head_branch,omitempty"`
	RemoteName string `json:"remote_name,omitempty"`
	gitrepo.Checkout
}

func (st *checkoutState) isBranch() bool { return st.Mode == "branch" }

func (h *handlers) git(repo forge.Repo) *gitrepo.Git {
	tok := ""
	if h.cfg.Tokens != nil {
		tok = h.cfg.Tokens(context.Background(), repo.Forge, repo.Host)
	}
	remote := gitrepo.RemoteFor(repo, tok)
	if h.cfg.RemoteFor != nil {
		remote = h.cfg.RemoteFor(repo, tok)
	}
	return gitrepo.Open(h.cfg.CacheDir, repo, remote)
}

func (h *handlers) loadCheckout(runID string) (*checkoutState, error) {
	raw, err := h.cfg.Store.Get(runID, "checkout.json")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("run %q has no checkout; call pr_checkout first", runID)
	}
	if err != nil {
		return nil, err
	}
	var st checkoutState
	return &st, json.Unmarshal(raw, &st)
}

func (h *handlers) loadMap(runID string) (*diffmap.Map, error) {
	raw, err := h.cfg.Store.Get(runID, "diffmap.json")
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("run %q has no diff-map; call diffmap_build first", runID)
	}
	if err != nil {
		return nil, err
	}
	var m diffmap.Map
	return &m, json.Unmarshal(raw, &m)
}

func ensureWorkspaceIgnore(root string) error {
	dir := filepath.Join(root, ".aireviewer")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	p := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	return os.WriteFile(p, []byte("# aireviewer workspace: local run artifacts and PR worktrees\n*\n"), 0o600)
}

// ---- pr_checkout ----

type CheckoutOut struct {
	ResolveOut
	Worktree   string `json:"worktree" jsonschema:"cache path of the PR checkout (internal); read code with code_read/code_search, not from disk"`
	HeadSHA    string `json:"head_sha"`
	MergeBase  string `json:"merge_base"`
	BaseBranch string `json:"base_branch"`
	Title      string `json:"title"`
}

func (h *handlers) prCheckout(ctx context.Context, _ *mcp.CallToolRequest, in URLIn) (*mcp.CallToolResult, CheckoutOut, error) {
	r, err := h.resolve(in.URL)
	if err != nil {
		return nil, CheckoutOut{}, err
	}
	p, err := h.cfg.Providers.Get(ctx, r.Ref.Repo)
	if err != nil {
		return nil, CheckoutOut{}, err
	}
	pr, err := p.GetPR(ctx, r.Ref)
	if err != nil {
		return nil, CheckoutOut{}, fmt.Errorf("fetch %s %s: %w", r.Abbrev, r.Short, err)
	}
	data, _ := json.MarshalIndent(pr, "", "  ")
	if _, err := h.cfg.Store.Put(r.RunID, "context.json", data); err != nil {
		return nil, CheckoutOut{}, err
	}
	if err := ensureWorkspaceIgnore(h.cfg.RepoRoot); err != nil {
		return nil, CheckoutOut{}, err
	}
	// The worktree only hydrates blobs in the blobless mirror. It lives in the
	// cache, outside the workspace, so PR-controlled files (nested CLAUDE.md,
	// skills, symlinks) are never in a place Claude Code reads on its own.
	// All code access goes through git objects (code_read/search/list).
	wt := filepath.Join(h.cfg.CacheDir, "worktrees", r.RunID)
	co, err := h.git(r.Ref.Repo).Prepare(ctx, pr, wt)
	if err != nil {
		return nil, CheckoutOut{}, fmt.Errorf("checkout %s %s: %w", r.Abbrev, r.Short, err)
	}
	st := checkoutState{Repo: r.Ref.Repo, Number: r.Ref.Number, BaseBranch: pr.BaseBranch, Checkout: *co}
	data, _ = json.MarshalIndent(st, "", "  ")
	if _, err := h.cfg.Store.Put(r.RunID, "checkout.json", data); err != nil {
		return nil, CheckoutOut{}, err
	}
	return nil, CheckoutOut{ResolveOut: r, Worktree: wt, HeadSHA: co.HeadSHA, MergeBase: co.MergeBase,
		BaseBranch: pr.BaseBranch, Title: pr.Title}, nil
}

// ---- diff-map ----

type RunIn struct {
	RunID string `json:"run_id"`
}

// source reads file content from git objects at the head and merge-base
// commits. It never touches the filesystem, so symlinks or files planted by a
// PR cannot redirect reads outside the repository.
type source struct {
	g    *gitrepo.Git
	head string
	base string
}

func (s source) Head(ctx context.Context, p string) ([]byte, error) {
	return s.g.Show(ctx, s.head, p)
}

func (s source) Base(ctx context.Context, p string) ([]byte, error) {
	return s.g.Show(ctx, s.base, p)
}

type DiffmapOut struct {
	RunID string       `json:"run_id"`
	Map   *diffmap.Map `json:"map"`
	// FindingCounts maps unit id → number of accepted findings anchored to it.
	FindingCounts map[string]int `json:"finding_counts,omitempty"`
}

func (h *handlers) diffmapBuild(ctx context.Context, _ *mcp.CallToolRequest, in RunIn) (*mcp.CallToolResult, DiffmapOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, DiffmapOut{}, err
	}
	g := h.git(st.Repo)
	out, err := g.Diff(ctx, st.MergeBase, st.HeadSHA, 3)
	if err != nil {
		return nil, DiffmapOut{}, err
	}
	m, err := diffmap.Build(ctx, diffmap.ParseGitDiff(out), source{g: g, head: st.HeadSHA, base: st.MergeBase}, st.HeadSHA, st.MergeBase)
	if err != nil {
		return nil, DiffmapOut{}, err
	}
	data, _ := json.Marshal(m)
	if _, err := h.cfg.Store.Put(in.RunID, "diffmap.json", data); err != nil {
		return nil, DiffmapOut{}, err
	}
	kv := map[string]any{"units": len(m.Units), "files": len(m.Files)}
	add, del := 0, 0
	for _, u := range m.Units {
		add += u.Additions
		del += u.Deletions
		kv[u.ID+".additions"] = u.Additions
		kv[u.ID+".deletions"] = u.Deletions
	}
	kv["additions"], kv["deletions"] = add, del
	if err := h.recordMetrics(in.RunID, "diffmap", kv); err != nil {
		return nil, DiffmapOut{}, err
	}
	return nil, DiffmapOut{RunID: in.RunID, Map: m.Summary()}, nil
}

func (h *handlers) diffmapGet(_ context.Context, _ *mcp.CallToolRequest, in RunIn) (*mcp.CallToolResult, DiffmapOut, error) {
	m, err := h.loadMap(in.RunID)
	if err != nil {
		return nil, DiffmapOut{}, err
	}
	counts := map[string]int{}
	hidden, _, _ := h.findingsHidden(in.RunID)
	for _, r := range h.findingReports(in.RunID, m.HeadSHA) {
		if hidden {
			break
		}
		for _, c := range r.Claims {
			for _, a := range c.Anchors {
				counts[a]++
			}
		}
	}
	sum := m.Summary()
	if order, ok := h.riskOrder(in.RunID, m); ok {
		sum.Order = order
	}
	return nil, DiffmapOut{RunID: in.RunID, Map: sum, FindingCounts: counts}, nil
}

type UnitIn struct {
	RunID  string `json:"run_id"`
	UnitID string `json:"unit_id" jsonschema:"diff-map unit id, e.g. unit:pkg/client.go#Client.Do"`
}

type AnchoredClaim struct {
	Agent string       `json:"agent"`
	Claim schema.Claim `json:"claim"`
}

type UnitOut struct {
	Note        string          `json:"note,omitempty"`
	Unit        diffmap.Unit    `json:"unit"`
	Rendered    string          `json:"rendered" jsonschema:"numbered diff: old-line new-line op text"`
	Highlighted string          `json:"highlighted" jsonschema:"Markdown diff block with every finding's cited lines marked; print verbatim to the reviewer"`
	Findings    []AnchoredClaim `json:"findings"`
}

// claimLabel is the one-line label shown next to highlighted lines.
func claimLabel(c schema.Claim) string {
	s := c.Statement
	if r := []rune(s); len(r) > 90 {
		s = string(r[:89]) + "…"
	}
	return fmt.Sprintf("%s %s/%s — %s", c.ID, c.Severity, c.Confidence, s)
}

// marksFor converts a claim's code evidence on a file into highlight marks.
func marksFor(c schema.Claim, file string) []diffmap.Mark {
	var out []diffmap.Mark
	for _, e := range c.Evidence {
		if e.Kind == "code" && e.File == file && len(e.Lines) == 2 {
			side := e.Side
			if side == "" {
				side = "new"
			}
			out = append(out, diffmap.Mark{Label: claimLabel(c), Side: side, Start: e.Lines[0], End: e.Lines[1]})
		}
	}
	return out
}

func (h *handlers) diffmapUnit(_ context.Context, _ *mcp.CallToolRequest, in UnitIn) (*mcp.CallToolResult, UnitOut, error) {
	m, err := h.loadMap(in.RunID)
	if err != nil {
		return nil, UnitOut{}, err
	}
	u, ok := m.Unit(in.UnitID)
	if !ok {
		return nil, UnitOut{}, fmt.Errorf("unknown unit %q; call diffmap_get for valid ids", in.UnitID)
	}
	out := UnitOut{Unit: *u, Rendered: u.Render(), Findings: []AnchoredClaim{}}
	h.markViewed(in.RunID, in.UnitID)
	hidden, viewed, total := h.findingsHidden(in.RunID)
	var marks []diffmap.Mark
	for _, r := range h.findingReports(in.RunID, m.HeadSHA) {
		if hidden {
			out.Note = fmt.Sprintf("reviewer-first mode: findings stay hidden until every reviewable unit has been viewed (%d/%d)", viewed, total)
			break
		}
		for _, c := range r.Claims {
			for _, a := range c.Anchors {
				if a == in.UnitID {
					out.Findings = append(out.Findings, AnchoredClaim{Agent: r.Agent, Claim: c})
					marks = append(marks, marksFor(c, u.File)...)
					if u.OldFile != "" {
						marks = append(marks, marksFor(c, u.OldFile)...)
					}
					break
				}
			}
		}
	}
	header := fmt.Sprintf("%s  [%s, %s, +%d −%d]", u.ID, u.Kind, u.Change, u.Additions, u.Deletions)
	out.Highlighted = diffmap.RenderHighlighted(header, u.Lines, marks)
	return nil, out, nil
}

// ---- finding_get ----

type FindingIn struct {
	RunID string `json:"run_id"`
	ID    string `json:"id" jsonschema:"finding id: S1 (summary), F2 if unambiguous, or agent-qualified like code-reviewer-2:F2"`
}

type FindingOut struct {
	Agent    string       `json:"agent"`
	Claim    schema.Claim `json:"claim"`
	Rendered string       `json:"rendered" jsonschema:"Markdown: finding header, proposal, and a highlighted diff excerpt per piece of evidence; print verbatim"`
}

var sevRank = map[string]int{"blocker": 0, "major": 1, "minor": 2, "info": 3}

// SeverityRank orders severities for walking findings (lower is more severe).
func SeverityRank(s string) int {
	if r, ok := sevRank[s]; ok {
		return r
	}
	return 9
}

func (h *handlers) findingGet(ctx context.Context, _ *mcp.CallToolRequest, in FindingIn) (*mcp.CallToolResult, FindingOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, FindingOut{}, err
	}
	m, err := h.loadMap(in.RunID)
	if err != nil {
		return nil, FindingOut{}, err
	}
	// Synthesised claims (S1…) are addressable too, so include every report.
	reports, err := h.acceptedReports(in.RunID, "review", st.HeadSHA)
	if err != nil {
		return nil, FindingOut{}, err
	}
	aug, _ := h.acceptedReports(in.RunID, "augmentation", st.HeadSHA)
	reports = append(reports, aug...)
	if hidden, viewed, total := h.findingsHidden(in.RunID); hidden {
		return nil, FindingOut{}, fmt.Errorf("reviewer-first mode: view every reviewable unit first (%d/%d viewed)", viewed, total)
	}
	agentWant, idWant, qualified := strings.Cut(in.ID, ":")
	if !qualified {
		agentWant, idWant = "", in.ID
	}
	var hits []AnchoredClaim
	for _, r := range reports {
		if agentWant != "" && r.Agent != agentWant {
			continue
		}
		for _, c := range r.Claims {
			if strings.EqualFold(c.ID, idWant) {
				hits = append(hits, AnchoredClaim{Agent: r.Agent, Claim: c})
			}
		}
	}
	switch {
	case len(hits) == 0:
		return nil, FindingOut{}, fmt.Errorf("no accepted finding %q for head %s", in.ID, st.HeadSHA[:7])
	case len(hits) > 1:
		var names []string
		for _, h := range hits {
			names = append(names, h.Agent+":"+h.Claim.ID)
		}
		return nil, FindingOut{}, fmt.Errorf("finding id %q is ambiguous; use one of %s", in.ID, strings.Join(names, ", "))
	}
	hit := hits[0]
	c := hit.Claim
	src := source{g: h.git(st.Repo), head: st.HeadSHA, base: st.MergeBase}

	var b strings.Builder
	fmt.Fprintf(&b, "**%s · %s · %s confidence** (%s, %s)  \n%s\n", c.ID, c.Severity, c.Confidence, hit.Agent, c.Category, c.Statement)
	if c.ConfidenceReason != "" {
		fmt.Fprintf(&b, "\n_Confidence:_ %s\n", c.ConfidenceReason)
	}
	if c.Proposal != "" {
		fmt.Fprintf(&b, "\n_Proposal:_ %s\n", c.Proposal)
	}
	if len(c.Sources) > 0 {
		fmt.Fprintf(&b, "\n_Sources:_ %s\n", strings.Join(c.Sources, ", "))
	}
	for _, e := range c.Evidence {
		switch e.Kind {
		case "code":
			side := e.Side
			if side == "" {
				side = "new"
			}
			mark := diffmap.Mark{Label: claimLabel(c), Side: side, Start: e.Lines[0], End: e.Lines[1]}
			var lines []diffmap.Line
			header := ""
			// Prefer the diff of the unit that actually contains a cited line,
			// then widen to ±3 lines of context within that unit.
			for _, u := range m.Units {
				if u.File != e.File && u.OldFile != e.File {
					continue
				}
				if diffmap.Excerpt(u.Lines, side, e.Lines[0], e.Lines[1], 0) == nil {
					continue
				}
				lines, header = diffmap.Excerpt(u.Lines, side, e.Lines[0], e.Lines[1], 3), u.ID
				break
			}
			if lines == nil { // unchanged code (e.g. a caller): show the file itself
				var content []byte
				var err error
				if side == "old" {
					content, err = src.Base(ctx, e.File)
				} else {
					content, err = src.Head(ctx, e.File)
				}
				if err != nil {
					fmt.Fprintf(&b, "\n`%s:%d-%d` (%s): could not read file: %v\n", e.File, e.Lines[0], e.Lines[1], side, err)
					continue
				}
				lines = diffmap.FileLines(content, side, e.Lines[0]-3, e.Lines[1]+3)
				header = e.File + "  (unchanged code)"
			}
			fmt.Fprintf(&b, "\n`%s:%d-%d` (%s)\n\n%s", e.File, e.Lines[0], e.Lines[1], side, diffmap.RenderHighlighted(header, lines, []diffmap.Mark{mark}))
		case "metric":
			fmt.Fprintf(&b, "\n- metric `%s` = %v (from `%s`)\n", e.Name, e.Value, e.Source)
		case "trace":
			fmt.Fprintf(&b, "\n- trace `%s`\n", e.Artifact)
		}
	}
	return nil, FindingOut{Agent: hit.Agent, Claim: c, Rendered: b.String()}, nil
}

// ---- findings_list ----

type FindingsListOut struct {
	HeadSHA  string          `json:"head_sha"`
	Findings []AnchoredClaim `json:"findings" jsonschema:"accepted reviewer findings (excluding the summary), most severe first; walk them in this order"`
}

func (h *handlers) findingsList(_ context.Context, _ *mcp.CallToolRequest, in RunIn) (*mcp.CallToolResult, FindingsListOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, FindingsListOut{}, err
	}
	if hidden, viewed, total := h.findingsHidden(in.RunID); hidden {
		return nil, FindingsListOut{}, fmt.Errorf("reviewer-first mode: view every reviewable unit first (%d/%d viewed)", viewed, total)
	}
	out := FindingsListOut{HeadSHA: st.HeadSHA, Findings: []AnchoredClaim{}}
	for _, r := range h.findingReports(in.RunID, st.HeadSHA) {
		for _, c := range r.Claims {
			out.Findings = append(out.Findings, AnchoredClaim{Agent: r.Agent, Claim: c})
		}
	}
	sort.SliceStable(out.Findings, func(i, j int) bool {
		a, b := out.Findings[i].Claim, out.Findings[j].Claim
		if SeverityRank(a.Severity) != SeverityRank(b.Severity) {
			return SeverityRank(a.Severity) < SeverityRank(b.Severity)
		}
		return out.Findings[i].Agent+a.ID < out.Findings[j].Agent+b.ID
	})
	return nil, out, nil
}

// ---- reports ----

var stageRE = regexp.MustCompile(`^(creation|review|augmentation|retro|changeset)$`)

func stageOf(s string) (string, error) {
	if s == "" {
		return "review", nil
	}
	if !stageRE.MatchString(s) {
		return "", fmt.Errorf("stage must be creation, review, augmentation, retro or changeset")
	}
	return s, nil
}

var agentRE = regexp.MustCompile(`[^a-z0-9._-]+`)

func (h *handlers) acceptedReports(runID, stage, head string) ([]schema.Report, error) {
	paths, err := h.cfg.Store.List(runID)
	if err != nil {
		return nil, err
	}
	prefix := stage + "/" + head + "/"
	var out []schema.Report
	for _, p := range paths {
		if !strings.HasPrefix(p, prefix) || !strings.HasSuffix(p, ".json") || strings.HasSuffix(p, ".dropped.json") ||
			path.Dir(p) != strings.TrimSuffix(prefix, "/") {
			continue
		}
		raw, err := h.cfg.Store.Get(runID, p)
		if err != nil {
			continue
		}
		var r schema.Report
		if json.Unmarshal(raw, &r) == nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Agent < out[j].Agent })
	return out, nil
}

func countLines(b []byte) int {
	if len(b) == 0 {
		return 0
	}
	n := bytes.Count(b, []byte("\n"))
	if b[len(b)-1] != '\n' {
		n++
	}
	return n
}

func (h *handlers) env(ctx context.Context, runID string, st *checkoutState, m *diffmap.Map, stage, selfAgent string) schema.Env {
	g := h.git(st.Repo)
	src := source{g: g, head: st.HeadSHA, base: st.MergeBase}
	cache := map[string]int{}
	upstream := map[string]bool{}
	if reports, err := h.acceptedReports(runID, stage, st.HeadSHA); err == nil {
		for _, r := range reports {
			if r.Agent == selfAgent {
				continue
			}
			for _, c := range r.Claims {
				upstream[r.Agent+":"+c.ID] = true
			}
		}
	}
	return schema.Env{
		HeadSHA: st.HeadSHA,
		Map:     m,
		LineCount: func(side, p string) (int, error) {
			key := side + "\x00" + p
			if n, ok := cache[key]; ok {
				return n, nil
			}
			var b []byte
			var err error
			if side == "old" {
				b, err = src.Base(ctx, p)
			} else {
				b, err = src.Head(ctx, p)
			}
			if err != nil {
				return 0, err
			}
			cache[key] = countLines(b)
			return cache[key], nil
		},
		ArtifactExists: func(p string) bool {
			_, err := h.cfg.Store.Get(runID, p)
			return err == nil
		},
		Metric:         h.metricLookup(runID),
		IssueText:      h.issueText(runID),
		UpstreamClaims: upstream,
	}
}

type SubmitIn struct {
	RunID    string `json:"run_id"`
	Stage    string `json:"stage,omitempty" jsonschema:"creation, review (default) or augmentation"`
	Token    string `json:"token,omitempty" jsonschema:"submit token from your orchestrator (report_slot); required"`
	Report   string `json:"report" jsonschema:"the report as a JSON string (schema: report_schema)"`
	Markdown string `json:"markdown,omitempty" jsonschema:"optional human-readable rendering (e.g. the summary panel), stored next to the report"`
}

type SubmitOut struct {
	Path      string           `json:"path"`
	Kept      int              `json:"kept"`
	Dropped   []schema.Dropped `json:"dropped"`
	Notes     []string         `json:"notes"`
	Accepted  bool             `json:"accepted"`
	Rejection string           `json:"rejection,omitempty" jsonschema:"why the whole report was rejected; fix and resubmit"`
}

func (h *handlers) reportSubmit(ctx context.Context, _ *mcp.CallToolRequest, in SubmitIn) (*mcp.CallToolResult, SubmitOut, error) {
	stage, err := stageOf(in.Stage)
	if err != nil {
		return nil, SubmitOut{}, err
	}
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, SubmitOut{}, err
	}
	var peek struct {
		Agent string `json:"agent"`
	}
	_ = json.Unmarshal([]byte(in.Report), &peek)
	agent := strings.Trim(agentRE.ReplaceAllString(strings.ToLower(peek.Agent), "-"), "-.")
	var env schema.Env
	if st.Mode == "changeset" {
		// A set-level report cites code in member runs (SPEC §21).
		if env, err = h.changesetEnv(ctx, in.RunID, st, stage, peek.Agent); err != nil {
			return nil, SubmitOut{}, err
		}
	} else {
		m, err := h.loadMap(in.RunID)
		if err != nil {
			return nil, SubmitOut{}, err
		}
		env = h.env(ctx, in.RunID, st, m, stage, peek.Agent)
	}
	res, err := schema.Validate([]byte(in.Report), env)
	if errors.Is(err, schema.ErrInvalid) {
		// Returned as a normal result so the agent can read it and retry.
		return nil, SubmitOut{Accepted: false, Rejection: err.Error(), Dropped: []schema.Dropped{}, Notes: []string{}}, nil
	}
	if err != nil {
		return nil, SubmitOut{}, err
	}
	if agent == "" {
		return nil, SubmitOut{Accepted: false, Rejection: "agent name is empty", Dropped: []schema.Dropped{}, Notes: []string{}}, nil
	}
	if !h.checkSlot(in.RunID, stage, st.HeadSHA, agent, in.Token) {
		return nil, SubmitOut{Accepted: false, Rejection: fmt.Sprintf("no report slot for agent %q with this token; "+
			"use exactly the agent_name and token your orchestrator gave you", agent), Dropped: []schema.Dropped{}, Notes: []string{}}, nil
	}
	base := stage + "/" + st.HeadSHA + "/" + agent
	data, _ := json.MarshalIndent(res.Report, "", "  ")
	p, err := h.cfg.Store.Put(in.RunID, base+".json", data)
	if err != nil {
		return nil, SubmitOut{}, err
	}
	dropped, _ := json.MarshalIndent(res, "", "  ")
	if _, err := h.cfg.Store.Put(in.RunID, base+".dropped.json", dropped); err != nil {
		return nil, SubmitOut{}, err
	}
	if in.Markdown != "" {
		if _, err := h.cfg.Store.Put(in.RunID, base+".md", []byte(in.Markdown)); err != nil {
			return nil, SubmitOut{}, err
		}
	}
	return nil, SubmitOut{Path: p, Kept: len(res.Report.Claims), Dropped: res.Dropped, Notes: res.Notes, Accepted: true}, nil
}

type ReportsIn struct {
	RunID string `json:"run_id"`
	Stage string `json:"stage,omitempty"`
}

type ReportsOut struct {
	HeadSHA string          `json:"head_sha"`
	Reports []schema.Report `json:"reports"`
}

func (h *handlers) reportsGet(_ context.Context, _ *mcp.CallToolRequest, in ReportsIn) (*mcp.CallToolResult, ReportsOut, error) {
	stage, err := stageOf(in.Stage)
	if err != nil {
		return nil, ReportsOut{}, err
	}
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, ReportsOut{}, err
	}
	rs, err := h.acceptedReports(in.RunID, stage, st.HeadSHA)
	if rs == nil {
		rs = []schema.Report{}
	}
	return nil, ReportsOut{HeadSHA: st.HeadSHA, Reports: rs}, err
}

type EvidenceIn struct {
	RunID    string            `json:"run_id"`
	Evidence []schema.Evidence `json:"evidence"`
}

type EvidenceResult struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

type EvidenceOut struct {
	Results []EvidenceResult `json:"results"`
}

func (h *handlers) evidenceCheck(ctx context.Context, _ *mcp.CallToolRequest, in EvidenceIn) (*mcp.CallToolResult, EvidenceOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, EvidenceOut{}, err
	}
	m, err := h.loadMap(in.RunID)
	if err != nil {
		return nil, EvidenceOut{}, err
	}
	env := h.env(ctx, in.RunID, st, m, "review", "")
	out := EvidenceOut{Results: make([]EvidenceResult, 0, len(in.Evidence))}
	for _, e := range in.Evidence {
		// Reuse the validator on a one-claim report.
		rep := schema.Report{Agent: "evidence-check", Version: 1, HeadSHA: st.HeadSHA, Status: "ok", Summary: "-",
			Claims: []schema.Claim{{ID: "e", Statement: "-", Category: "defect", Severity: "info", Confidence: "low", Evidence: []schema.Evidence{e}}}}
		raw, _ := json.Marshal(rep)
		res, err := schema.Validate(raw, env)
		switch {
		case err != nil:
			out.Results = append(out.Results, EvidenceResult{OK: false, Reason: err.Error()})
		case len(res.Dropped) > 0:
			out.Results = append(out.Results, EvidenceResult{OK: false, Reason: strings.Join(res.Dropped[0].Reasons, "; ")})
		default:
			out.Results = append(out.Results, EvidenceResult{OK: true})
		}
	}
	return nil, out, nil
}

type SchemaOut struct {
	Schema map[string]any `json:"schema"`
}

func (h *handlers) reportSchema(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, SchemaOut, error) {
	var m map[string]any
	err := json.Unmarshal(schema.ReportSchemaJSON(), &m)
	return nil, SchemaOut{Schema: m}, err
}

// Synthesisers restate other agents' claims; finding views skip them to
// avoid double counting.
var synthesisers = map[string]bool{"summary-generator": true, "findings-digest": true}

// findingReports returns accepted review and augmentation reports for a head,
// excluding synthesisers.
func (h *handlers) findingReports(runID, head string) []schema.Report {
	var out []schema.Report
	for _, stage := range []string{"augmentation", "review"} {
		rs, _ := h.acceptedReports(runID, stage, head)
		for _, r := range rs {
			if !synthesisers[r.Agent] {
				out = append(out, r)
			}
		}
	}
	return out
}

func (h *handlers) issueText(runID string) func(string) (string, bool) {
	is, ok := h.loadIssues(runID)
	if !ok {
		return nil
	}
	by := map[string]string{}
	for _, i := range is {
		by[strings.ToLower(i.Ref)] = i.Title + "\n" + i.Body
	}
	return func(ref string) (string, bool) {
		t, ok := by[strings.ToLower(ref)]
		return t, ok
	}
}

// riskOrder re-orders units by file risk (repo_metrics) within the same
// review-priority group, once risk has been computed (SPEC §7).
func (h *handlers) riskOrder(runID string, m *diffmap.Map) ([]string, bool) {
	raw, err := h.cfg.Store.Get(runID, "repo_metrics.json")
	if err != nil {
		return nil, false
	}
	var rm RepoMetricsOut
	if json.Unmarshal(raw, &rm) != nil {
		return nil, false
	}
	risk := map[string]int{}
	for _, f := range rm.Risk.Files {
		risk[f.Path] = f.Score
	}
	pos := map[string]int{}
	for i, id := range m.Order {
		pos[id] = i
	}
	order := append([]string{}, m.Order...)
	group := func(u *diffmap.Unit) int {
		switch {
		case u.Kind == diffmap.KindBinary || u.Kind == diffmap.KindFile:
			return 4
		case u.Kind == diffmap.KindDoc:
			return 3
		case u.Kind == diffmap.KindConfig:
			return 2
		case u.IsTest:
			return 1
		}
		return 0
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, _ := m.Unit(order[i])
		b, _ := m.Unit(order[j])
		if ga, gb := group(a), group(b); ga != gb {
			return ga < gb
		}
		if ra, rb := risk[a.File], risk[b.File]; ra != rb {
			return ra > rb
		}
		return pos[order[i]] < pos[order[j]]
	})
	return order, true
}
