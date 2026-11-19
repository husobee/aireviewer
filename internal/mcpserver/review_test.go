// SPDX-License-Identifier: MIT

package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/gitrepo"
	"github.com/husobee/aireviewer/internal/runctx"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

const svcBase = "package svc\n\nfunc Retry(n int) int {\n\treturn n\n}\n"
const svcHead = "package svc\n\nfunc Retry(n int) int {\n\tfor i := 0; i < n; i++ {\n\t}\n\treturn n\n}\n\nfunc New() {}\n"

type prProvider struct{ head string }

func (p prProvider) Kind() forge.Kind { return forge.GitHub }
func (p prProvider) GetPR(_ context.Context, ref forge.PRRef) (*forge.PR, error) {
	return &forge.PR{Ref: ref, Noun: "pull request", Title: "retry loop", HeadSHA: p.head, BaseBranch: "main", State: forge.StateOpen}, nil
}
func (p prProvider) GetDiff(context.Context, forge.PRRef) (*forge.Diff, error) {
	return &forge.Diff{}, nil
}

type prSource struct{ p prProvider }

func (s prSource) Get(context.Context, forge.Repo) (forge.Provider, error) { return s.p, nil }

func reviewServer(t *testing.T) (*mcp.ClientSession, string, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	src := t.TempDir()
	gitT(t, src, "init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(src, "svc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "svc", "svc.go"), []byte(svcBase), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, src, "add", "-A")
	gitT(t, src, "commit", "-q", "-m", "base")
	gitT(t, src, "checkout", "-q", "-b", "feat")
	if err := os.WriteFile(filepath.Join(src, "svc", "svc.go"), []byte(svcHead), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, src, "commit", "-q", "-am", "head")
	head := gitT(t, src, "rev-parse", "HEAD")
	gitT(t, src, "update-ref", "refs/pull/3/head", head)
	gitT(t, src, "checkout", "-q", "main")

	root := t.TempDir()
	srv := New(Config{
		Version: "test", RepoRoot: root, Store: runctx.New(root), CacheDir: t.TempDir(),
		Providers: prSource{prProvider{head: head}},
		RemoteFor: func(repo forge.Repo, tok string) gitrepo.Remote {
			return gitrepo.Remote{URL: "file://" + src, Kind: repo.Forge, Token: tok}
		},
	})
	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, root, head
}

func TestReviewFlow(t *testing.T) {
	cs, root, head := reviewServer(t)
	url := "https://github.com/o/r/pull/3"

	co := call[CheckoutOut](t, cs, "pr_checkout", map[string]any{"url": url})
	if co.HeadSHA != head || !strings.HasPrefix(co.Worktree, root) {
		t.Fatalf("checkout = %+v", co)
	}
	if b, _ := os.ReadFile(filepath.Join(root, ".aireviewer", ".gitignore")); !strings.Contains(string(b), "*") {
		t.Error("workspace .gitignore not written")
	}

	dm := call[DiffmapOut](t, cs, "diffmap_build", map[string]any{"run_id": co.RunID})
	if _, ok := dm.Map.Unit("unit:svc/svc.go#Retry"); !ok {
		t.Fatalf("Retry unit missing: %+v", dm.Map.Order)
	}
	if _, ok := dm.Map.Unit("unit:svc/svc.go#New"); !ok {
		t.Fatalf("New unit missing: %+v", dm.Map.Order)
	}

	report := fmt.Sprintf(`{"agent":"code-reviewer","version":1,"head_sha":%q,"status":"ok","summary":"one issue","claims":[
		{"id":"F1","statement":"loop body is empty","category":"defect","severity":"minor","confidence":"high",
		 "evidence":[{"kind":"code","file":"svc/svc.go","lines":[4,5]}],"anchors":["unit:svc/svc.go#Retry"]},
		{"id":"F2","statement":"hallucinated","category":"defect","severity":"major","confidence":"high",
		 "evidence":[{"kind":"code","file":"svc/svc.go","lines":[40,41]}]}]}`, head)
	sub := call[SubmitOut](t, cs, "report_submit", map[string]any{"run_id": co.RunID, "report": report})
	if !sub.Accepted || sub.Kept != 1 || len(sub.Dropped) != 1 || sub.Dropped[0].ClaimID != "F2" {
		t.Fatalf("submit = %+v", sub)
	}

	stale := call[SubmitOut](t, cs, "report_submit", map[string]any{"run_id": co.RunID,
		"report": `{"agent":"x","version":1,"head_sha":"0000000","status":"ok","summary":"s","claims":[]}`})
	if stale.Accepted || !strings.Contains(stale.Rejection, "stale") {
		t.Errorf("stale report = %+v", stale)
	}

	unit := call[UnitOut](t, cs, "diffmap_unit", map[string]any{"run_id": co.RunID, "unit_id": "unit:svc/svc.go#Retry"})
	if len(unit.Findings) != 1 || unit.Findings[0].Claim.ID != "F1" || !strings.Contains(unit.Rendered, "for i := 0") {
		t.Errorf("unit = %+v", unit)
	}
	got := call[DiffmapOut](t, cs, "diffmap_get", map[string]any{"run_id": co.RunID})
	if got.FindingCounts["unit:svc/svc.go#Retry"] != 1 {
		t.Errorf("finding counts = %v", got.FindingCounts)
	}

	// The summary may cite upstream claims; unknown sources are stripped.
	summary := fmt.Sprintf(`{"agent":"summary-generator","version":1,"head_sha":%q,"status":"ok","summary":"s","claims":[
		{"id":"S1","statement":"empty loop","category":"defect","severity":"minor","confidence":"high",
		 "evidence":[{"kind":"code","file":"svc/svc.go","lines":[4,5]}],"sources":["code-reviewer:F1","code-reviewer:F2"]}]}`, head)
	ss := call[SubmitOut](t, cs, "report_submit", map[string]any{"run_id": co.RunID, "report": summary})
	if !ss.Accepted || len(ss.Notes) != 1 {
		t.Errorf("summary submit = %+v", ss)
	}
	rs := call[ReportsOut](t, cs, "reports_get", map[string]any{"run_id": co.RunID})
	if len(rs.Reports) != 2 || rs.Reports[0].Agent != "code-reviewer" {
		t.Errorf("reports = %+v", rs)
	}

	ev := call[EvidenceOut](t, cs, "evidence_check", map[string]any{"run_id": co.RunID, "evidence": []map[string]any{
		{"kind": "code", "file": "svc/svc.go", "lines": []int{1, 3}, "side": "old"},
		{"kind": "code", "file": "svc/svc.go", "lines": []int{1, 9}, "side": "old"},
		{"kind": "metric", "name": "x", "value": 1, "source": "vibes"},
	}})
	if !ev.Results[0].OK || ev.Results[1].OK || ev.Results[2].OK {
		t.Errorf("evidence = %+v", ev)
	}

	var sch SchemaOut
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "report_schema", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("report_schema: %v %+v", err, res)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if json.Unmarshal(raw, &sch) != nil || !strings.Contains(fmt.Sprint(sch.Schema["$id"]), "report.v1") {
		t.Error("schema not returned")
	}
}
