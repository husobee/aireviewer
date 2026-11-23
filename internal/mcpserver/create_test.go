// SPDX-License-Identifier: MIT

package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/gitrepo"
	"github.com/husobee/aireviewer/internal/runctx"
)

// createProvider fakes the forge side of /pr-create and records writes.
type createProvider struct {
	mu       sync.Mutex
	open     *forge.PR
	issues   map[int]forge.Issue
	created  []forge.NewIssue
	prs      []forge.PRSpec
	failPRs  int
	searched []string
}

func (c *createProvider) Kind() forge.Kind { return forge.GitHub }
func (c *createProvider) GetPR(_ context.Context, ref forge.PRRef) (*forge.PR, error) {
	return &forge.PR{Ref: ref}, nil
}
func (c *createProvider) GetDiff(context.Context, forge.PRRef) (*forge.Diff, error) {
	return &forge.Diff{}, nil
}
func (c *createProvider) CurrentUser(context.Context, forge.Repo) (string, error) { return "ada", nil }
func (c *createProvider) DefaultBranch(context.Context, forge.Repo) (string, error) {
	return "main", nil
}
func (c *createProvider) FindOpenPR(context.Context, forge.Repo, string) (*forge.PR, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.open, nil
}
func (c *createProvider) SearchIssues(_ context.Context, _ forge.Repo, q forge.IssueSearch) ([]forge.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.searched = append(c.searched, q.Query)
	return []forge.Issue{c.issues[7], c.issues[8]}, nil
}
func (c *createProvider) GetIssue(_ context.Context, _ forge.Repo, n int) (*forge.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	is, ok := c.issues[n]
	if !ok {
		return nil, forge.ErrNotFound
	}
	return &is, nil
}
func (c *createProvider) CreateIssue(_ context.Context, _ forge.Repo, ni forge.NewIssue) (*forge.Issue, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.created = append(c.created, ni)
	return &forge.Issue{Ref: "o/r#42", Number: 42, Title: ni.Title, URL: "https://github.com/o/r/issues/42"}, nil
}
func (c *createProvider) CreatePR(_ context.Context, repo forge.Repo, spec forge.PRSpec) (*forge.PR, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.failPRs > 0 {
		c.failPRs--
		return nil, errors.New("502 bad gateway")
	}
	c.prs = append(c.prs, spec)
	c.open = &forge.PR{Ref: forge.PRRef{Repo: repo, Number: 5}, Title: spec.Title, Body: spec.Body, WebURL: "https://github.com/o/r/pull/5"}
	return c.open, nil
}

type createSource struct{ p *createProvider }

func (s createSource) Get(context.Context, forge.Repo) (forge.Provider, error) { return s.p, nil }

// authorWorkspace is the author's clone (the MCP server's RepoRoot) with
// branch feat one commit ahead of main. Its origin is the GitHub URL, which
// git rewrites to a local bare repository standing in for the forge.
func authorWorkspace(t *testing.T) (dir, bare, head string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	bare = t.TempDir()
	gitT(t, bare, "init", "-q", "--bare", "-b", "main")
	dir = t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	gitT(t, dir, "config", "user.name", "Ada")
	gitT(t, dir, "config", "user.email", "ada@example.com")
	gitT(t, dir, "remote", "add", "origin", "https://github.com/o/r.git")
	gitT(t, dir, "config", "url."+bare+".insteadOf", "https://github.com/o/r.git")
	for p, c := range map[string]string{"svc/svc.go": svcBase, ".github/pull_request_template.md": "## Why\n\n## Testing\n"} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "base")
	gitT(t, dir, "push", "-q", "origin", "main")
	gitT(t, dir, "checkout", "-q", "-b", "feat")
	if err := os.WriteFile(filepath.Join(dir, "svc/svc.go"), []byte(svcHead), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "commit", "-q", "-am", "svc: retry loop and constructor")
	return dir, bare, gitT(t, dir, "rev-parse", "HEAD")
}

func createServer(t *testing.T, dir, bare string, cp *createProvider) *mcp.ClientSession {
	t.Helper()
	return createServerWith(t, dir, bare, createSource{cp})
}

