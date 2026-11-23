// SPDX-License-Identifier: MIT

package mcpserver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/husobee/aireviewer/internal/changeset"
	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/gitrepo"
	"github.com/husobee/aireviewer/internal/runctx"
)

// prRepo builds a repository with main (base) and a PR head at refs/pull/<n>/head.
func prRepo(t *testing.T, base, head map[string]string, n int) (dir, headSHA string) {
	t.Helper()
	dir = t.TempDir()
	write := func(files map[string]string) {
		for p, c := range files {
			_ = os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755)
			if c == "" {
				_ = os.Remove(filepath.Join(dir, p))
				continue
			}
			if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	gitT(t, dir, "init", "-q", "-b", "main")
	write(base)
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "base")
	gitT(t, dir, "checkout", "-q", "-b", "feature")
	write(head)
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "change")
	headSHA = gitT(t, dir, "rev-parse", "HEAD")
	gitT(t, dir, "update-ref", fmt.Sprintf("refs/pull/%d/head", n), headSHA)
	gitT(t, dir, "checkout", "-q", "main")
	return dir, headSHA
}

// setProvider serves one PR per project.
type setProvider struct {
	prs map[string]*forge.PR
}

func (p setProvider) Kind() forge.Kind { return forge.GitHub }
func (p setProvider) GetPR(_ context.Context, ref forge.PRRef) (*forge.PR, error) {
	pr := *p.prs[ref.Project]
	pr.Ref = ref
	pr.WebURL = fmt.Sprintf("https://github.com/%s/pull/%d", ref.Project, ref.Number)
	return &pr, nil
}

// discoverProvider adds stack lookups for changeset_discover.
type discoverProvider struct{ setProvider }

func (d discoverProvider) ListOpenPRsByBase(_ context.Context, repo forge.Repo, base string) ([]*forge.PR, error) {
	if base != "feature" {
		return nil, nil
	}
	return []*forge.PR{{Ref: forge.PRRef{Repo: repo, Number: 9}, Title: "part 2", BaseBranch: "feature", WebURL: "https://github.com/o/api/pull/9"}}, nil
}
func (d discoverProvider) FindOpenPR(_ context.Context, repo forge.Repo, branch string) (*forge.PR, error) {
	if branch != "main" {
		return nil, nil
	}
	return &forge.PR{Ref: forge.PRRef{Repo: repo, Number: 3}, Title: "base work", WebURL: "https://github.com/o/api/pull/3"}, nil
}

type discoverSource struct{ p discoverProvider }

func (s discoverSource) Get(context.Context, forge.Repo) (forge.Provider, error) { return s.p, nil }

func TestChangesetDiscover(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	dir, head := prRepo(t, map[string]string{"a.go": "package a\n"}, map[string]string{"a.go": "package a\n\nfunc New() {}\n"}, 1)
	dp := discoverProvider{setProvider{prs: map[string]*forge.PR{"o/api": {Title: "API", HeadSHA: head, BaseBranch: "main", HeadBranch: "feature",
		State: forge.StateOpen, Body: "Depends on #7.\nClient side: https://github.com/o/client/pull/2 (and ignore https://example.com/x)"}}}}
	root := t.TempDir()
	srv := New(Config{Version: "test", RepoRoot: root, Store: runctx.New(root), CacheDir: t.TempDir(), Providers: discoverSource{dp},
		RemoteFor: func(repo forge.Repo, tok string) gitrepo.Remote {
			return gitrepo.Remote{URL: "file://" + dir, Kind: repo.Forge}
		}})
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	run := call[CheckoutOut](t, cs, "pr_checkout", map[string]any{"url": "https://github.com/o/api/pull/1"}).RunID
	d := call[DiscoverOut](t, cs, "changeset_discover", map[string]any{"run_id": run})
	got := map[string]string{}
	for _, s := range d.Suggestions {
		got[s.Ref] = s.Reason
	}
	if len(got) != 4 || !strings.Contains(got["o/api#9"], "targets this PR's branch") || !strings.Contains(got["o/api#3"], "this PR targets its branch") ||
		!strings.Contains(got["o/client#2"], "description") || !strings.Contains(got["o/api#7"], "Depends on #7") {
		t.Errorf("suggestions = %+v", d.Suggestions)
	}
}
func (p setProvider) GetDiff(context.Context, forge.PRRef) (*forge.Diff, error) {
	return &forge.Diff{}, nil
}

type setSource struct{ p setProvider }

func (s setSource) Get(context.Context, forge.Repo) (forge.Provider, error) { return s.p, nil }

