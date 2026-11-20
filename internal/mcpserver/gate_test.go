// SPDX-License-Identifier: MIT

package mcpserver

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/husobee/aireviewer/internal/forge"
)

// gateProvider is a forge fake that records every write.
type gateProvider struct {
	prProvider
	mu        sync.Mutex
	author    string
	user      string
	curHead   string
	reviews   []forge.Review
	requested [][]string
	marker    *forge.MarkerComment
}

func (g *gateProvider) GetPR(ctx context.Context, ref forge.PRRef) (*forge.PR, error) {
	pr, _ := g.prProvider.GetPR(ctx, ref)
	g.mu.Lock()
	defer g.mu.Unlock()
	pr.Author = g.author
	if g.curHead != "" {
		pr.HeadSHA = g.curHead
	}
	return pr, nil
}
func (g *gateProvider) CurrentUser(context.Context, forge.Repo) (string, error) { return g.user, nil }
func (g *gateProvider) SubmitReview(_ context.Context, _ *forge.PR, r forge.Review) (forge.PostResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reviews = append(g.reviews, r)
	return forge.PostResult{URL: "https://forge/review/1"}, nil
}
func (g *gateProvider) RequestReviewers(_ context.Context, _ *forge.PR, users []string) (forge.PostResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.requested = append(g.requested, users)
	return forge.PostResult{URL: "https://forge/pr"}, nil
}
func (g *gateProvider) FindMarkerComment(context.Context, *forge.PR, string) (*forge.MarkerComment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.marker, nil
}
func (g *gateProvider) UpsertMarkerComment(_ context.Context, _ *forge.PR, _, body string) (forge.PostResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.marker = &forge.MarkerComment{ID: 1, Author: g.user, Body: body, URL: "https://forge/comment/1"}
	return forge.PostResult{URL: g.marker.URL}, nil
}

type gateSource struct{ p *gateProvider }

func (s gateSource) Get(context.Context, forge.Repo) (forge.Provider, error) { return s.p, nil }

func elicit(action string, confirm bool) *mcp.ClientOptions {
	return &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: action, Content: map[string]any{"confirm": confirm}}, nil
	}}
}

func callErr(t *testing.T, cs *mcp.ClientSession, name string, args any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("%s unexpectedly succeeded", name)
	}
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// prepRun checks out the fixture PR and submits one code-review report with a
// high-confidence finding (F1, lines 4-5) and a low-confidence one (F2, line 9).
func prepRun(t *testing.T, cs *mcp.ClientSession, head string) string {
	t.Helper()
	co := call[CheckoutOut](t, cs, "pr_checkout", map[string]any{"url": "https://github.com/o/r/pull/3"})
	call[DiffmapOut](t, cs, "diffmap_build", map[string]any{"run_id": co.RunID})
	rep := fmt.Sprintf(`{"agent":"code-reviewer","version":1,"head_sha":%q,"status":"ok","summary":"s","claims":[
		{"id":"F1","statement":"loop body is empty","category":"defect","severity":"major","confidence":"high",
		 "evidence":[{"kind":"code","file":"svc/svc.go","lines":[4,5]}],"anchors":["unit:svc/svc.go#Retry"]},
		{"id":"F2","statement":"New is unused","category":"defect","severity":"minor","confidence":"low",
		 "evidence":[{"kind":"code","file":"svc/svc.go","lines":[9,9]}],"anchors":["unit:svc/svc.go#New"]}]}`, head)
	tok := slot(t, cs, co.RunID, "code-reviewer")
	if s := call[SubmitOut](t, cs, "report_submit", map[string]any{"run_id": co.RunID, "token": tok, "report": rep}); s.Kept != 2 {
		t.Fatalf("report = %+v", s)
	}
	return co.RunID
}

func passChecks(t *testing.T, cs *mcp.ClientSession, run, id string) {
	t.Helper()
	for _, c := range []string{"toxicity", "usefulness"} {
		call[DraftOut](t, cs, "comment_check_record", map[string]any{"run_id": run, "id": id, "check": c, "verdict": "pass"})
	}
}

const fixPatch = `--- a/svc/svc.go
+++ b/svc/svc.go
@@ -4,2 +4,3 @@
 	for i := 0; i < n; i++ {
+		backoff(i)
 	}
`

