// SPDX-License-Identifier: MIT

package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/gitrepo"
	"github.com/husobee/aireviewer/internal/runctx"
)

// headlessServer is a CI-mode server: no elicitation, no human.
func headlessServer(t *testing.T, src string, gp *gateProvider) *mcp.ClientSession {
	t.Helper()
	root := t.TempDir()
	srv := New(Config{Version: "test", RepoRoot: root, Store: runctx.New(root), CacheDir: t.TempDir(), Headless: true,
		VoiceDir: t.TempDir(), MemoryDir: t.TempDir(), Sandbox: newFake(), Providers: gateSource{gp},
		RemoteFor: func(repo forge.Repo, tok string) gitrepo.Remote {
			return gitrepo.Remote{URL: "file://" + src, Kind: repo.Forge, Token: tok}
		}})
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "ci"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func augmentRun(t *testing.T, cs *mcp.ClientSession, head string) string {
	t.Helper()
	run := call[CheckoutOut](t, cs, "pr_checkout", map[string]any{"url": "https://github.com/o/r/pull/3"}).RunID
	call[DiffmapOut](t, cs, "diffmap_build", map[string]any{"run_id": run})
	sum := fmt.Sprintf(`{"agent":"summary-generator","version":1,"head_sha":%q,"status":"ok","summary":"overall","claims":[]}`, head)
	tok := call[SlotOut](t, cs, "report_slot", map[string]any{"run_id": run, "stage": "augmentation", "agent_name": "summary-generator"}).Token
	call[SubmitOut](t, cs, "report_submit", map[string]any{"run_id": run, "stage": "augmentation", "token": tok, "report": sum,
		"markdown": "## Augmentation summary\nRisk low."})
	return run
}

func TestHeadlessPostsOnlyOptedInSummary(t *testing.T) {
	src, head := runtimeRepo(t, runtimeYML+"ci:\n  post_augmentation: true\n")
	gp := &gateProvider{prProvider: prProvider{head: head}, author: "alice", user: "github-actions[bot]"}
	cs := headlessServer(t, src, gp)
	run := augmentRun(t, cs, head)

	pp := call[ProposalOut](t, cs, "sticky_prepare", map[string]any{"run_id": run})
	if strings.Contains(pp.Preview, "confirmed by") || !strings.Contains(pp.Preview, "Not reviewed by a human") {
		t.Errorf("CI footer must not claim human confirmation:\n%s", pp.Preview)
	}
	sub := call[SubmitProposalOut](t, cs, "proposal_submit", map[string]any{"run_id": run, "proposal_id": pp.ProposalID, "hash": pp.Hash})
	if !sub.Posted || sub.Confirmation != "ci_policy" || gp.marker == nil {
		t.Fatalf("summary not posted in CI: %+v", sub)
	}
	dg := call[DecisionsOut](t, cs, "decisions_get", map[string]any{"run_id": run})
	var actors []string
	for _, e := range dg.Entries {
		if e.Event == "human_confirmed" {
			actors = append(actors, e.Actor)
		}
	}
	if len(actors) != 1 || actors[0] != "ci_policy" {
		t.Errorf("confirmation actors = %v", actors)
	}

	// Everything else is refused, even when proposed.
	call[DraftOut](t, cs, "comment_draft_add", map[string]any{"run_id": run, "path": "svc/svc.go", "line": 9, "body": "Is New used?"})
	passChecks(t, cs, run, "C1")
	rp := call[ProposalOut](t, cs, "review_prepare", map[string]any{"run_id": run, "verdict": "comment", "body": "x", "comment_ids": []string{"C1"}})
	if msg := callErr(t, cs, "proposal_submit", map[string]any{"run_id": run, "proposal_id": rp.ProposalID, "hash": rp.Hash}); !strings.Contains(msg, "headless") {
		t.Errorf("review posted in CI: %s", msg)
	}
	if len(gp.reviews) != 0 {
		t.Fatal("a review reached the forge in CI")
	}
	if out := call[RuntimePrepareOut](t, cs, "runtime_prepare", map[string]any{"run_id": run}); out.Applicable || !strings.Contains(out.Reason, "headless") {
		t.Errorf("runtime in CI = %+v", out)
	}
	if msg := callErr(t, cs, "voice_set", map[string]any{"description": "x"}); !strings.Contains(msg, "headless") {
		t.Errorf("voice_set in CI: %s", msg)
	}
	if msg := callErr(t, cs, "memory_forget", map[string]any{"run_id": run, "all": true}); !strings.Contains(msg, "headless") {
		t.Errorf("memory_forget in CI: %s", msg)
	}
}

func TestHeadlessWithoutOptInPostsNothing(t *testing.T) {
	// The PR itself opts in; only the merge-base config counts, so CI
	// still posts nothing.
	src, head := runtimeRepoHead(t, runtimeYML, runtimeYML+"ci:\n  post_augmentation: true\n")
	gp := &gateProvider{prProvider: prProvider{head: head}, author: "alice", user: "github-actions[bot]"}
	cs := headlessServer(t, src, gp)
	run := augmentRun(t, cs, head)
	pp := call[ProposalOut](t, cs, "sticky_prepare", map[string]any{"run_id": run})
	if msg := callErr(t, cs, "proposal_submit", map[string]any{"run_id": run, "proposal_id": pp.ProposalID, "hash": pp.Hash}); !strings.Contains(msg, "ci.post_augmentation") {
		t.Errorf("summary posted without opt-in: %s", msg)
	}
	if gp.marker != nil {
		t.Fatal("comment reached the forge without opt-in")
	}
}
