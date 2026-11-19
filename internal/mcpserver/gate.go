// SPDX-License-Identifier: MIT

package mcpserver

// M3: the human gate. Comment drafts, quality checks, disputes and fixes are
// local state; every forge write goes through a frozen proposal
// (review_prepare / reviewers_prepare / sticky_prepare) that a human must
// confirm via proposal_submit. Confirmation is asked by the server itself
// through MCP elicitation when the client supports it, so the model cannot
// answer on the user's behalf; otherwise it relies on the client's tool
// permission prompt (or refuses, with AIREVIEWER_STRICT_GATE=1). Every step is
// appended to a hash-chained decision log (decisions.jsonl).

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/husobee/aireviewer/internal/config"
	"github.com/husobee/aireviewer/internal/decisionlog"
	"github.com/husobee/aireviewer/internal/diffmap"
	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/schema"
)

func registerGateTools(s *mcp.Server, h *handlers) {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	local := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: false}
	write := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: false, OpenWorldHint: boolPtr(true)}

	mcp.AddTool(s, &mcp.Tool{Name: "whoami", Annotations: ro,
		Description: "Return the authenticated forge user and whether they authored this PR (an author cannot approve or request changes on their own PR)."},
		h.whoami)
	mcp.AddTool(s, &mcp.Tool{Name: "comment_draft_add", Annotations: local,
		Description: "Add a draft inline comment (local only; nothing is posted). The line must be in the PR diff. Link it to a finding with from_finding."},
		h.draftAdd)
	mcp.AddTool(s, &mcp.Tool{Name: "comment_draft_update", Annotations: local,
		Description: "Replace a draft comment's body. Quality checks must be re-run afterwards."},
		h.draftUpdate)
	mcp.AddTool(s, &mcp.Tool{Name: "comment_draft_remove", Annotations: local,
		Description: "Remove a draft comment."},
		h.draftRemove)
	mcp.AddTool(s, &mcp.Tool{Name: "comment_drafts_list", Annotations: ro,
		Description: "List draft comments with their rendered bodies, quality-check status, circuit-breaker status and attached fixes."},
		h.draftsList)
	mcp.AddTool(s, &mcp.Tool{Name: "comment_check_record", Annotations: local,
		Description: "Orchestrator: record a toxicity or usefulness verdict (from the comment-toxicity / comment-usefulness agents) for a draft's current body."},
		h.checkRecord)
	mcp.AddTool(s, &mcp.Tool{Name: "finding_dispute", Annotations: local,
		Description: "Record that the reviewer disputes a finding. Disputed findings are held by the circuit breaker: they cannot back a posted comment without a logged override."},
		h.findingDispute)
	mcp.AddTool(s, &mcp.Tool{Name: "fix_submit", Annotations: local,
		Description: "fix-suggester agents: submit a unified-diff patch for one finding, with what it modifies and what it leaves intact. " +
			"Requires a report_slot token. The patch must apply cleanly to the PR head and touch one contiguous range of one changed file."},
		h.fixSubmit)
	mcp.AddTool(s, &mcp.Tool{Name: "fixes_list", Annotations: ro,
		Description: "List submitted fix suggestions with their patches and suggestion ranges."},
		h.fixesList)
	mcp.AddTool(s, &mcp.Tool{Name: "comment_attach_fix", Annotations: local,
		Description: "After the reviewer approves a fix, attach it to a draft comment as a forge suggestion block (the comment's range becomes the fix's range)."},
		h.attachFix)
	mcp.AddTool(s, &mcp.Tool{Name: "review_mode_set", Annotations: local,
		Description: "Turn reviewer-first mode on or off: AI findings stay hidden until the reviewer has viewed every reviewable unit (paper §5.1.8, automation bias)."},
		h.modeSet)
	mcp.AddTool(s, &mcp.Tool{Name: "review_prepare", Annotations: local,
		Description: "Freeze a batched review (verdict chosen by the human, body, draft comments) into a proposal after running every gate check. " +
			"Returns the exact preview and a hash. Nothing is posted until proposal_submit."},
		h.reviewPrepare)
	mcp.AddTool(s, &mcp.Tool{Name: "reviewers_prepare", Annotations: local,
		Description: "Freeze a reviewer request into a proposal (preview + hash). Nothing is requested until proposal_submit."},
		h.reviewersPrepare)
	mcp.AddTool(s, &mcp.Tool{Name: "sticky_prepare", Annotations: local,
		Description: "Freeze the augmentation summary as a sticky PR comment proposal (with machine-readable reports for re-use). Nothing is posted until proposal_submit."},
		h.stickyPrepare)
	mcp.AddTool(s, &mcp.Tool{Name: "proposal_submit", Annotations: write,
		Description: "Post a frozen proposal to the forge. Requires the proposal hash. The server asks the human to confirm the exact content " +
			"(MCP elicitation) when the client supports it. Every outcome is written to the decision log."},
		h.proposalSubmit)
	mcp.AddTool(s, &mcp.Tool{Name: "augmentation_import", Annotations: local,
		Description: "Re-use augmentation reports from a sticky PR comment for the current head: only from the authenticated user or configured bots, " +
			"and every report is re-validated against the local checkout before it is accepted."},
		h.augmentationImport)
	mcp.AddTool(s, &mcp.Tool{Name: "decisions_get", Annotations: ro,
		Description: "Return the run's decision log and verify its hash chain."},
		h.decisionsGet)
}

// ---- state helpers ----

func (h *handlers) headDir(st *checkoutState) string { return "review/" + st.HeadSHA + "/" }