func TestChangesetFlow(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	apiDir, apiHead := prRepo(t,
		map[string]string{"api/api.go": "package api\n\nfunc Ping() {}\n\nfunc Limits() int {\n\treturn 1\n}\n"},
		map[string]string{"api/api.go": "package api\n\nfunc Limits() int {\n\treturn 5\n}\n\nfunc GetV2() string {\n\treturn \"v2\"\n}\n"}, 1)
	clientDir, clientHead := prRepo(t,
		map[string]string{"client/client.go": "package client\n\nfunc Run() {\n\tapi.Ping()\n\t_ = api.Limits()\n}\n"},
		map[string]string{"client/client.go": "package client\n\nfunc Run() {\n\tapi.Ping()\n\t_ = api.Limits()\n\t_ = api.GetV2()\n}\n"}, 2)
	sp := setProvider{prs: map[string]*forge.PR{
		"o/api":    {Title: "API v2", HeadSHA: apiHead, BaseBranch: "main", HeadBranch: "feature", State: forge.StateOpen, Author: "ann"},
		"o/client": {Title: "Use API v2", HeadSHA: clientHead, BaseBranch: "main", HeadBranch: "feature", State: forge.StateOpen, Author: "bob"},
	}}
	root := t.TempDir()
	srv := New(Config{Version: "test", RepoRoot: root, Store: runctx.New(root), CacheDir: t.TempDir(), Providers: setSource{sp},
		RemoteFor: func(repo forge.Repo, tok string) gitrepo.Remote {
			src := apiDir
			if repo.Project == "o/client" {
				src = clientDir
			}
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
	defer func() { _ = cs.Close() }()

	if msg := callErr(t, cs, "changeset_open", map[string]any{"urls": []string{"https://github.com/o/api/pull/1"}}); !strings.Contains(msg, "2-8") {
		t.Errorf("single PR: %s", msg)
	}
	op := call[ChangesetOut](t, cs, "changeset_open", map[string]any{"name": "api v2",
		"urls": []string{"https://github.com/o/api/pull/1", "https://github.com/o/client/pull/2"}})
	set := op.RunID
	if len(op.Set.Members) != 2 || !strings.HasPrefix(set, "changeset-") || op.Set.Key == "" {
		t.Fatalf("open = %+v", op)
	}
	apiRun, clientRun := op.Set.Members[0].RunID, op.Set.Members[1].RunID

	mp := call[MapOut](t, cs, "changeset_map", map[string]any{"run_id": set})
	byChange := map[string]changeset.Edge{}
	for _, e := range mp.Edges {
		if e.From == apiRun && e.To == clientRun {
			byChange[e.Change+":"+e.Symbol] = e
		}
	}
	if e, ok := byChange["added:GetV2"]; !ok || !e.UsedAtHead || e.UsedAtBase || e.Uses[0].File != "client/client.go" {
		t.Errorf("GetV2 edge = %+v (all %+v)", e, mp.Edges)
	}
	if e, ok := byChange["deleted:Ping"]; !ok || !e.UsedAtHead {
		t.Errorf("Ping edge = %+v", e)
	}
	if _, ok := byChange["modified:Limits"]; !ok {
		t.Errorf("Limits edge missing: %+v", mp.Edges)
	}

	ord := call[OrderOut](t, cs, "changeset_order", map[string]any{"run_id": set})
	if len(ord.Constraints) != 1 || ord.Constraints[0].Before != apiRun || ord.Constraints[0].Kind != "introduces" {
		t.Errorf("constraints = %+v", ord.Constraints)
	}
	if len(ord.Contracts) != 1 || len(ord.Problems) != 1 || ord.Problems[0].Kind != "incompatible" ||
		!strings.Contains(ord.Problems[0].Detail, "o/client#2 still uses Ping") {
		t.Errorf("order = %+v", ord)
	}
	if len(ord.SequenceRefs) != 2 || ord.SequenceRefs[0] != "o/api#1" {
		t.Errorf("sequence = %v", ord.SequenceRefs)
	}

	// The set-level report: evidence names its member; anchors are qualified.
	tok := call[SlotOut](t, cs, "report_slot", map[string]any{"run_id": set, "stage": "changeset", "agent_name": "changeset-analyst"}).Token
	rep := fmt.Sprintf(`{"agent":"changeset-analyst","version":1,"head_sha":%q,"status":"ok","summary":"api must merge first; Ping removal breaks the client","claims":[
		{"id":"S1","statement":"o/client#2 still calls Ping, which o/api#1 removes","category":"interface","severity":"blocker","confidence":"high",
		 "evidence":[{"kind":"code","file":"client/client.go","lines":[4,4],"member":%q},{"kind":"metric","source":"changeset_order","name":"problems.incompatible","value":1}],
		 "anchors":["unit:github.com/o/api:api/api.go#Ping"]},
		{"id":"S2","statement":"no member named","category":"interface","severity":"minor","confidence":"low",
		 "evidence":[{"kind":"code","file":"client/client.go","lines":[4,4]}]}]}`, op.Set.Key, clientRun)
	sub := call[SubmitOut](t, cs, "report_submit", map[string]any{"run_id": set, "stage": "changeset", "token": tok, "report": rep})
	if !sub.Accepted || sub.Kept != 1 || len(sub.Dropped) != 1 {
		t.Fatalf("set report = %+v", sub)
	}
	rs := call[ReportsOut](t, cs, "reports_get", map[string]any{"run_id": set, "stage": "changeset"})
	if len(rs.Reports) != 1 || len(rs.Reports[0].Claims[0].Anchors) != 1 {
		t.Errorf("stored = %+v", rs.Reports)
	}

	// Posting happens per member, never on the set run.
	if msg := callErr(t, cs, "sticky_prepare", map[string]any{"run_id": set}); msg == "" {
		t.Error("sticky on a set run")
	}

	// A member moves: the set is stale.
	sp.prs["o/client"].HeadSHA = strings.Repeat("e", 40)
	if g := call[ChangesetOut](t, cs, "changeset_get", map[string]any{"run_id": set}); !g.Stale || !strings.Contains(strings.Join(g.Notes, " "), "o/client#2 moved") {
		t.Errorf("stale = %+v", g)
	}
}