func TestGateReviewFlow(t *testing.T) {
	src, head := fixtureRepo(t)
	gp := &gateProvider{prProvider: prProvider{head: head}, author: "alice", user: "bob"}
	cs, root := connectServer(t, src, gateSource{gp}, elicit("accept", true))
	run := prepRun(t, cs, head)

	if msg := callErr(t, cs, "comment_draft_add", map[string]any{"run_id": run, "path": "svc/use.go", "line": 3, "body": "x"}); !strings.Contains(msg, "not part of the PR diff") {
		t.Errorf("non-diff line: %s", msg)
	}
	c1 := call[DraftOut](t, cs, "comment_draft_add", map[string]any{"run_id": run, "path": "svc/svc.go", "start_line": 4, "line": 5,
		"body": "This loop retries without waiting; add a backoff.", "from_finding": "code-reviewer:F1"}).Draft
	c2 := call[DraftOut](t, cs, "comment_draft_add", map[string]any{"run_id": run, "path": "svc/svc.go", "line": 9,
		"body": "Is New used anywhere?", "from_finding": "code-reviewer:F2"}).Draft
	if c1.ID != "C1" || c2.ID != "C2" || c2.FindingConfidence != "low" {
		t.Fatalf("drafts = %+v %+v", c1, c2)
	}

	prep := map[string]any{"run_id": run, "verdict": "request_changes", "body": "Two issues.", "comment_ids": []string{"C1", "C2"}}
	if msg := callErr(t, cs, "review_prepare", prep); !strings.Contains(msg, "toxicity check missing") {
		t.Errorf("unchecked drafts not held: %s", msg)
	}
	passChecks(t, cs, run, "C1")
	passChecks(t, cs, run, "C2")
	if msg := callErr(t, cs, "review_prepare", prep); !strings.Contains(msg, "C2: circuit breaker") || strings.Contains(msg, "C1:") {
		t.Errorf("circuit breaker: %s", msg)
	}
	// Editing a checked draft invalidates its checks.
	call[DraftOut](t, cs, "comment_draft_update", map[string]any{"run_id": run, "id": "C2", "body": "Is New() called anywhere yet?"})
	prep["overrides"] = []map[string]any{{"id": "C2", "reason": "reviewer verified manually"}}
	if msg := callErr(t, cs, "review_prepare", prep); !strings.Contains(msg, "C2: toxicity check missing") {
		t.Errorf("stale check not detected: %s", msg)
	}
	passChecks(t, cs, run, "C2")

	// Fix suggestion: must apply to head and becomes a suggestion block.
	ftok := slot(t, cs, run, "fix-suggester-1")
	bad := strings.Replace(fixPatch, "for i := 0", "for j := 0", 1)
	if msg := callErr(t, cs, "fix_submit", map[string]any{"run_id": run, "agent_name": "fix-suggester-1", "token": ftok, "finding": "code-reviewer:F1",
		"patch": bad, "modifies": "m", "leaves_intact": "l"}); !strings.Contains(msg, "does not apply") {
		t.Errorf("bad patch: %s", msg)
	}
	if msg := callErr(t, cs, "fix_submit", map[string]any{"run_id": run, "agent_name": "fix-suggester-1", "token": "wrong", "finding": "code-reviewer:F1",
		"patch": fixPatch, "modifies": "m", "leaves_intact": "l"}); !strings.Contains(msg, "no report slot") {
		t.Errorf("fix without token: %s", msg)
	}
	fx := call[FixOut](t, cs, "fix_submit", map[string]any{"run_id": run, "agent_name": "fix-suggester-1", "token": ftok, "finding": "code-reviewer:F1",
		"patch": fixPatch, "modifies": "adds a backoff call inside the loop", "leaves_intact": "loop bounds and return value"}).Fix
	if fx.Start != 4 || fx.End != 5 || !strings.Contains(fx.Replacement, "backoff(i)") {
		t.Fatalf("fix = %+v", fx)
	}
	call[DraftOut](t, cs, "comment_attach_fix", map[string]any{"run_id": run, "comment_id": "C1", "fix_id": fx.ID})
	dl := call[DraftsOut](t, cs, "comment_drafts_list", map[string]any{"run_id": run})
	if !strings.Contains(dl.Drafts[0].Rendered, "```suggestion\n") || !strings.Contains(dl.Drafts[1].Status[0], "circuit breaker") {
		t.Errorf("drafts list = %+v", dl.Drafts)
	}

	pp := call[ProposalOut](t, cs, "review_prepare", prep)
	if !strings.Contains(pp.Preview, "verdict: request_changes") || !strings.Contains(pp.Preview, "```suggestion") ||
		!strings.Contains(pp.Preview, "confirmed by @bob") || pp.Hash == "" {
		t.Fatalf("preview:\n%s", pp.Preview)
	}
	if msg := callErr(t, cs, "proposal_submit", map[string]any{"run_id": run, "proposal_id": pp.ProposalID, "hash": "deadbeef"}); !strings.Contains(msg, "hash mismatch") {
		t.Errorf("wrong hash: %s", msg)
	}
	sub := call[SubmitProposalOut](t, cs, "proposal_submit", map[string]any{"run_id": run, "proposal_id": pp.ProposalID, "hash": pp.Hash})
	if !sub.Posted || sub.Confirmation != "elicitation" || len(gp.reviews) != 1 {
		t.Fatalf("submit = %+v reviews=%d", sub, len(gp.reviews))
	}
	r := gp.reviews[0]
	if r.Verdict != forge.VerdictRequestChanges || len(r.Comments) != 2 || r.Comments[0].StartLine != 4 || r.Comments[0].Line != 5 ||
		!strings.Contains(r.Comments[0].Body, "```suggestion") || r.HeadSHA != head {
		t.Errorf("posted review = %+v", r)
	}
	if msg := callErr(t, cs, "proposal_submit", map[string]any{"run_id": run, "proposal_id": pp.ProposalID, "hash": pp.Hash}); !strings.Contains(msg, "already submitted") {
		t.Errorf("double submit: %s", msg)
	}

	// The decision log has the full trail and detects tampering.
	dg := call[DecisionsOut](t, cs, "decisions_get", map[string]any{"run_id": run})
	var events []string
	for _, e := range dg.Entries {
		events = append(events, e.Event)
	}
	joined := strings.Join(events, ",")
	for _, want := range []string{"comment_drafted", "comment_checked", "fix_suggested", "fix_attached", "proposal_created", "human_confirmed", "posted"} {
		if !strings.Contains(joined, want) {
			t.Errorf("log lacks %s: %v", want, events)
		}
	}
	if !dg.Valid {
		t.Errorf("log invalid: %s", dg.Problem)
	}
	logPath := root + "/.aireviewer/runs/" + run + "/decisions.jsonl"
	b, _ := os.ReadFile(logPath)
	_ = os.WriteFile(logPath, []byte(strings.Replace(string(b), `"posted"`, `"declined"`, 1)), 0o600)
	if dg := call[DecisionsOut](t, cs, "decisions_get", map[string]any{"run_id": run}); dg.Valid {
		t.Error("tampered log reported valid")
	}
}