func createServerWith(t *testing.T, dir, bare string, ps ProviderSource) *mcp.ClientSession {
	t.Helper()
	srv := New(Config{
		Version: "test", RepoRoot: dir, Store: runctx.New(dir), CacheDir: t.TempDir(),
		Hosts: forge.HostConfigFromEnv(), Providers: ps,
		RemoteFor: func(repo forge.Repo, tok string) gitrepo.Remote {
			return gitrepo.Remote{URL: bare, Kind: repo.Forge, Token: tok}
		},
	})
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, elicit("accept", true)).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestCreateFlow(t *testing.T) {
	dir, bare, head := authorWorkspace(t)
	cp := &createProvider{issues: map[int]forge.Issue{
		7: {Ref: "o/r#7", Number: 7, Title: "Retry should back off", Body: "Acceptance: retries wait between attempts", State: "open", URL: "u7"},
		8: {Ref: "o/r#8", Number: 8, Title: "Docs typo", State: "open", URL: "u8"},
	}, failPRs: 1}
	cs := createServer(t, dir, bare, cp)

	co := call[BranchCheckoutOut](t, cs, "branch_checkout", map[string]any{})
	if co.RunID == "" || co.Branch != "feat" || co.BaseBranch != "main" || co.HeadSHA != head || co.Ahead != 1 || co.ExistingPR != nil {
		t.Fatalf("checkout = %+v", co)
	}
	run := co.RunID
	call[DiffmapOut](t, cs, "diffmap_build", map[string]any{"run_id": run})
	cc := call[CreateContextOut](t, cs, "create_context", map[string]any{"run_id": run})
	if len(cc.Commits) != 1 || cc.Commits[0].Subject != "svc: retry loop and constructor" || !strings.Contains(cc.Template, "## Testing") || cc.Stats.Files != 1 {
		t.Errorf("context = %+v", cc)
	}
	if msg := callErr(t, cs, "review_prepare", map[string]any{"run_id": run, "verdict": "comment", "body": "x", "comment_ids": []string{}}); !strings.Contains(msg, "no PR exists yet") {
		t.Errorf("review tools on a branch run: %s", msg)
	}

	is := call[IssuesSearchOut](t, cs, "issues_search", map[string]any{"run_id": run, "queries": []string{"retry backoff"}})
	if len(is.Candidates) != 2 || is.Threshold != 0.7 {
		t.Errorf("search = %+v", is)
	}

	// Author-mode review finding plus a validated fix.
	rep := fmt.Sprintf(`{"agent":"code-reviewer","version":1,"head_sha":%q,"status":"ok","summary":"s","claims":[
		{"id":"F1","statement":"loop body is empty","category":"defect","severity":"major","confidence":"high",
		 "evidence":[{"kind":"code","file":"svc/svc.go","lines":[4,5]}],"anchors":["unit:svc/svc.go#Retry"]}]}`, head)
	if s := call[SubmitOut](t, cs, "report_submit", map[string]any{"run_id": run, "token": slot(t, cs, run, "code-reviewer"), "report": rep}); s.Kept != 1 {
		t.Fatalf("report = %+v", s)
	}
	fx := call[FixOut](t, cs, "fix_submit", map[string]any{"run_id": run, "agent_name": "fix-suggester-1", "token": slot(t, cs, run, "fix-suggester-1"),
		"finding": "code-reviewer:F1", "patch": fixPatch, "modifies": "adds a backoff call", "leaves_intact": "loop bounds"}).Fix

	if msg := callErr(t, cs, "create_prepare", map[string]any{"run_id": run}); !strings.Contains(msg, "no title") {
		t.Errorf("untitled draft: %s", msg)
	}
	if msg := callErr(t, cs, "create_draft_update", map[string]any{"run_id": run, "issues": []map[string]any{{"number": 99, "score": 0.9}}}); !strings.Contains(msg, "#99") {
		t.Errorf("unknown issue: %s", msg)
	}
	d := call[CreateDraftOut](t, cs, "create_draft_update", map[string]any{"run_id": run,
		"title": "Add backoff to the retry loop", "body": "## Why\nRetries hammer upstream.",
		"issues":       []map[string]any{{"number": 7, "score": 0.91, "rationale": "same symptom"}, {"number": 8, "score": 0.2}},
		"new_issue":    map[string]any{"title": "Track retry metrics", "body": "Follow-up."},
		"accept_fixes": []string{fx.ID}})
	if len(d.Issues) != 2 || !d.Issues[0].Link || d.Issues[1].Link || len(d.Fixes) != 1 || !d.Fixes[0].Accepted {
		t.Fatalf("draft = %+v", d)
	}
	if io := call[IssuesOut](t, cs, "pr_issues", map[string]any{"run_id": run}); len(io.Issues) != 1 || io.Issues[0].Number != 7 {
		t.Errorf("linked issues for alignment = %+v", io)
	}

	pp := call[ProposalOut](t, cs, "create_prepare", map[string]any{"run_id": run})
	for _, want := range []string{"Commit 1 accepted fix", "Create issue \"Track retry metrics\"", "Push `feat` to remote `origin`",
		"Fixes #7", "Fixes #<new issue>", "+\t\tbackoff(i)", "confirmed by @ada"} {
		if !strings.Contains(pp.Preview, want) {
			t.Errorf("preview lacks %q:\n%s", want, pp.Preview)
		}
	}
	if strings.Contains(pp.Preview, "#8") {
		t.Errorf("below-threshold issue linked:\n%s", pp.Preview)
	}

	// First attempt: commit, issue and push succeed, opening the PR fails.
	msg := callErr(t, cs, "proposal_submit", map[string]any{"run_id": run, "proposal_id": pp.ProposalID, "hash": pp.Hash})
	if !strings.Contains(msg, "already done: committed fixes") || !strings.Contains(msg, "issue #42") || !strings.Contains(msg, "502") {
		t.Fatalf("partial failure: %s", msg)
	}
	committed := gitT(t, dir, "rev-parse", "refs/heads/feat")
	if committed == head || gitT(t, bare, "rev-parse", "refs/heads/feat") != committed {
		t.Fatalf("commit/push not done: local %s bare %s", committed, gitT(t, bare, "rev-parse", "refs/heads/feat"))
	}

	// Retry resumes: no second commit, issue or push; the PR opens.
	sub := call[SubmitProposalOut](t, cs, "proposal_submit", map[string]any{"run_id": run, "proposal_id": pp.ProposalID, "hash": pp.Hash})
	if !sub.Posted || sub.Result.URL != "https://github.com/o/r/pull/5" {
		t.Fatalf("submit = %+v", sub)
	}
	if len(cp.created) != 1 || len(cp.prs) != 1 || gitT(t, dir, "rev-parse", "refs/heads/feat") != committed {
		t.Errorf("not idempotent: issues=%d prs=%d", len(cp.created), len(cp.prs))
	}
	spec := cp.prs[0]
	if spec.Head != "feat" || spec.Base != "main" || !strings.Contains(spec.Body, "Fixes #7\nFixes #42") || !strings.Contains(spec.Body, "aireviewer") {
		t.Errorf("PR spec = %+v", spec)
	}
	if show := gitT(t, bare, "show", "feat:svc/svc.go"); !strings.Contains(show, "backoff(i)") {
		t.Errorf("pushed content lacks the fix:\n%s", show)
	}
	if m := gitT(t, dir, "log", "-1", "--format=%s"); m != "aireviewer: apply review fixes" {
		t.Errorf("commit subject = %q", m)
	}

	dg := call[DecisionsOut](t, cs, "decisions_get", map[string]any{"run_id": run})
	var events []string
	for _, e := range dg.Entries {
		events = append(events, e.Event)
	}
	joined := strings.Join(events, ",")
	for _, want := range []string{"branch_checked_out", "draft_updated", "proposal_created", "human_confirmed", "fixes_committed", "issue_created", "pushed", "post_failed", "pr_created", "posted"} {
		if !strings.Contains(joined, want) {
			t.Errorf("log lacks %s: %v", want, events)
		}
	}
	if !dg.Valid {
		t.Errorf("log invalid: %s", dg.Problem)
	}

	// Now that a PR exists, preflight skips creation.
	again := call[BranchCheckoutOut](t, cs, "branch_checkout", map[string]any{"branch": "feat"})
	if again.ExistingPR == nil || again.RunID != "" {
		t.Errorf("existing PR not detected: %+v", again)
	}
}

