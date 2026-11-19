// SPDX-License-Identifier: MIT

package mcpserver

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/runctx"
)

type fakeProvider struct{ kind forge.Kind }

func (f fakeProvider) Kind() forge.Kind { return f.kind }
func (f fakeProvider) GetPR(_ context.Context, ref forge.PRRef) (*forge.PR, error) {
	return &forge.PR{Ref: ref, Noun: f.kind.Noun(), Title: "t", HeadSHA: "head123", State: forge.StateOpen}, nil
}
func (f fakeProvider) GetDiff(context.Context, forge.PRRef) (*forge.Diff, error) {
	d := &forge.Diff{Files: []forge.FileChange{
		{Path: "a.go", Status: forge.FileModified, Additions: 2, Deletions: 1, Patch: "@@ -1 +1,2 @@\n-x\n+y\n+z"},
		{Path: "b.go", Status: forge.FileAdded, Additions: 5, Patch: "@@ -0,0 +1,5 @@"},
	}}
	d.ComputeStats()
	return d, nil
}

type fakeSource struct{}

func (fakeSource) Get(_ context.Context, repo forge.Repo) (forge.Provider, error) {
	return fakeProvider{kind: repo.Forge}, nil
}

func connect(t *testing.T) (*mcp.ClientSession, *runctx.Store) {
	t.Helper()
	ctx := context.Background()
	store := runctx.New(t.TempDir())
	srv := New(Config{Version: "test", RepoRoot: t.TempDir(), Providers: fakeSource{}, Store: store})
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, store
}

func call[T any](t *testing.T, cs *mcp.ClientSession, name string, args any) T {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("%s returned error: %+v", name, res.Content)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v (%s)", name, err, raw)
	}
	return out
}

func TestToolsRegistered(t *testing.T) {
	cs, _ := connect(t)
	var names []string
	for tool, err := range cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
	}
	for _, want := range []string{"forge_detect", "pr_resolve", "pr_get", "pr_diff", "run_context_get", "run_context_put", "run_context_list"} {
		if !slices.Contains(names, want) {
			t.Errorf("tool %s not registered (have %v)", want, names)
		}
	}
}

func TestPRFlowGitLab(t *testing.T) {
	cs, store := connect(t)
	url := map[string]any{"url": "https://gitlab.com/grp/proj/-/merge_requests/7"}

	r := call[ResolveOut](t, cs, "pr_resolve", url)
	if r.Abbrev != "MR" || r.Short != "grp/proj!7" {
		t.Errorf("resolve = %+v", r)
	}

	g := call[PRGetOut](t, cs, "pr_get", url)
	if g.PR.HeadSHA != "head123" || g.Noun != "merge request" {
		t.Errorf("pr_get = %+v", g)
	}
	if _, err := store.Get(r.RunID, "context.json"); err != nil {
		t.Errorf("context.json not saved: %v", err)
	}

	d := call[PRDiffOut](t, cs, "pr_diff", url)
	if d.Stats.Files != 2 || d.Stats.Additions != 7 || d.HeadSHA != "head123" {
		t.Errorf("pr_diff = %+v", d)
	}
	for _, f := range d.Files {
		if f.Patch != "" {
			t.Errorf("patch returned without include_patches: %s", f.Path)
		}
	}
	saved, _ := store.Get(r.RunID, "diff.json")
	var savedDiff struct {
		HeadSHA string             `json:"head_sha"`
		Files   []forge.FileChange `json:"files"`
	}
	if err := json.Unmarshal(saved, &savedDiff); err != nil || savedDiff.HeadSHA != "head123" || savedDiff.Files[0].Patch == "" {
		t.Errorf("diff.json not saved with full patches: %v %+v", err, savedDiff)
	}

	d2 := call[PRDiffOut](t, cs, "pr_diff", map[string]any{"url": url["url"], "include_patches": true, "paths": []string{"a.go"}, "max_patch_bytes": 5})
	if len(d2.Files) != 1 || !d2.Files[0].Truncated || d2.Stats.Files != 2 {
		t.Errorf("filtered diff = %+v", d2)
	}
}

func TestRunContextRoundTrip(t *testing.T) {
	cs, _ := connect(t)
	call[RunPutOut](t, cs, "run_context_put", map[string]any{"run_id": "r1", "path": "review/x/findings.json", "content": "[]"})
	got := call[RunGetOut](t, cs, "run_context_get", map[string]any{"run_id": "r1", "path": "review/x/findings.json"})
	if got.Content != "[]" {
		t.Errorf("content = %q", got.Content)
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "run_context_put",
		Arguments: map[string]any{"run_id": "r1", "path": "../../etc/x", "content": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Error("path traversal was accepted")
	}
}