func (h *handlers) loadJSON(runID, rel string, v any) error {
	raw, err := h.cfg.Store.Get(runID, rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

func (h *handlers) saveJSON(runID, rel string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	_, err = h.cfg.Store.Put(runID, rel, data)
	return err
}

// logEvent appends to the run's hash-chained decision log and returns the new head hash.
func (h *handlers) logEvent(runID, event, actor string, data map[string]any) (string, error) {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	cur, err := h.cfg.Store.Get(runID, "decisions.jsonl")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	next, e, err := decisionlog.Append(cur, h.now(), event, actor, data)
	if err != nil {
		return "", err
	}
	if _, err := h.cfg.Store.Put(runID, "decisions.jsonl", next); err != nil {
		return "", err
	}
	return e.Hash, nil
}

func (h *handlers) logHead(runID string) string {
	cur, _ := h.cfg.Store.Get(runID, "decisions.jsonl")
	es, _ := decisionlog.Parse(cur)
	if len(es) == 0 {
		return decisionlog.Genesis
	}
	return es[len(es)-1].Hash
}

func (h *handlers) now() time.Time {
	if h.cfg.Now != nil {
		return h.cfg.Now()
	}
	return time.Now()
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// ---- whoami ----

type WhoamiOut struct {
	User       string `json:"user"`
	PRAuthor   string `json:"pr_author"`
	IsAuthor   bool   `json:"is_author"`
	CanApprove bool   `json:"can_approve" jsonschema:"false when the user authored the PR"`
	Note       string `json:"note,omitempty"`
}

func (h *handlers) currentUser(ctx context.Context, repo forge.Repo) (string, error) {
	p, err := h.cfg.Providers.Get(ctx, repo)
	if err != nil {
		return "", err
	}
	id, ok := p.(forge.Identity)
	if !ok {
		return "", errors.New("this forge adapter cannot identify the current user")
	}
	return id.CurrentUser(ctx, repo)
}

func (h *handlers) whoami(ctx context.Context, _ *mcp.CallToolRequest, in RunIn) (*mcp.CallToolResult, WhoamiOut, error) {
	pr, err := h.loadPR(in.RunID)
	if err != nil {
		return nil, WhoamiOut{}, err
	}
	out := WhoamiOut{PRAuthor: pr.Author, CanApprove: true}
	u, err := h.currentUser(ctx, pr.Ref.Repo)
	if err != nil {
		out.Note = "could not identify the current user (" + err.Error() + "); posting requires an authenticated token"
		return nil, out, nil
	}
	out.User = u
	out.IsAuthor = strings.EqualFold(u, pr.Author)
	out.CanApprove = !out.IsAuthor
	if out.IsAuthor {
		out.Note = "You authored this " + pr.Ref.Forge.Abbrev() + ": the forge will not accept your approval or change request. Comment-only reviews are possible."
	}
	return nil, out, nil
}

// ---- drafts ----

type Check struct {
	Verdict  string `json:"verdict" jsonschema:"pass | warn | fail"`
	Reason   string `json:"reason,omitempty"`
	Rewrite  string `json:"rewrite,omitempty"`
	BodyHash string `json:"body_hash"`
}

type Draft struct {
	ID                string           `json:"id"`
	Path              string           `json:"path"`
	OldPath           string           `json:"old_path,omitempty"`
	Side              string           `json:"side"`
	Line              int              `json:"line"`
	StartLine         int              `json:"start_line,omitempty"`
	ContextOldLine    int              `json:"context_old_line,omitempty"`
	Body              string           `json:"body"`
	BodyHash          string           `json:"body_hash"`
	FromFinding       string           `json:"from_finding,omitempty"`
	FindingConfidence string           `json:"finding_confidence,omitempty"`
	FixID             string           `json:"fix_id,omitempty"`
	Checks            map[string]Check `json:"checks"`
}

type draftFile struct {
	Next   int     `json:"next"`
	Drafts []Draft `json:"drafts"`
}

type DraftAddIn struct {
	RunID       string `json:"run_id"`
	Path        string `json:"path"`
	Line        int    `json:"line" jsonschema:"line to comment on (last line of a range)"`
	StartLine   int    `json:"start_line,omitempty"`
	Side        string `json:"side,omitempty" jsonschema:"new (head, default) or old (merge base)"`
	Body        string `json:"body"`
	FromFinding string `json:"from_finding,omitempty" jsonschema:"qualified finding id this comment comes from, e.g. code-reviewer:F2"`
}

type DraftOut struct {
	Draft Draft `json:"draft"`
}

// diffLine finds the diff line for (path, side, n) in the diff-map.
func diffLine(m *diffmap.Map, path, side string, n int) (*diffmap.Line, *diffmap.Unit) {
	for i := range m.Units {
		u := &m.Units[i]
		if u.File != path && u.OldFile != path {
			continue
		}
		for j := range u.Lines {
			l := &u.Lines[j]
			if side == "old" && l.Op != "+" && l.Old == n {
				return l, u
			}
			if side != "old" && l.Op != "-" && l.New == n {
				return l, u
			}
		}
	}
	return nil, nil
}

func validateRange(m *diffmap.Map, path, side string, start, end int) (ctxOld int, oldPath string, err error) {
	if end < 1 || (start > 0 && start > end) {
		return 0, "", fmt.Errorf("invalid line range %d-%d", start, end)
	}
	if start == 0 {
		start = end
	}
	var unit *diffmap.Unit
	for n := start; n <= end; n++ {
		l, u := diffLine(m, path, side, n)
		if l == nil {
			return 0, "", fmt.Errorf("%s:%d (%s side) is not part of the PR diff; forges only accept inline comments on diff lines", path, n, side)
		}
		if unit != nil && u != unit {
			return 0, "", fmt.Errorf("lines %d-%d span more than one diff unit; comment on a smaller range", start, end)
		}
		unit = u
		if n == end && side != "old" && l.Op == " " {
			ctxOld = l.Old
		}
	}
	return ctxOld, unit.OldFile, nil
}

func (h *handlers) resolveFinding(runID, head, ref string) (string, schema.Claim, bool) {
	agent, id, ok := strings.Cut(ref, ":")
	if !ok {
		return "", schema.Claim{}, false
	}
	for _, r := range h.findingReports(runID, head) {
		if r.Agent != agent {
			continue
		}
		for _, c := range r.Claims {
			if c.ID == id {
				return r.Agent + ":" + c.ID, c, true
			}
		}
	}
	return "", schema.Claim{}, false
}

func (h *handlers) draftAdd(_ context.Context, _ *mcp.CallToolRequest, in DraftAddIn) (*mcp.CallToolResult, DraftOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, DraftOut{}, err
	}
	m, err := h.loadMap(in.RunID)
	if err != nil {
		return nil, DraftOut{}, err
	}
	if strings.TrimSpace(in.Body) == "" {
		return nil, DraftOut{}, errors.New("comment body is empty")
	}
	side := in.Side
	if side == "" {
		side = "new"
	}
	ctxOld, oldPath, err := validateRange(m, in.Path, side, in.StartLine, in.Line)
	if err != nil {
		return nil, DraftOut{}, err
	}
	d := Draft{Path: in.Path, OldPath: oldPath, Side: side, Line: in.Line, StartLine: in.StartLine, ContextOldLine: ctxOld,
		Body: in.Body, BodyHash: sha(in.Body), Checks: map[string]Check{}}
	if in.FromFinding != "" {
		ref, c, ok := h.resolveFinding(in.RunID, st.HeadSHA, in.FromFinding)
		if !ok {
			return nil, DraftOut{}, fmt.Errorf("unknown finding %q (use the qualified id, e.g. code-reviewer:F2)", in.FromFinding)
		}
		d.FromFinding, d.FindingConfidence = ref, c.Confidence
	}
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	var df draftFile
	if err := h.loadJSON(in.RunID, h.headDir(st)+"drafts.json", &df); err != nil {
		return nil, DraftOut{}, err
	}
	df.Next++
	d.ID = fmt.Sprintf("C%d", df.Next)
	df.Drafts = append(df.Drafts, d)
	if err := h.saveJSON(in.RunID, h.headDir(st)+"drafts.json", df); err != nil {
		return nil, DraftOut{}, err
	}
	_, _ = h.logEvent(in.RunID, "comment_drafted", "orchestrator", map[string]any{"id": d.ID, "path": d.Path, "line": d.Line, "from_finding": d.FromFinding, "body_hash": d.BodyHash})
	return nil, DraftOut{Draft: d}, nil
}

type DraftUpdateIn struct {
	RunID string `json:"run_id"`
	ID    string `json:"id"`
	Body  string `json:"body"`
}

func (h *handlers) mutateDraft(runID, id string, fn func(df *draftFile, i int) error) (Draft, error) {
	st, err := h.loadCheckout(runID)
	if err != nil {
		return Draft{}, err
	}
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	var df draftFile
	if err := h.loadJSON(runID, h.headDir(st)+"drafts.json", &df); err != nil {
		return Draft{}, err
	}
	for i := range df.Drafts {
		if df.Drafts[i].ID == id {
			if err := fn(&df, i); err != nil {
				return Draft{}, err
			}
			var out Draft
			if i < len(df.Drafts) && df.Drafts[i].ID == id {
				out = df.Drafts[i]
			}
			return out, h.saveJSON(runID, h.headDir(st)+"drafts.json", df)
		}
	}
	return Draft{}, fmt.Errorf("no draft %q for the current head", id)
}

func (h *handlers) draftUpdate(_ context.Context, _ *mcp.CallToolRequest, in DraftUpdateIn) (*mcp.CallToolResult, DraftOut, error) {
	if strings.TrimSpace(in.Body) == "" {
		return nil, DraftOut{}, errors.New("comment body is empty")
	}
	d, err := h.mutateDraft(in.RunID, in.ID, func(df *draftFile, i int) error {
		df.Drafts[i].Body, df.Drafts[i].BodyHash = in.Body, sha(in.Body)
		return nil
	})
	if err == nil {
		_, _ = h.logEvent(in.RunID, "comment_edited", "orchestrator", map[string]any{"id": in.ID, "body_hash": d.BodyHash})
	}
	return nil, DraftOut{Draft: d}, err
}

type IDIn struct {
	RunID string `json:"run_id"`
	ID    string `json:"id"`
}

type OKOut struct {
	OK bool `json:"ok"`
}

func (h *handlers) draftRemove(_ context.Context, _ *mcp.CallToolRequest, in IDIn) (*mcp.CallToolResult, OKOut, error) {
	_, err := h.mutateDraft(in.RunID, in.ID, func(df *draftFile, i int) error {
		df.Drafts = append(df.Drafts[:i], df.Drafts[i+1:]...)
		return nil
	})
	if err == nil {
		_, _ = h.logEvent(in.RunID, "comment_removed", "orchestrator", map[string]any{"id": in.ID})
	}
	return nil, OKOut{OK: err == nil}, err
}

type CheckRecordIn struct {
	RunID   string `json:"run_id"`
	ID      string `json:"id"`
	Check   string `json:"check" jsonschema:"toxicity or usefulness"`
	Verdict string `json:"verdict" jsonschema:"pass, warn or fail"`
	Reason  string `json:"reason,omitempty"`
	Rewrite string `json:"rewrite,omitempty"`
}

func (h *handlers) checkRecord(_ context.Context, _ *mcp.CallToolRequest, in CheckRecordIn) (*mcp.CallToolResult, DraftOut, error) {
	if in.Check != "toxicity" && in.Check != "usefulness" {
		return nil, DraftOut{}, errors.New("check must be toxicity or usefulness")
	}
	if in.Verdict != "pass" && in.Verdict != "warn" && in.Verdict != "fail" {
		return nil, DraftOut{}, errors.New("verdict must be pass, warn or fail")
	}
	d, err := h.mutateDraft(in.RunID, in.ID, func(df *draftFile, i int) error {
		df.Drafts[i].Checks[in.Check] = Check{Verdict: in.Verdict, Reason: in.Reason, Rewrite: in.Rewrite, BodyHash: df.Drafts[i].BodyHash}
		return nil
	})
	if err == nil {
		_, _ = h.logEvent(in.RunID, "comment_checked", "agent", map[string]any{"id": in.ID, "check": in.Check, "verdict": in.Verdict, "body_hash": d.BodyHash})
	}
	return nil, DraftOut{Draft: d}, err
}

type DraftView struct {
	Draft
	Rendered string   `json:"rendered" jsonschema:"exact text that would be posted for this comment"`
	Status   []string `json:"status" jsonschema:"what still stands between this draft and posting"`
}

type DraftsOut struct {
	Drafts []DraftView `json:"drafts"`
}

func (h *handlers) draftsList(ctx context.Context, _ *mcp.CallToolRequest, in RunIn) (*mcp.CallToolResult, DraftsOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, DraftsOut{}, err
	}
	cfg, _ := h.loadConfig(ctx, st)
	var df draftFile
	if err := h.loadJSON(in.RunID, h.headDir(st)+"drafts.json", &df); err != nil {
		return nil, DraftsOut{}, err
	}
	var ff fixFile
	_ = h.loadJSON(in.RunID, h.headDir(st)+"fixes.json", &ff)
	disputes := h.disputes(in.RunID, st)
	out := DraftsOut{Drafts: []DraftView{}}
	for _, d := range df.Drafts {
		var fx *Fix
		for i := range ff.Fixes {
			if ff.Fixes[i].ID == d.FixID {
				fx = &ff.Fixes[i]
			}
		}
		v := DraftView{Draft: d, Rendered: renderComment(st.Repo.Forge, d, fx, cfg), Status: []string{}}
		for _, check := range []struct{ name, mode string }{{"toxicity", cfg.Comments.Toxicity}, {"usefulness", cfg.Comments.Usefulness}} {
			if check.mode == "off" {
				continue
			}
			c, ok := d.Checks[check.name]
			switch {
			case !ok || c.BodyHash != d.BodyHash:
				v.Status = append(v.Status, check.name+" check needed")
			case c.Verdict == "fail" && check.mode == "block":
				v.Status = append(v.Status, check.name+" failed (blocks posting): "+c.Reason)
			case c.Verdict != "pass":
				v.Status = append(v.Status, check.name+" "+c.Verdict+": "+c.Reason)
			}
		}
		if cfg.Breaker() && d.FromFinding != "" {
			if d.FindingConfidence == "low" {
				v.Status = append(v.Status, "circuit breaker: low-confidence finding (needs an override reason)")
			}
			if r, ok := disputes[d.FromFinding]; ok {
				v.Status = append(v.Status, "circuit breaker: finding disputed ("+r+")")
			}
		}
		out.Drafts = append(out.Drafts, v)
	}
	return nil, out, nil
}

// ---- disputes ----

type DisputeIn struct {
	RunID   string `json:"run_id"`
	Finding string `json:"finding" jsonschema:"qualified finding id, e.g. risk-analyst:R2"`
	Reason  string `json:"reason"`
}

func (h *handlers) disputes(runID string, st *checkoutState) map[string]string {
	d := map[string]string{}
	_ = h.loadJSON(runID, h.headDir(st)+"disputes.json", &d)
	return d
}

func (h *handlers) findingDispute(_ context.Context, _ *mcp.CallToolRequest, in DisputeIn) (*mcp.CallToolResult, OKOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, OKOut{}, err
	}
	ref, _, ok := h.resolveFinding(in.RunID, st.HeadSHA, in.Finding)
	if !ok {
		return nil, OKOut{}, fmt.Errorf("unknown finding %q", in.Finding)
	}
	if strings.TrimSpace(in.Reason) == "" {
		return nil, OKOut{}, errors.New("a dispute needs a reason")
	}
	h.stateMu.Lock()
	d := h.disputes(in.RunID, st)
	d[ref] = in.Reason
	err = h.saveJSON(in.RunID, h.headDir(st)+"disputes.json", d)
	h.stateMu.Unlock()
	if err == nil {
		_, _ = h.logEvent(in.RunID, "finding_disputed", "orchestrator", map[string]any{"finding": ref, "reason": in.Reason})
	}
	return nil, OKOut{OK: err == nil}, err
}

