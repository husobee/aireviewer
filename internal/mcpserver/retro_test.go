// SPDX-License-Identifier: MIT

package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/gitrepo"
	"github.com/husobee/aireviewer/internal/retro"
	"github.com/husobee/aireviewer/internal/runctx"
)

type activityProvider struct {
	*gateProvider
}

func (a activityProvider) Activity(context.Context, *forge.PR) (*forge.Activity, error) {
	c := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	merged := c.Add(30 * time.Hour)
	return &forge.Activity{CreatedAt: c, MergedAt: &merged, Author: "alice", Events: []forge.ActivityEvent{
		{Kind: forge.EventReview, Author: "bob", At: c.Add(4 * time.Hour), State: "commented", Body: "x\n" + forge.ProvenanceMarker, AIAssisted: true},
		{Kind: forge.EventCommit, Author: "alice", At: c.Add(8 * time.Hour)},
		{Kind: forge.EventApproval, Author: "carol", At: c.Add(28 * time.Hour), State: "approved"},
	}}, nil
}

type activitySource struct{ p activityProvider }

func (s activitySource) Get(context.Context, forge.Repo) (forge.Provider, error) { return s.p, nil }

func TestRetroFlow(t *testing.T) {
	src, head := fixtureRepo(t)
	gp := &gateProvider{prProvider: prProvider{head: head}, author: "alice", user: "bob"}
	root := t.TempDir()
	srv := New(Config{Version: "test", RepoRoot: root, Store: runctx.New(root), CacheDir: t.TempDir(), MemoryDir: t.TempDir(),
		Providers: activitySource{activityProvider{gp}},
		RemoteFor: func(repo forge.Repo, tok string) gitrepo.Remote {
			return gitrepo.Remote{URL: "file://" + src, Kind: repo.Forge, Token: tok}
		}})
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, elicit("accept", true)).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()

	// A review done with aireviewer: F1 commented and posted, F2 disputed.
	run := prepRun(t, cs, head)
	call[DraftOut](t, cs, "comment_draft_add", map[string]any{"run_id": run, "path": "svc/svc.go", "start_line": 4, "line": 5,
		"body": "This loop retries without waiting; add a backoff.", "from_finding": "code-reviewer:F1"})
	passChecks(t, cs, run, "C1")
	call[OKOut](t, cs, "finding_dispute", map[string]any{"run_id": run, "finding": "code-reviewer:F2", "reason": "New is used by a later PR"})
	pp := call[ProposalOut](t, cs, "review_prepare", map[string]any{"run_id": run, "verdict": "comment", "body": "One issue.", "comment_ids": []string{"C1"}})
	call[SubmitProposalOut](t, cs, "proposal_submit", map[string]any{"run_id": run, "proposal_id": pp.ProposalID, "hash": pp.Hash})

	m := call[retro.Metrics](t, cs, "review_metrics", map[string]any{"run_id": run})
	if m.State != "merged" || *m.TimeToFirstReviewHours != 4 || *m.DecisionLatencyHours != 30 || m.ReviewRounds != 2 || m.CommitsAfterFirstReview != 1 {
		t.Errorf("timing = %+v", m)
	}
	if m.Local.FindingsTotal != 2 || m.Local.FindingsPosted != 1 || m.Local.FindingsDisputed != 1 || *m.AIAcceptanceRate != 0.5 || m.Comments.AIAssisted != 1 {
		t.Errorf("local = %+v", m.Local)
	}

	if msg := callErr(t, cs, "retro_prepare", map[string]any{"run_id": run}); !strings.Contains(msg, "no retro-summarizer report") {
		t.Errorf("retro without report: %s", msg)
	}
	tok := call[SlotOut](t, cs, "report_slot", map[string]any{"run_id": run, "agent_name": "retro-summarizer", "stage": "retro"}).Token
	rep := fmt.Sprintf(`{"agent":"retro-summarizer","version":1,"head_sha":%q,"status":"ok","summary":"Merged after one round; the retry backoff concern was raised by aireviewer and fixed.","claims":[
		{"id":"T1","statement":"Half of the AI findings were posted; the other was disputed as intended behaviour.","category":"risk","severity":"info","confidence":"high",
		 "evidence":[{"kind":"metric","source":"review_metrics","name":"ai_acceptance_rate","value":0.5}]}]}`, head)
	if s := call[SubmitOut](t, cs, "report_submit", map[string]any{"run_id": run, "token": tok, "stage": "retro", "report": rep,
		"markdown": "Merged after one round of review."}); s.Kept != 1 {
		t.Fatalf("retro report = %+v", s)
	}
	rp := call[ProposalOut](t, cs, "retro_prepare", map[string]any{"run_id": run})
	for _, want := range []string{"Review retrospective", "Merged after one round", "| Time to first review | 4.0 h |", "| AI findings posted / disputed | 1 / 1 of 2 (acceptance 50%) |"} {
		if !strings.Contains(rp.Preview, want) {
			t.Errorf("preview lacks %q:\n%s", want, rp.Preview)
		}
	}
	sub := call[SubmitProposalOut](t, cs, "proposal_submit", map[string]any{"run_id": run, "proposal_id": rp.ProposalID, "hash": rp.Hash})
	if !sub.Posted || gp.marker == nil || !strings.HasPrefix(gp.marker.Body, retroMarker) {
		t.Fatalf("retro post = %+v marker = %+v", sub, gp.marker)
	}

	ms := call[MemorySaveOut](t, cs, "memory_save", map[string]any{"run_id": run})
	if ms.Path == "" {
		t.Fatal("memory not saved")
	}
	q := call[MemoryQueryOut](t, cs, "memory_query", map[string]any{"run_id": run, "files": []string{"svc/svc.go"}})
	if len(q.Hits) != 1 || q.Hits[0].Entry.Number != 3 || !strings.Contains(strings.Join(q.Hits[0].Entry.Decisions, " "), "disputed") {
		t.Errorf("query = %+v", q)
	}
	if msg := callErr(t, cs, "memory_forget", map[string]any{"run_id": run}); !strings.Contains(msg, "all: true") {
		t.Errorf("unscoped forget: %s", msg)
	}
	if f := call[MemoryForgetOut](t, cs, "memory_forget", map[string]any{"run_id": run, "number": 3}); f.Erased != 1 {
		t.Errorf("forget = %+v", f)
	}
}