func TestCreateRefusesMovedBranch(t *testing.T) {
	dir, bare, _ := authorWorkspace(t)
	cp := &createProvider{issues: map[int]forge.Issue{}}
	cs := createServer(t, dir, bare, cp)
	run := call[BranchCheckoutOut](t, cs, "branch_checkout", map[string]any{}).RunID
	call[CreateDraftOut](t, cs, "create_draft_update", map[string]any{"run_id": run, "title": "t"})
	pp := call[ProposalOut](t, cs, "create_prepare", map[string]any{"run_id": run})

	// The author commits after preparing: nothing may be pushed.
	if err := os.WriteFile(filepath.Join(dir, "svc/extra.go"), []byte("package svc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-q", "-m", "more")
	msg := callErr(t, cs, "proposal_submit", map[string]any{"run_id": run, "proposal_id": pp.ProposalID, "hash": pp.Hash})
	if !strings.Contains(msg, "nothing was pushed") {
		t.Errorf("moved branch: %s", msg)
	}
	if out := gitT(t, bare, "branch", "--list", "feat"); out != "" {
		t.Errorf("branch was pushed: %q", out)
	}
	if msg := callErr(t, cs, "create_prepare", map[string]any{"run_id": run}); !strings.Contains(msg, "moved since branch_checkout") {
		t.Errorf("prepare on moved branch: %s", msg)
	}
	if len(cp.prs) != 0 {
		t.Error("PR opened")
	}

	// Nothing ahead of the base: refused.
	gitT(t, dir, "checkout", "-q", "-b", "empty", "main")
	if msg := callErr(t, cs, "branch_checkout", map[string]any{}); !strings.Contains(msg, "no commits") {
		t.Errorf("empty branch: %s", msg)
	}
}