func TestGateRefusals(t *testing.T) {
	src, head := fixtureRepo(t)

	// Self-review: the author cannot approve.
	gp := &gateProvider{prProvider: prProvider{head: head}, author: "bob", user: "bob"}
	cs, _ := connectServer(t, src, gateSource{gp}, elicit("decline", false))
	run := prepRun(t, cs, head)
	if w := call[WhoamiOut](t, cs, "whoami", map[string]any{"run_id": run}); !w.IsAuthor || w.CanApprove {
		t.Errorf("whoami = %+v", w)
	}
	if msg := callErr(t, cs, "review_prepare", map[string]any{"run_id": run, "verdict": "approve", "body": "ok", "comment_ids": []string{}}); !strings.Contains(msg, "you authored") {
		t.Errorf("self-approve: %s", msg)
	}
	// Declined confirmation: nothing posted, decline logged.
	pp := call[ProposalOut](t, cs, "review_prepare", map[string]any{"run_id": run, "verdict": "comment", "body": "notes", "comment_ids": []string{}})
	sub := call[SubmitProposalOut](t, cs, "proposal_submit", map[string]any{"run_id": run, "proposal_id": pp.ProposalID, "hash": pp.Hash})
	if !sub.Declined || sub.Posted || sub.Reason == "" || len(gp.reviews) != 0 {
		t.Errorf("declined submit = %+v", sub)
	}
	// Head moved after preparing: refused.
	pp = call[ProposalOut](t, cs, "review_prepare", map[string]any{"run_id": run, "verdict": "comment", "body": "notes", "comment_ids": []string{}})
	gp.mu.Lock()
	gp.curHead = "ffffffffffffffffffffffffffffffffffffffff"
	gp.mu.Unlock()
	if msg := callErr(t, cs, "proposal_submit", map[string]any{"run_id": run, "proposal_id": pp.ProposalID, "hash": pp.Hash}); !strings.Contains(msg, "head moved") {
		t.Errorf("moved head: %s", msg)
	}
	// git: identities cannot be requested.
	gp.mu.Lock()
	gp.curHead = ""
	gp.mu.Unlock()
	if msg := callErr(t, cs, "reviewers_prepare", map[string]any{"run_id": run, "users": []string{"git:x@y"}}); !strings.Contains(msg, "git identity") {
		t.Errorf("git identity: %s", msg)
	}
}