// ---- fixes ----

type Fix struct {
	ID           string `json:"id"`
	Finding      string `json:"finding"`
	Agent        string `json:"agent"`
	Patch        string `json:"patch"`
	Path         string `json:"path"`
	Start        int    `json:"start" jsonschema:"first head line replaced"`
	End          int    `json:"end" jsonschema:"last head line replaced"`
	Replacement  string `json:"replacement"`
	Modifies     string `json:"modifies"`
	LeavesIntact string `json:"leaves_intact"`
	Approved     bool   `json:"approved"`
}

type fixFile struct {
	Next  int   `json:"next"`
	Fixes []Fix `json:"fixes"`
}

type FixSubmitIn struct {
	RunID        string `json:"run_id"`
	AgentName    string `json:"agent_name"`
	Token        string `json:"token,omitempty"`
	Finding      string `json:"finding" jsonschema:"qualified finding id the fix resolves"`
	Patch        string `json:"patch" jsonschema:"unified diff against the PR head (one file, one hunk)"`
	Modifies     string `json:"modifies" jsonschema:"exactly what the patch changes"`
	LeavesIntact string `json:"leaves_intact" jsonschema:"what structural logic it deliberately leaves unchanged"`
}

type FixOut struct {
	Fix Fix `json:"fix"`
}

