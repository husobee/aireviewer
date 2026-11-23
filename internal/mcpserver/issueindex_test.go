// SPDX-License-Identifier: MIT

package mcpserver

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/husobee/aireviewer/internal/forge"
)

// indexProvider adds incremental issue listing to the create fake.
type indexProvider struct {
	*createProvider
	updated map[int]time.Time
	listed  int
}

func (p *indexProvider) ListIssuesUpdatedSince(_ context.Context, _ forge.Repo, since time.Time, page int) ([]forge.Issue, []time.Time, []*time.Time, bool, error) {
	if page > 1 {
		return nil, nil, nil, false, nil
	}
	var is []forge.Issue
	var up []time.Time
	var cl []*time.Time
	for n, t := range p.updated {
		if t.After(since) {
			is, up, cl = append(is, p.issues[n]), append(up, t), append(cl, nil)
		}
	}
	p.listed += len(is)
	return is, up, cl, false, nil
}

type indexSource struct{ p *indexProvider }

func (s indexSource) Get(context.Context, forge.Repo) (forge.Provider, error) { return s.p, nil }

func TestIssueIndexInCreateFlow(t *testing.T) {
	dir, bare, _ := authorWorkspace(t)
	now := time.Now()
	cp := &createProvider{issues: map[int]forge.Issue{
		7: {Ref: "o/r#7", Number: 7, Title: "Retry should back off", Body: "Retries hammer the upstream without waiting.", State: "open", URL: "u7"},
		8: {Ref: "o/r#8", Number: 8, Title: "Docs typo", Body: "README.", State: "open", URL: "u8"},
		9: {Ref: "o/r#9", Number: 9, Title: "Add metrics endpoint", Body: "Expose counters.", State: "open", URL: "u9"},
	}}
	ip := &indexProvider{createProvider: cp, updated: map[int]time.Time{7: now.Add(-48 * time.Hour), 8: now.Add(-24 * time.Hour), 9: now.Add(-time.Hour)}}
	cs := createServerWith(t, dir, bare, indexSource{ip})
	run := call[BranchCheckoutOut](t, cs, "branch_checkout", map[string]any{}).RunID

	before := call[IssuesSearchOut](t, cs, "issues_search", map[string]any{"run_id": run, "queries": []string{"retry backoff"}})
	if before.Source != "forge" || len(cp.searched) != 1 {
		t.Errorf("before sync: source %s, forge searches %d", before.Source, len(cp.searched))
	}

	sync := call[IndexSyncOut](t, cs, "issues_index_sync", map[string]any{"run_id": run})
	if !sync.Full || sync.Fetched != 3 || sync.Issues != 3 {
		t.Fatalf("sync = %+v", sync)
	}
	after := call[IssuesSearchOut](t, cs, "issues_search", map[string]any{"run_id": run, "queries": []string{"retry backoff", "upstream waiting"}})
	if after.Source != "index" || len(after.Candidates) == 0 || after.Candidates[0].Number != 7 || after.BM25["#7"] <= 0 {
		t.Fatalf("after sync = %+v", after)
	}
	if len(cp.searched) != 1 {
		t.Error("forge search used although the index exists")
	}
	if !strings.Contains(strings.Join(after.Notes, " "), "BM25") {
		t.Errorf("notes = %v", after.Notes)
	}
	// The candidate is usable downstream (verified against the forge).
	d := call[CreateDraftOut](t, cs, "create_draft_update", map[string]any{"run_id": run, "issues": []map[string]any{{"number": 7, "score": 0.9}}})
	if len(d.Issues) != 1 || !d.Issues[0].Link {
		t.Errorf("draft = %+v", d)
	}

	again := call[IndexSyncOut](t, cs, "issues_index_sync", map[string]any{"run_id": run})
	if again.Full || again.Fetched != 0 || ip.listed != 3 {
		t.Errorf("incremental sync refetched: %+v (listed %d)", again, ip.listed)
	}
}
