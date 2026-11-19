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

// M1 metric sources: tools whose outputs may be cited as metric evidence.
var metricSources = map[string]bool{"diffmap": true, "pr_diff": true}

func registerReviewTools(s *mcp.Server, h *handlers) {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	local := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: true}

	mcp.AddTool(s, &mcp.Tool{Name: "pr_checkout", Annotations: local,
		Description: "Fetch the PR/MR head into a cached local mirror and check it out as a detached worktree under " +
			".aireviewer/worktrees/<run_id> so its code can be read and searched. Computes the merge base. Never pushes or writes to the forge."},
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
	mcp.AddTool(s, &mcp.Tool{Name: "report_submit", Annotations: local,
		Description: "Submit an agent report (JSON string conforming to the aireviewer report schema v1). It is validated: schema errors reject " +
			"the whole report; claims whose evidence cites non-existent files/lines, metrics not produced by a tool, or missing artifacts are dropped. " +
			"Returns what was kept and why anything was dropped so you can fix and resubmit. Saves to the local run context only."},
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
	gitrepo.Checkout
}

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
	Worktree   string `json:"worktree" jsonschema:"absolute path of the PR head checkout; read and grep code here"`
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
	wt := filepath.Join(h.cfg.RepoRoot, ".aireviewer", "worktrees", r.RunID)
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

type source struct {
	g        *gitrepo.Git
	worktree string
	base     string
}

func (s source) Head(_ context.Context, p string) ([]byte, error) {
	if !filepath.IsLocal(p) {
		return nil, fmt.Errorf("path %q escapes the worktree", p)
	}
	return os.ReadFile(filepath.Join(s.worktree, filepath.FromSlash(p))) //nolint:gosec // confined by IsLocal
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
	m, err := diffmap.Build(ctx, diffmap.ParseGitDiff(out), source{g: g, worktree: st.Worktree, base: st.MergeBase}, st.HeadSHA, st.MergeBase)
	if err != nil {
		return nil, DiffmapOut{}, err
	}
	data, _ := json.Marshal(m)
	if _, err := h.cfg.Store.Put(in.RunID, "diffmap.json", data); err != nil {
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
	reports, _ := h.acceptedReports(in.RunID, "review", m.HeadSHA)
	for _, r := range reports {
		for _, c := range r.Claims {
			for _, a := range c.Anchors {
				counts[a]++
			}
		}
	}
	return nil, DiffmapOut{RunID: in.RunID, Map: m.Summary(), FindingCounts: counts}, nil
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
	Unit     diffmap.Unit    `json:"unit"`
	Rendered string          `json:"rendered" jsonschema:"numbered diff: old-line new-line op text"`
	Findings []AnchoredClaim `json:"findings"`
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
	reports, _ := h.acceptedReports(in.RunID, "review", m.HeadSHA)
	for _, r := range reports {
		for _, c := range r.Claims {
			for _, a := range c.Anchors {
				if a == in.UnitID {
					out.Findings = append(out.Findings, AnchoredClaim{Agent: r.Agent, Claim: c})
					break
				}
			}
		}
	}
	return nil, out, nil
}

// ---- reports ----

var stageRE = regexp.MustCompile(`^(creation|review|augmentation)$`)

func stageOf(s string) (string, error) {
	if s == "" {
		return "review", nil
	}
	if !stageRE.MatchString(s) {
		return "", fmt.Errorf("stage must be creation, review or augmentation")
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
		if !strings.HasPrefix(p, prefix) || strings.HasSuffix(p, ".dropped.json") || path.Dir(p) != strings.TrimSuffix(prefix, "/") {
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
	src := source{g: g, worktree: st.Worktree, base: st.MergeBase}
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
		MetricSources:  metricSources,
		UpstreamClaims: upstream,
	}
}

type SubmitIn struct {
	RunID  string `json:"run_id"`
	Stage  string `json:"stage,omitempty" jsonschema:"creation, review (default) or augmentation"`
	Report string `json:"report" jsonschema:"the report as a JSON string (schema: report_schema)"`
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
	m, err := h.loadMap(in.RunID)
	if err != nil {
		return nil, SubmitOut{}, err
	}
	var peek struct {
		Agent string `json:"agent"`
	}
	_ = json.Unmarshal([]byte(in.Report), &peek)
	agent := strings.Trim(agentRE.ReplaceAllString(strings.ToLower(peek.Agent), "-"), "-.")
	res, err := schema.Validate([]byte(in.Report), h.env(ctx, in.RunID, st, m, stage, peek.Agent))
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