// normalisePatch makes an agent-written patch parseable by ParseGitDiff.
func normalisePatch(p string) string {
	p = strings.ReplaceAll(p, "\r\n", "\n")
	if !strings.HasPrefix(strings.TrimSpace(p), "diff --git ") {
		var a, b string
		for _, l := range strings.Split(p, "\n") {
			if strings.HasPrefix(l, "--- ") && a == "" {
				a = strings.TrimSpace(l[4:])
			} else if strings.HasPrefix(l, "+++ ") && b == "" {
				b = strings.TrimSpace(l[4:])
			}
		}
		p = "diff --git " + a + " " + b + "\n" + strings.TrimLeft(p, "\n")
	}
	if !strings.HasSuffix(p, "\n") {
		p += "\n"
	}
	return p
}

func (h *handlers) fixSubmit(ctx context.Context, _ *mcp.CallToolRequest, in FixSubmitIn) (*mcp.CallToolResult, FixOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, FixOut{}, err
	}
	agent := strings.Trim(agentRE.ReplaceAllString(strings.ToLower(in.AgentName), "-"), "-.")
	if !h.checkSlot(in.RunID, "review", st.HeadSHA, agent, in.Token) {
		return nil, FixOut{}, fmt.Errorf("no report slot for agent %q with this token", agent)
	}
	ref, _, ok := h.resolveFinding(in.RunID, st.HeadSHA, in.Finding)
	if !ok {
		return nil, FixOut{}, fmt.Errorf("unknown finding %q", in.Finding)
	}
	if strings.TrimSpace(in.Modifies) == "" || strings.TrimSpace(in.LeavesIntact) == "" {
		return nil, FixOut{}, errors.New("state what the patch modifies and what it leaves intact (paper §4.2.4)")
	}
	patch := normalisePatch(in.Patch)
	files := diffmap.ParseGitDiff(patch)
	if len(files) != 1 || len(files[0].Hunks) != 1 {
		return nil, FixOut{}, errors.New("a fix must change exactly one file in exactly one hunk, so it can become a forge suggestion")
	}
	f := files[0]
	m, err := h.loadMap(in.RunID)
	if err != nil {
		return nil, FixOut{}, err
	}
	if !m.HasFile(f.NewPath) {
		return nil, FixOut{}, fmt.Errorf("%s is not changed by this PR; suggestions can only target the PR diff", f.NewPath)
	}
	if err := h.git(st.Repo).ApplyCheck(ctx, st.Worktree, patch); err != nil {
		return nil, FixOut{}, err
	}
	hk := f.Hunks[0]
	var start, end int
	var repl []string
	for _, l := range hk.Lines {
		if l.Op != "+" { // context or deleted: a head line being replaced
			if start == 0 {
				start = l.Old
			}
			end = l.Old
		}
		if l.Op != "-" {
			repl = append(repl, l.Text)
		}
	}
	if start == 0 {
		return nil, FixOut{}, errors.New("pure insertions cannot be expressed as a suggestion: include at least one existing line in the hunk")
	}
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	var ff fixFile
	if err := h.loadJSON(in.RunID, h.headDir(st)+"fixes.json", &ff); err != nil {
		return nil, FixOut{}, err
	}
	ff.Next++
	fx := Fix{ID: fmt.Sprintf("X%d", ff.Next), Finding: ref, Agent: agent, Patch: patch, Path: f.NewPath, Start: start, End: end,
		Replacement: strings.Join(repl, "\n"), Modifies: in.Modifies, LeavesIntact: in.LeavesIntact}
	ff.Fixes = append(ff.Fixes, fx)
	if err := h.saveJSON(in.RunID, h.headDir(st)+"fixes.json", ff); err != nil {
		return nil, FixOut{}, err
	}
	_, _ = h.logEvent(in.RunID, "fix_suggested", "agent", map[string]any{"id": fx.ID, "finding": ref, "path": fx.Path, "range": []int{start, end}, "patch_hash": sha(patch)})
	return nil, FixOut{Fix: fx}, nil
}

type FixesOut struct {
	Fixes []Fix `json:"fixes"`
}

func (h *handlers) fixesList(_ context.Context, _ *mcp.CallToolRequest, in RunIn) (*mcp.CallToolResult, FixesOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, FixesOut{}, err
	}
	var ff fixFile
	err = h.loadJSON(in.RunID, h.headDir(st)+"fixes.json", &ff)
	if ff.Fixes == nil {
		ff.Fixes = []Fix{}
	}
	return nil, FixesOut{Fixes: ff.Fixes}, err
}

type AttachIn struct {
	RunID     string `json:"run_id"`
	CommentID string `json:"comment_id"`
	FixID     string `json:"fix_id"`
}

func (h *handlers) attachFix(_ context.Context, _ *mcp.CallToolRequest, in AttachIn) (*mcp.CallToolResult, DraftOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, DraftOut{}, err
	}
	m, err := h.loadMap(in.RunID)
	if err != nil {
		return nil, DraftOut{}, err
	}
	var ff fixFile
	if err := h.loadJSON(in.RunID, h.headDir(st)+"fixes.json", &ff); err != nil {
		return nil, DraftOut{}, err
	}
	var fx *Fix
	for i := range ff.Fixes {
		if ff.Fixes[i].ID == in.FixID {
			fx = &ff.Fixes[i]
		}
	}
	if fx == nil {
		return nil, DraftOut{}, fmt.Errorf("no fix %q", in.FixID)
	}
	ctxOld, oldPath, err := validateRange(m, fx.Path, "new", fx.Start, fx.End)
	if err != nil {
		return nil, DraftOut{}, fmt.Errorf("fix %s cannot be a suggestion: %w", fx.ID, err)
	}
	d, err := h.mutateDraft(in.RunID, in.CommentID, func(df *draftFile, i int) error {
		dr := &df.Drafts[i]
		dr.Path, dr.OldPath, dr.Side, dr.StartLine, dr.Line, dr.ContextOldLine, dr.FixID = fx.Path, oldPath, "new", fx.Start, fx.End, ctxOld, fx.ID
		if fx.Start == fx.End {
			dr.StartLine = 0
		}
		return nil
	})
	if err != nil {
		return nil, DraftOut{}, err
	}
	h.stateMu.Lock()
	fx.Approved = true
	_ = h.saveJSON(in.RunID, h.headDir(st)+"fixes.json", ff)
	h.stateMu.Unlock()
	_, _ = h.logEvent(in.RunID, "fix_attached", "orchestrator", map[string]any{"comment": in.CommentID, "fix": fx.ID, "patch_hash": sha(fx.Patch)})
	return nil, DraftOut{Draft: d}, nil
}

// ---- reviewer-first mode ----

type ModeIn struct {
	RunID         string `json:"run_id"`
	ReviewerFirst bool   `json:"reviewer_first"`
}

type modeFile struct {
	ReviewerFirst bool     `json:"reviewer_first"`
	Viewed        []string `json:"viewed"`
}

func (h *handlers) mode(runID string, st *checkoutState) modeFile {
	var mf modeFile
	_ = h.loadJSON(runID, h.headDir(st)+"mode.json", &mf)
	return mf
}