func TestGateNoElicitation(t *testing.T) {
	src, head := fixtureRepo(t)
	gp := &gateProvider{prProvider: prProvider{head: head}, author: "alice", user: "bob"}
	cs, _ := connectServer(t, src, gateSource{gp}, nil) // client without elicitation
	run := prepRun(t, cs, head)
	pp := call[ProposalOut](t, cs, "reviewers_prepare", map[string]any{"run_id": run, "users": []string{"@carol", "@org/team"}})

	t.Setenv("AIREVIEWER_STRICT_GATE", "1")
	if msg := callErr(t, cs, "proposal_submit", map[string]any{"run_id": run, "proposal_id": pp.ProposalID, "hash": pp.Hash}); !strings.Contains(msg, "STRICT_GATE") {
		t.Errorf("strict gate: %s", msg)
	}
	t.Setenv("AIREVIEWER_STRICT_GATE", "")
	sub := call[SubmitProposalOut](t, cs, "proposal_submit", map[string]any{"run_id": run, "proposal_id": pp.ProposalID, "hash": pp.Hash})
	if !sub.Posted || sub.Confirmation != "permission_prompt" || len(gp.requested) != 1 || gp.requested[0][1] != "@org/team" {
		t.Errorf("permission-prompt submit = %+v requested=%v", sub, gp.requested)
	}
	dg := call[DecisionsOut](t, cs, "decisions_get", map[string]any{"run_id": run})
	found := false
	for _, e := range dg.Entries {
		if e.Event == "human_confirmed" && e.Actor == "human_via_permission_prompt" {
			found = true
		}
	}
	if !found {
		t.Error("permission-prompt confirmation not logged as such")
	}
}

func TestReviewerFirst(t *testing.T) {
	src, head := fixtureRepo(t)
	gp := &gateProvider{prProvider: prProvider{head: head}, author: "alice", user: "bob"}
	cs, _ := connectServer(t, src, gateSource{gp}, elicit("accept", true))
	run := prepRun(t, cs, head)
	call[OKOut](t, cs, "review_mode_set", map[string]any{"run_id": run, "reviewer_first": true})
	if msg := callErr(t, cs, "findings_list", map[string]any{"run_id": run}); !strings.Contains(msg, "reviewer-first") {
		t.Errorf("findings visible: %s", msg)
	}
	plan := call[PlanOut](t, cs, "review_plan", map[string]any{"run_id": run})
	var units []string
	for _, b := range plan.Batches {
		units = append(units, b...)
	}
	for i, u := range units {
		uo := call[UnitOut](t, cs, "diffmap_unit", map[string]any{"run_id": run, "unit_id": u})
		if i < len(units)-1 && (len(uo.Findings) != 0 || strings.Contains(uo.Highlighted, "▶") || uo.Note == "") {
			t.Errorf("unit %s leaked findings before all were viewed", u)
		}
	}
	if fl := call[FindingsListOut](t, cs, "findings_list", map[string]any{"run_id": run}); len(fl.Findings) != 2 {
		t.Errorf("findings after viewing = %d", len(fl.Findings))
	}
}

