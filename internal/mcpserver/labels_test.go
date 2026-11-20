// SPDX-License-Identifier: MIT

package mcpserver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/gitrepo"
	"github.com/husobee/aireviewer/internal/runctx"
)

type labelProvider struct {
	*gateProvider
	mu      sync.Mutex
	calls   int
	commits map[string]bool
	err     error
	labels  []string
}

func (l *labelProvider) LabelledFixCommits(_ context.Context, _ forge.Repo, labels []string, _, _ time.Time, _ int) (map[string]bool, int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	l.labels = labels
	return l.commits, len(l.commits), l.err
}

type labelSource struct{ p *labelProvider }

func (s labelSource) Get(context.Context, forge.Repo) (forge.Provider, error) { return s.p, nil }

func labelServer(t *testing.T, src string, lp *labelProvider, cache string) *mcp.ClientSession {
	t.Helper()
	root := t.TempDir()
	srv := New(Config{Version: "test", RepoRoot: root, Store: runctx.New(root), CacheDir: cache, Providers: labelSource{lp},
		RemoteFor: func(repo forge.Repo, tok string) gitrepo.Remote {
			return gitrepo.Remote{URL: "file://" + src, Kind: repo.Forge, Token: tok}
		}})
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestRepoMetricsBugLabels(t *testing.T) {
	src, head := runtimeRepo(t, "risk:\n  bug_labels: [bug, incident]\n")
	base := gitT(t, src, "rev-parse", "main") // subject "base": no fix keyword
	lp := &labelProvider{gateProvider: &gateProvider{prProvider: prProvider{head: head}}, commits: map[string]bool{base: true}}
	cache := t.TempDir()
	cs := labelServer(t, src, lp, cache)
	run := call[CheckoutOut](t, cs, "pr_checkout", map[string]any{"url": "https://github.com/o/r/pull/3"}).RunID
	call[DiffmapOut](t, cs, "diffmap_build", map[string]any{"run_id": run})

	rm := call[RepoMetricsOut](t, cs, "repo_metrics", map[string]any{"run_id": run})
	f := rm.Files["svc/svc.go"]
	if f == nil || f.Bugfix365 != 1 || f.BugfixLabel365 != 1 || f.BugfixKeyword365 != 0 {
		t.Fatalf("svc.go = %+v", f)
	}
	if !strings.Contains(rm.BugLabels, "1 PRs labelled bug/incident") || strings.Join(lp.labels, ",") != "bug,incident" {
		t.Errorf("note = %q labels = %v", rm.BugLabels, lp.labels)
	}
	ev := call[EvidenceOut](t, cs, "evidence_check", map[string]any{"run_id": run, "evidence": []map[string]any{
		{"kind": "metric", "source": "repo_metrics", "name": "svc/svc.go.bugfix_label_365d", "value": 1},
		{"kind": "metric", "source": "repo_metrics", "name": "repo.bugfix_label_only", "value": 1}}})
	if !ev.Results[0].OK || !ev.Results[1].OK {
		t.Errorf("label metrics = %+v", ev.Results)
	}

	// Cached for a day: a second run does not hit the forge again.
	rm = call[RepoMetricsOut](t, cs, "repo_metrics", map[string]any{"run_id": run})
	if lp.calls != 1 || !strings.Contains(rm.BugLabels, "cached") {
		t.Errorf("calls = %d, note = %q", lp.calls, rm.BugLabels)
	}

	// Forge unavailable: keywords only, stated.
	lp2 := &labelProvider{gateProvider: lp.gateProvider, err: errors.New("rate limited")}
	cs2 := labelServer(t, src, lp2, t.TempDir())
	run2 := call[CheckoutOut](t, cs2, "pr_checkout", map[string]any{"url": "https://github.com/o/r/pull/3"}).RunID
	call[DiffmapOut](t, cs2, "diffmap_build", map[string]any{"run_id": run2})
	rm = call[RepoMetricsOut](t, cs2, "repo_metrics", map[string]any{"run_id": run2})
	if !strings.Contains(rm.BugLabels, "keywords only") || rm.Files["svc/svc.go"].Bugfix365 != 0 {
		t.Errorf("fallback = %q %+v", rm.BugLabels, rm.Files["svc/svc.go"])
	}
}