func (h *handlers) modeSet(_ context.Context, _ *mcp.CallToolRequest, in ModeIn) (*mcp.CallToolResult, OKOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, OKOut{}, err
	}
	h.stateMu.Lock()
	mf := h.mode(in.RunID, st)
	mf.ReviewerFirst = in.ReviewerFirst
	err = h.saveJSON(in.RunID, h.headDir(st)+"mode.json", mf)
	h.stateMu.Unlock()
	_, _ = h.logEvent(in.RunID, "review_mode", "orchestrator", map[string]any{"reviewer_first": in.ReviewerFirst})
	return nil, OKOut{OK: err == nil}, err
}

// findingsHidden reports whether reviewer-first mode still hides findings,
// and how many reviewable units remain unviewed.
func (h *handlers) findingsHidden(runID string) (bool, int, int) {
	st, err := h.loadCheckout(runID)
	if err != nil {
		return false, 0, 0
	}
	mf := h.mode(runID, st)
	if !mf.ReviewerFirst {
		return false, 0, 0
	}
	m, err := h.loadMap(runID)
	if err != nil {
		return true, 0, 0
	}
	seen := map[string]bool{}
	for _, v := range mf.Viewed {
		seen[v] = true
	}
	total, viewed := 0, 0
	for _, u := range m.Units {
		if u.Kind == diffmap.KindDoc || u.Kind == diffmap.KindBinary || u.Kind == diffmap.KindFile {
			continue
		}
		total++
		if seen[u.ID] {
			viewed++
		}
	}
	return viewed < total, viewed, total
}

func (h *handlers) markViewed(runID, unitID string) {
	st, err := h.loadCheckout(runID)
	if err != nil {
		return
	}
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	mf := h.mode(runID, st)
	if !mf.ReviewerFirst {
		return
	}
	for _, v := range mf.Viewed {
		if v == unitID {
			return
		}
	}
	mf.Viewed = append(mf.Viewed, unitID)
	_ = h.saveJSON(runID, h.headDir(st)+"mode.json", mf)
}

// ---- proposals ----

type Override struct {
	ID     string `json:"id" jsonschema:"draft comment id"`
	Reason string `json:"reason"`
}

type Proposal struct {
	ID        string            `json:"id"`
	Kind      string            `json:"kind" jsonschema:"review, reviewers or sticky"`
	HeadSHA   string            `json:"head_sha"`
	Payload   json.RawMessage   `json:"payload"`
	Preview   string            `json:"preview"`
	Hash      string            `json:"hash"`
	Created   time.Time         `json:"created"`
	Submitted bool              `json:"submitted"`
	Result    *forge.PostResult `json:"result,omitempty"`
}

type ProposalOut struct {
	ProposalID string   `json:"proposal_id"`
	Hash       string   `json:"hash" jsonschema:"pass to proposal_submit; it must match the frozen content"`
	Preview    string   `json:"preview" jsonschema:"the exact content that would be posted; show it to the human verbatim"`
	Warnings   []string `json:"warnings"`
}

func (h *handlers) saveProposal(runID string, p *Proposal) error {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	var idx struct{ Next int }
	_ = h.loadJSON(runID, "proposals/index.json", &idx)
	idx.Next++
	p.ID = fmt.Sprintf("P%d", idx.Next)
	if err := h.saveJSON(runID, "proposals/index.json", idx); err != nil {
		return err
	}
	return h.saveJSON(runID, "proposals/"+p.ID+".json", p)
}

func freeze(kind, head string, payload any, preview string, now time.Time) (*Proposal, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &Proposal{Kind: kind, HeadSHA: head, Payload: raw, Preview: preview, Hash: sha(kind + "\x00" + head + "\x00" + string(raw)), Created: now}, nil
}

// freshPR re-fetches the PR and fails if its head moved since checkout.
func (h *handlers) freshPR(ctx context.Context, st *checkoutState) (*forge.PR, forge.Provider, error) {
	p, err := h.cfg.Providers.Get(ctx, st.Repo)
	if err != nil {
		return nil, nil, err
	}
	pr, err := p.GetPR(ctx, forge.PRRef{Repo: st.Repo, Number: st.Number})
	if err != nil {
		return nil, nil, err
	}
	if pr.HeadSHA != "" && pr.HeadSHA != st.HeadSHA {
		return nil, nil, fmt.Errorf("the %s head moved from %s to %s since this review was prepared; re-run the review on the new head",
			st.Repo.Forge.Abbrev(), st.HeadSHA[:7], pr.HeadSHA[:min(7, len(pr.HeadSHA))])
	}
	if pr.BaseSHA == "" {
		pr.BaseSHA = st.MergeBase
	}
	return pr, p, nil
}

func footer(cfg config.Config, user, logHead string) string {
	if !cfg.Footer() {
		return ""
	}
	who := "the reviewer"
	if user != "" {
		who = "@" + user
	}
	return fmt.Sprintf("\n\n---\n<sub>Drafted with aireviewer (AI-assisted). Every item was reviewed and confirmed by %s before posting. Decision log: %s</sub>", who, logHead[:12])
}

func renderComment(kind forge.Kind, d Draft, fx *Fix, cfg config.Config) string {
	b := d.Body
	if fx != nil {
		b += "\n\n" + forge.SuggestionBlock(kind, fx.Replacement, fx.Start, fx.End)
		b += fmt.Sprintf("\n\n_This suggestion modifies:_ %s  \n_It leaves intact:_ %s", fx.Modifies, fx.LeavesIntact)
	}
	if cfg.Footer() {
		b += "\n\n<sub>aireviewer-assisted</sub>"
	}
	return b
}

type ReviewPrepareIn struct {
	RunID      string     `json:"run_id"`
	Verdict    string     `json:"verdict" jsonschema:"approve, request_changes or comment, chosen by the human reviewer"`
	Body       string     `json:"body" jsonschema:"review summary written or approved by the human"`
	CommentIDs []string   `json:"comment_ids" jsonschema:"draft comments to include"`
	Critique   string     `json:"critique,omitempty" jsonschema:"the reviewer's own architectural or mentorship note (required when review.require_critique)"`
	Overrides  []Override `json:"overrides,omitempty" jsonschema:"reasons for posting comments the gate would otherwise hold"`
}