func TestStickyExportImport(t *testing.T) {
	src, head := fixtureRepo(t)
	gp := &gateProvider{prProvider: prProvider{head: head}, author: "alice", user: "bob"}
	cs, _ := connectServer(t, src, gateSource{gp}, elicit("accept", true))
	co := call[CheckoutOut](t, cs, "pr_checkout", map[string]any{"url": "https://github.com/o/r/pull/3"})
	run := co.RunID
	call[DiffmapOut](t, cs, "diffmap_build", map[string]any{"run_id": run})
	rm := call[RepoMetricsOut](t, cs, "repo_metrics", map[string]any{"run_id": run})
	risk := fmt.Sprintf(`{"agent":"risk-analyst","version":1,"head_sha":%q,"status":"ok","summary":"risk","facets":{"risk_score":%d},"claims":[
		{"id":"R1","statement":"risk","category":"risk","severity":"info","confidence":"low",
		 "evidence":[{"kind":"metric","name":"pr_risk","value":%d,"source":"repo_metrics"}]}]}`, head, rm.Risk.Score, rm.Risk.Score)
	tok := call[SlotOut](t, cs, "report_slot", map[string]any{"run_id": run, "stage": "augmentation", "agent_name": "risk-analyst"}).Token
	call[SubmitOut](t, cs, "report_submit", map[string]any{"run_id": run, "stage": "augmentation", "token": tok, "report": risk})
	sum := fmt.Sprintf(`{"agent":"summary-generator","version":1,"head_sha":%q,"status":"ok","summary":"overall","claims":[]}`, head)
	tok = call[SlotOut](t, cs, "report_slot", map[string]any{"run_id": run, "stage": "augmentation", "agent_name": "summary-generator"}).Token
	call[SubmitOut](t, cs, "report_submit", map[string]any{"run_id": run, "stage": "augmentation", "token": tok, "report": sum,
		"markdown": "## Augmentation summary\nRisk low."})

	pp := call[ProposalOut](t, cs, "sticky_prepare", map[string]any{"run_id": run})
	if !strings.Contains(pp.Preview, "## Augmentation summary") {
		t.Fatalf("sticky preview: %s", pp.Preview)
	}
	call[SubmitProposalOut](t, cs, "proposal_submit", map[string]any{"run_id": run, "proposal_id": pp.ProposalID, "hash": pp.Hash})
	if gp.marker == nil || !strings.Contains(gp.marker.Body, "<!-- aireviewer:v1 run=") || !strings.Contains(gp.marker.Body, "aireviewer-data:v1") {
		t.Fatalf("sticky not posted: %+v", gp.marker)
	}

	// A different machine (fresh workspace) imports and re-validates.
	cs2, _ := connectServer(t, src, gateSource{gp}, nil)
	co2 := call[CheckoutOut](t, cs2, "pr_checkout", map[string]any{"url": "https://github.com/o/r/pull/3"})
	call[DiffmapOut](t, cs2, "diffmap_build", map[string]any{"run_id": co2.RunID})
	call[RepoMetricsOut](t, cs2, "repo_metrics", map[string]any{"run_id": co2.RunID})
	imp := call[ImportOut](t, cs2, "augmentation_import", map[string]any{"run_id": co2.RunID})
	if len(imp.Imported) != 2 || imp.Imported[len(imp.Imported)-1] != "summary-generator" {
		t.Fatalf("import = %+v", imp)
	}
	rs := call[ReportsOut](t, cs2, "reports_get", map[string]any{"run_id": co2.RunID, "stage": "augmentation"})
	if len(rs.Reports) != 2 || rs.Reports[0].Facets["imported_from"] == nil {
		t.Errorf("imported reports = %+v", rs.Reports)
	}

	// Untrusted author: refused.
	gp.mu.Lock()
	gp.marker.Author = "mallory"
	gp.mu.Unlock()
	if imp := call[ImportOut](t, cs2, "augmentation_import", map[string]any{"run_id": co2.RunID}); len(imp.Imported) != 0 || !strings.Contains(imp.Note, "mallory") {
		t.Errorf("untrusted import = %+v", imp)
	}
}

func TestElicitAnswer(t *testing.T) {
	cases := []struct {
		name string
		r    mcp.InputResponse
		ok   bool
	}{
		{"accept, box left ticked", &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, true},
		{"accept, default omitted by client", &mcp.ElicitResult{Action: "accept"}, true},
		{"accept, box unticked", &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": false}}, false},
		{"accept, non-bool value", &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": "yes"}}, false},
		{"decline", &mcp.ElicitResult{Action: "decline", Content: map[string]any{"confirm": true}}, false},
		{"cancel", &mcp.ElicitResult{Action: "cancel"}, false},
		{"nil response", nil, false},
	}
	for _, c := range cases {
		ok, reason := elicitAnswer(c.r)
		if ok != c.ok {
			t.Errorf("%s: ok=%v, want %v (reason %q)", c.name, ok, c.ok, reason)
		}
		if !ok && reason == "" {
			t.Errorf("%s: a refusal must carry a reason", c.name)
		}
	}
}