func (h *handlers) reviewPrepare(ctx context.Context, _ *mcp.CallToolRequest, in ReviewPrepareIn) (*mcp.CallToolResult, ProposalOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, ProposalOut{}, err
	}
	verdict := forge.Verdict(in.Verdict)
	if verdict != forge.VerdictApprove && verdict != forge.VerdictRequestChanges && verdict != forge.VerdictComment {
		return nil, ProposalOut{}, errors.New("verdict must be approve, request_changes or comment, and it is the human's choice")
	}
	pr, _, err := h.freshPR(ctx, st)
	if err != nil {
		return nil, ProposalOut{}, err
	}
	cfg, _ := h.loadConfig(ctx, st)
	out := ProposalOut{Warnings: []string{}}
	user, uerr := h.currentUser(ctx, st.Repo)
	if uerr != nil {
		out.Warnings = append(out.Warnings, "could not identify the current user: "+uerr.Error())
	} else if strings.EqualFold(user, pr.Author) && verdict != forge.VerdictComment {
		return nil, ProposalOut{}, fmt.Errorf("you authored this %s; the forge will not accept your %s. Use verdict \"comment\"", st.Repo.Forge.Abbrev(), verdict)
	}
	if hidden, viewed, total := h.findingsHidden(in.RunID); hidden {
		return nil, ProposalOut{}, fmt.Errorf("reviewer-first mode: view every reviewable unit first (%d/%d viewed)", viewed, total)
	}
	if cfg.Review.RequireCritique && len(strings.TrimSpace(in.Critique)) < 40 {
		return nil, ProposalOut{}, errors.New("this repository requires a PR Critique: the reviewer's own architectural or mentorship note (at least 40 characters)")
	}
	overrides := map[string]string{}
	for _, o := range in.Overrides {
		if strings.TrimSpace(o.Reason) != "" {
			overrides[o.ID] = o.Reason
		}
	}
	var df draftFile
	if err := h.loadJSON(in.RunID, h.headDir(st)+"drafts.json", &df); err != nil {
		return nil, ProposalOut{}, err
	}
	var ff fixFile
	_ = h.loadJSON(in.RunID, h.headDir(st)+"fixes.json", &ff)
	disputes := h.disputes(in.RunID, st)
	byID := map[string]Draft{}
	for _, d := range df.Drafts {
		byID[d.ID] = d
	}
	var blocked []string
	review := forge.Review{Verdict: verdict, HeadSHA: st.HeadSHA, BaseSHA: pr.BaseSHA, StartSHA: pr.StartSHA, Comments: []forge.ReviewComment{}}
	if review.StartSHA == "" {
		review.StartSHA = st.MergeBase
	}
	usedOverrides := map[string]string{}
	for _, id := range in.CommentIDs {
		d, ok := byID[id]
		if !ok {
			return nil, ProposalOut{}, fmt.Errorf("no draft %q for the current head", id)
		}
		for _, check := range []struct{ name, mode string }{{"toxicity", cfg.Comments.Toxicity}, {"usefulness", cfg.Comments.Usefulness}} {
			if check.mode == "off" {
				continue
			}
			c, ok := d.Checks[check.name]
			if !ok || c.BodyHash != d.BodyHash {
				blocked = append(blocked, fmt.Sprintf("%s: %s check missing or stale (run it on the current text)", id, check.name))
				continue
			}
			switch {
			case c.Verdict == "fail" && check.mode == "block":
				if r, ok := overrides[id]; ok {
					usedOverrides[id] = r
				} else {
					blocked = append(blocked, fmt.Sprintf("%s: failed the %s check (%s); rewrite it or give an override reason", id, check.name, c.Reason))
				}
			case c.Verdict != "pass":
				out.Warnings = append(out.Warnings, fmt.Sprintf("%s: %s %s (%s)", id, check.name, c.Verdict, c.Reason))
			}
		}
		if cfg.Breaker() && d.FromFinding != "" {
			reason := ""
			if d.FindingConfidence == "low" {
				reason = "it comes from a low-confidence finding"
			}
			if dr, ok := disputes[d.FromFinding]; ok {
				reason = "its finding was disputed (" + dr + ")"
			}
			if reason != "" {
				if r, ok := overrides[id]; ok {
					usedOverrides[id] = r
				} else {
					blocked = append(blocked, fmt.Sprintf("%s: circuit breaker, because %s; confirm with an override reason or drop it", id, reason))
				}
			}
		}
		var fx *Fix
		for i := range ff.Fixes {
			if ff.Fixes[i].ID == d.FixID {
				fx = &ff.Fixes[i]
			}
		}
		review.Comments = append(review.Comments, forge.ReviewComment{Path: d.Path, OldPath: d.OldPath, Side: d.Side, Line: d.Line,
			StartLine: d.StartLine, ContextOldLine: d.ContextOldLine, Body: renderComment(st.Repo.Forge, d, fx, cfg)})
	}
	if len(blocked) > 0 {
		return nil, ProposalOut{}, fmt.Errorf("the gate holds this review:\n- %s", strings.Join(blocked, "\n- "))
	}
	body := strings.TrimSpace(in.Body)
	if c := strings.TrimSpace(in.Critique); c != "" {
		body += "\n\n### Reviewer notes\n" + c
	}
	review.Body = body + footer(cfg, user, h.logHead(in.RunID))

	var pv strings.Builder
	fmt.Fprintf(&pv, "**%s %s — verdict: %s** (head %s)\n\n#### Review body\n%s\n", st.Repo.Forge.Abbrev(), forge.PRRef{Repo: st.Repo, Number: st.Number}, verdict, st.HeadSHA[:7], review.Body)
	for i, c := range review.Comments {
		rng := fmt.Sprint(c.Line)
		if c.StartLine > 0 {
			rng = fmt.Sprintf("%d-%d", c.StartLine, c.Line)
		}
		fmt.Fprintf(&pv, "\n#### Comment %d of %d: `%s:%s` (%s)\n%s\n", i+1, len(review.Comments), c.Path, rng, c.Side, c.Body)
	}
	p, err := freeze("review", st.HeadSHA, review, pv.String(), h.now())
	if err != nil {
		return nil, ProposalOut{}, err
	}
	if err := h.saveProposal(in.RunID, p); err != nil {
		return nil, ProposalOut{}, err
	}
	_, _ = h.logEvent(in.RunID, "proposal_created", "orchestrator", map[string]any{"id": p.ID, "kind": "review", "hash": p.Hash,
		"verdict": string(verdict), "comments": in.CommentIDs, "overrides": usedOverrides})
	out.ProposalID, out.Hash, out.Preview = p.ID, p.Hash, p.Preview
	return nil, out, nil
}

type ReviewersPrepareIn struct {
	RunID string   `json:"run_id"`
	Users []string `json:"users" jsonschema:"@user or @org/team handles chosen by the human"`
}

func (h *handlers) reviewersPrepare(ctx context.Context, _ *mcp.CallToolRequest, in ReviewersPrepareIn) (*mcp.CallToolResult, ProposalOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, ProposalOut{}, err
	}
	if len(in.Users) == 0 {
		return nil, ProposalOut{}, errors.New("no reviewers chosen")
	}
	for _, u := range in.Users {
		if strings.HasPrefix(u, "git:") {
			return nil, ProposalOut{}, fmt.Errorf("%s is a git identity, not a forge username; map it to a username first", u)
		}
	}
	if _, _, err := h.freshPR(ctx, st); err != nil {
		return nil, ProposalOut{}, err
	}
	preview := fmt.Sprintf("**Request reviewers on %s %s:** %s", st.Repo.Forge.Abbrev(), forge.PRRef{Repo: st.Repo, Number: st.Number}, strings.Join(in.Users, ", "))
	p, err := freeze("reviewers", st.HeadSHA, in.Users, preview, h.now())
	if err != nil {
		return nil, ProposalOut{}, err
	}
	if err := h.saveProposal(in.RunID, p); err != nil {
		return nil, ProposalOut{}, err
	}
	_, _ = h.logEvent(in.RunID, "proposal_created", "orchestrator", map[string]any{"id": p.ID, "kind": "reviewers", "hash": p.Hash, "users": in.Users})
	return nil, ProposalOut{ProposalID: p.ID, Hash: p.Hash, Preview: preview, Warnings: []string{}}, nil
}

// ---- sticky augmentation comment ----

const markerPrefix = "<!-- aireviewer:v1 "

var dataRE = regexp.MustCompile(`<!-- aireviewer-data:v1 ([A-Za-z0-9+/=]+) -->`)
var headRE = regexp.MustCompile(`<!-- aireviewer:v1 run=\S+ head=([0-9a-f]{7,40}) -->`)

type stickyPayload struct {
	Marker string `json:"marker"`
	Body   string `json:"body"`
}

func (h *handlers) stickyPrepare(ctx context.Context, _ *mcp.CallToolRequest, in RunIn) (*mcp.CallToolResult, ProposalOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, ProposalOut{}, err
	}
	reports, err := h.acceptedReports(in.RunID, "augmentation", st.HeadSHA)
	if err != nil {
		return nil, ProposalOut{}, err
	}
	panel, perr := h.cfg.Store.Get(in.RunID, "augmentation/"+st.HeadSHA+"/summary-generator.md")
	if perr != nil || len(reports) == 0 {
		return nil, ProposalOut{}, errors.New("no augmentation summary for this head yet; run the analysts and summary-generator first")
	}
	if _, _, err := h.freshPR(ctx, st); err != nil {
		return nil, ProposalOut{}, err
	}
	cfg, _ := h.loadConfig(ctx, st)
	user, _ := h.currentUser(ctx, st.Repo)
	marker := fmt.Sprintf("%srun=%s head=%s -->", markerPrefix, in.RunID, st.HeadSHA)
	data, _ := json.Marshal(reports)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(data)
	_ = zw.Close()
	enc := base64.StdEncoding.EncodeToString(gz.Bytes())
	body := marker + "\n" + strings.TrimSpace(string(panel)) + footer(cfg, user, h.logHead(in.RunID))
	warnings := []string{}
	if len(body)+len(enc) < 60000 {
		body += "\n<!-- aireviewer-data:v1 " + enc + " -->"
	} else {
		warnings = append(warnings, "reports too large to embed; the comment is human-readable only and cannot be imported elsewhere")
	}
	preview := "**Sticky analysis comment** (replaces any previous aireviewer summary on this " + st.Repo.Forge.Noun() + "):\n\n" +
		strings.TrimSpace(string(panel)) + footer(cfg, user, h.logHead(in.RunID)) + "\n\n_(plus hidden markers with the machine-readable reports)_"
	p, err := freeze("sticky", st.HeadSHA, stickyPayload{Marker: markerPrefix, Body: body}, preview, h.now())
	if err != nil {
		return nil, ProposalOut{}, err
	}
	if err := h.saveProposal(in.RunID, p); err != nil {
		return nil, ProposalOut{}, err
	}
	_, _ = h.logEvent(in.RunID, "proposal_created", "orchestrator", map[string]any{"id": p.ID, "kind": "sticky", "hash": p.Hash})
	return nil, ProposalOut{ProposalID: p.ID, Hash: p.Hash, Preview: preview, Warnings: warnings}, nil
}

// ---- submit ----

type SubmitProposalIn struct {
	RunID      string `json:"run_id"`
	ProposalID string `json:"proposal_id"`
	Hash       string `json:"hash"`
}

type SubmitProposalOut struct {
	Posted       bool              `json:"posted"`
	Declined     bool              `json:"declined"`
	Confirmation string            `json:"confirmation" jsonschema:"elicitation (asked by the server) or permission_prompt (client tool approval)"`
	Result       *forge.PostResult `json:"result,omitempty"`
	LogHead      string            `json:"log_head"`
}

// confirmState binds an elicitation answer to one exact proposal.
func confirmState(p *Proposal) string { return p.ID + ":" + p.Hash }

func clientElicits(req *mcp.CallToolRequest) bool {
	if req == nil || req.Session == nil {
		return false
	}
	ip := req.Session.InitializeParams()
	return ip != nil && ip.Capabilities != nil && ip.Capabilities.Elicitation != nil
}

func elicitParams(p *Proposal, where string) *mcp.ElicitParams {
	msg := p.Preview
	if len(msg) > 6000 {
		msg = msg[:6000] + "\n… [preview truncated; full text in proposals/" + p.ID + ".json]"
	}
	msg += "\n\nPost exactly this to " + where + "?"
	return &mcp.ElicitParams{Message: msg, RequestedSchema: map[string]any{
		"type":       "object",
		"properties": map[string]any{"confirm": map[string]any{"type": "boolean", "title": "Yes, post exactly this", "default": false}},
		"required":   []string{"confirm"},
	}}
}

// confirm decides whether the human confirmed this proposal. With an
// elicitation-capable client the server asks the human itself: the first call
// returns an input request (SEP-2322 multi round-trip; the SDK falls back to a
// direct elicitation for older clients) and the retried call carries the
// answer, bound to this exact proposal through RequestState. Otherwise the
// client's tool-permission prompt is the confirmation, unless
// AIREVIEWER_STRICT_GATE=1.
func (h *handlers) confirm(req *mcp.CallToolRequest, p *Proposal, where string) (mech string, ok bool, ask *mcp.CallToolResult, err error) {
	if req != nil && req.Params != nil {
		if r, found := req.Params.InputResponses["confirm"]; found {
			if req.Params.RequestState != confirmState(p) {
				return "elicitation", false, nil, errors.New("confirmation does not belong to this proposal")
			}
			er, isElicit := r.(*mcp.ElicitResult)
			return "elicitation", isElicit && er.Action == "accept" && er.Content["confirm"] == true, nil, nil
		}
	}
	if clientElicits(req) {
		return "elicitation", false, &mcp.CallToolResult{
			InputRequests: mcp.InputRequestMap{"confirm": elicitParams(p, where)},
			RequestState:  confirmState(p),
		}, nil
	}
	if os.Getenv("AIREVIEWER_STRICT_GATE") == "1" {
		return "none", false, nil, errors.New("AIREVIEWER_STRICT_GATE=1: this client cannot show a server-side confirmation (MCP elicitation), so posting is refused")
	}
	// proposal_submit is never pre-approved by the skill, so the client's
	// tool-permission prompt is the confirmation.
	return "permission_prompt", true, nil, nil
}

func (h *handlers) proposalSubmit(ctx context.Context, req *mcp.CallToolRequest, in SubmitProposalIn) (*mcp.CallToolResult, SubmitProposalOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, SubmitProposalOut{}, err
	}
	var p Proposal
	if err := h.loadJSON(in.RunID, "proposals/"+in.ProposalID+".json", &p); err != nil || p.ID == "" {
		return nil, SubmitProposalOut{}, fmt.Errorf("no proposal %q", in.ProposalID)
	}
	if p.Submitted {
		return nil, SubmitProposalOut{}, fmt.Errorf("proposal %s was already submitted", p.ID)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, p.Payload); err != nil {
		return nil, SubmitProposalOut{}, fmt.Errorf("proposal %s is corrupt: %w", p.ID, err)
	}
	if in.Hash != p.Hash || p.Hash != sha(p.Kind+"\x00"+p.HeadSHA+"\x00"+compact.String()) {
		return nil, SubmitProposalOut{}, errors.New("hash mismatch: the proposal changed or the wrong hash was given; prepare it again")
	}
	if p.HeadSHA != st.HeadSHA {
		return nil, SubmitProposalOut{}, errors.New("the proposal was prepared for an older head; prepare it again")
	}
	pr, prov, err := h.freshPR(ctx, st)
	if err != nil {
		return nil, SubmitProposalOut{}, err
	}
	where := fmt.Sprintf("%s %s", st.Repo.Forge.Abbrev(), pr.Ref)
	mech, ok, ask, err := h.confirm(req, &p, where)
	if ask != nil {
		return ask, SubmitProposalOut{}, nil // the client asks the human and retries with the answer
	}
	if err != nil {
		_, _ = h.logEvent(in.RunID, "confirmation_failed", "server", map[string]any{"id": p.ID, "error": err.Error()})
		return nil, SubmitProposalOut{}, err
	}
	if !ok {
		head, _ := h.logEvent(in.RunID, "human_declined", "human", map[string]any{"id": p.ID, "hash": p.Hash, "via": mech})
		return nil, SubmitProposalOut{Declined: true, Confirmation: mech, LogHead: head}, nil
	}
	actor := "human"
	if mech == "permission_prompt" {
		actor = "human_via_permission_prompt"
	}
	_, _ = h.logEvent(in.RunID, "human_confirmed", actor, map[string]any{"id": p.ID, "hash": p.Hash, "via": mech})

	var res forge.PostResult
	switch p.Kind {
	case "review":
		rp, ok := prov.(forge.ReviewPoster)
		if !ok {
			return nil, SubmitProposalOut{}, errors.New("this forge adapter cannot post reviews")
		}
		var r forge.Review
		if err := json.Unmarshal(p.Payload, &r); err != nil {
			return nil, SubmitProposalOut{}, err
		}
		res, err = rp.SubmitReview(ctx, pr, r)
	case "reviewers":
		rr, ok := prov.(forge.ReviewerRequester)
		if !ok {
			return nil, SubmitProposalOut{}, errors.New("this forge adapter cannot request reviewers")
		}
		var users []string
		if err := json.Unmarshal(p.Payload, &users); err != nil {
			return nil, SubmitProposalOut{}, err
		}
		res, err = rr.RequestReviewers(ctx, pr, users)
	case "sticky":
		mc, ok := prov.(forge.MarkerCommenter)
		if !ok {
			return nil, SubmitProposalOut{}, errors.New("this forge adapter cannot post comments")
		}
		var sp stickyPayload
		if err := json.Unmarshal(p.Payload, &sp); err != nil {
			return nil, SubmitProposalOut{}, err
		}
		res, err = mc.UpsertMarkerComment(ctx, pr, sp.Marker, sp.Body)
	default:
		return nil, SubmitProposalOut{}, fmt.Errorf("unknown proposal kind %q", p.Kind)
	}
	if err != nil {
		head, _ := h.logEvent(in.RunID, "post_failed", "server", map[string]any{"id": p.ID, "error": err.Error()})
		return nil, SubmitProposalOut{Confirmation: mech, LogHead: head}, fmt.Errorf("posting failed (logged): %w", err)
	}
	p.Submitted, p.Result = true, &res
	h.stateMu.Lock()
	_ = h.saveJSON(in.RunID, "proposals/"+p.ID+".json", p)
	h.stateMu.Unlock()
	head, _ := h.logEvent(in.RunID, "posted", "server", map[string]any{"id": p.ID, "kind": p.Kind, "hash": p.Hash, "url": res.URL, "notes": res.Notes})
	return nil, SubmitProposalOut{Posted: true, Confirmation: mech, Result: &res, LogHead: head}, nil
}

// ---- import ----

type ImportOut struct {
	Imported []string         `json:"imported" jsonschema:"agents whose reports were accepted"`
	Dropped  []schema.Dropped `json:"dropped"`
	Source   string           `json:"source,omitempty"`
	Note     string           `json:"note,omitempty"`
}

func (h *handlers) augmentationImport(ctx context.Context, _ *mcp.CallToolRequest, in RunIn) (*mcp.CallToolResult, ImportOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, ImportOut{}, err
	}
	pr, err := h.loadPR(in.RunID)
	if err != nil {
		return nil, ImportOut{}, err
	}
	out := ImportOut{Imported: []string{}, Dropped: []schema.Dropped{}}
	prov, err := h.cfg.Providers.Get(ctx, st.Repo)
	if err != nil {
		return nil, ImportOut{}, err
	}
	mc, ok := prov.(forge.MarkerCommenter)
	if !ok {
		out.Note = "this forge adapter cannot read comments"
		return nil, out, nil
	}
	c, err := mc.FindMarkerComment(ctx, pr, markerPrefix)
	if err != nil {
		return nil, ImportOut{}, err
	}
	if c == nil {
		out.Note = "no aireviewer summary comment on this " + st.Repo.Forge.Noun()
		return nil, out, nil
	}
	out.Source = c.URL
	if m := headRE.FindStringSubmatch(c.Body); m == nil || !strings.HasPrefix(st.HeadSHA, m[1]) {
		out.Note = "the posted summary is for a different head; not imported"
		return nil, out, nil
	}
	cfg, _ := h.loadConfig(ctx, st)
	trusted := false
	if u, err := h.currentUser(ctx, st.Repo); err == nil && strings.EqualFold(u, c.Author) {
		trusted = true
	}
	for _, b := range cfg.Gate.TrustedBots {
		if strings.EqualFold(strings.TrimPrefix(b, "@"), c.Author) {
			trusted = true
		}
	}
	if !trusted {
		out.Note = "the summary was posted by " + c.Author + ", who is neither you nor a configured trusted bot (gate.trusted_bots); not imported"
		return nil, out, nil
	}
	m := dataRE.FindStringSubmatch(c.Body)
	if m == nil {
		out.Note = "the summary has no machine-readable reports"
		return nil, out, nil
	}
	raw, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		return nil, ImportOut{}, fmt.Errorf("decode reports: %w", err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, ImportOut{}, fmt.Errorf("decode reports: %w", err)
	}
	data, err := io.ReadAll(io.LimitReader(zr, 16<<20))
	if err != nil {
		return nil, ImportOut{}, err
	}
	var reports []json.RawMessage
	if err := json.Unmarshal(data, &reports); err != nil {
		return nil, ImportOut{}, fmt.Errorf("decode reports: %w", err)
	}
	dm, err := h.loadMap(in.RunID)
	if err != nil {
		return nil, ImportOut{}, err
	}
	// Analysts first, the summary last (its sources must resolve).
	sort.SliceStable(reports, func(i, j int) bool {
		return !strings.Contains(string(reports[i]), `"agent":"summary-generator"`) && strings.Contains(string(reports[j]), `"agent":"summary-generator"`)
	})
	for _, rr := range reports {
		var peek struct {
			Agent string `json:"agent"`
		}
		_ = json.Unmarshal(rr, &peek)
		agent := strings.Trim(agentRE.ReplaceAllString(strings.ToLower(peek.Agent), "-"), "-.")
		res, err := schema.Validate(rr, h.env(ctx, in.RunID, st, dm, "augmentation", peek.Agent))
		if err != nil || agent == "" {
			out.Dropped = append(out.Dropped, schema.Dropped{ClaimID: "*" + agent, Reasons: []string{fmt.Sprint(err)}})
			continue
		}
		if res.Report.Facets == nil {
			res.Report.Facets = map[string]any{}
		}
		res.Report.Facets["imported_from"] = c.URL
		data, _ := json.MarshalIndent(res.Report, "", "  ")
		if _, err := h.cfg.Store.Put(in.RunID, "augmentation/"+st.HeadSHA+"/"+agent+".json", data); err != nil {
			return nil, ImportOut{}, err
		}
		out.Imported = append(out.Imported, agent)
		out.Dropped = append(out.Dropped, res.Dropped...)
	}
	_, _ = h.logEvent(in.RunID, "augmentation_imported", "server", map[string]any{"source": c.URL, "author": c.Author, "agents": out.Imported, "dropped": len(out.Dropped)})
	return nil, out, nil
}

// ---- decision log ----

type DecisionsOut struct {
	Entries  []decisionlog.Entry `json:"entries"`
	Valid    bool                `json:"valid"`
	Problem  string              `json:"problem,omitempty"`
	HeadHash string              `json:"head_hash"`
}

func (h *handlers) decisionsGet(_ context.Context, _ *mcp.CallToolRequest, in RunIn) (*mcp.CallToolResult, DecisionsOut, error) {
	cur, err := h.cfg.Store.Get(in.RunID, "decisions.jsonl")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, DecisionsOut{}, err
	}
	es, _ := decisionlog.Parse(cur)
	out := DecisionsOut{Entries: es, Valid: true, HeadHash: h.logHead(in.RunID)}
	if out.Entries == nil {
		out.Entries = []decisionlog.Entry{}
	}
	if _, err := decisionlog.Verify(cur); err != nil {
		out.Valid, out.Problem = false, err.Error()
	}
	return nil, out, nil
}
