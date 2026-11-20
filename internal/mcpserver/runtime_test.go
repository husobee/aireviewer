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
	"github.com/husobee/aireviewer/internal/sandbox"
)

// fakeRunner scripts container behaviour per side, command and attempt, and
// records every spec so the isolation settings can be asserted.
type fakeRunner struct {
	mu       sync.Mutex
	unavail  error
	side     map[string]string // work volume -> side
	attempts map[string]int
	specs    []sandbox.Spec
	volumes  map[string]bool
	script   func(side, cmd string, attempt int) (int, string)
}

func (f *fakeRunner) Available(context.Context) error           { return f.unavail }
func (f *fakeRunner) EnsureImage(context.Context, string) error { return nil }
func (f *fakeRunner) CreateVolume(_ context.Context, n string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumes[n] = true
	return nil
}
func (f *fakeRunner) RemoveVolume(_ context.Context, n string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.volumes, n)
}
func (f *fakeRunner) Run(_ context.Context, s sandbox.Spec) (sandbox.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.specs = append(f.specs, s)
	var work, src string
	for _, m := range s.Mounts {
		switch m.Target {
		case "/work":
			work = m.Source
		case "/src":
			src = m.Source
		}
	}
	if strings.HasPrefix(s.Cmd, "cp -R /src/.") {
		f.side[work] = filepath.Base(src)
		if _, err := os.Stat(filepath.Join(src, "svc", "svc.go")); err != nil {
			return sandbox.Result{Exit: 1, Output: []byte("source copy missing")}, nil
		}
		return sandbox.Result{}, nil
	}
	side := f.side[work]
	key := side + "\x00" + s.Cmd
	f.attempts[key]++
	exit, out := f.script(side, s.Cmd, f.attempts[key])
	return sandbox.Result{Exit: exit, Output: []byte(out)}, nil
}

func goEvents(pkg string, results map[string]string) string {
	var b strings.Builder
	failed := false
	for name, a := range results {
		fmt.Fprintf(&b, "{\"Action\":\"run\",\"Package\":%q,\"Test\":%q}\n{\"Action\":%q,\"Package\":%q,\"Test\":%q}\n", pkg, name, a, pkg, name)
		failed = failed || a == "fail"
	}
	end := "pass"
	if failed {
		end = "fail"
	}
	fmt.Fprintf(&b, "{\"Action\":%q,\"Package\":%q}\n", end, pkg)
	return b.String()
}

// script: on the head, TestB fails once then passes (flaky), TestC fails on
// both sides (still failing), TestD is new and always fails (new failure);
// the smoke script breaks on the head only.
func seededScript(side, cmd string, attempt int) (int, string) {
	switch {
	case cmd == "echo setup":
		return 0, "setup ok\n"
	case cmd == "go test -json ./..." && side == "base":
		return 1, goEvents("ex/svc", map[string]string{"TestA": "pass", "TestB": "pass", "TestC": "fail"})
	case cmd == "go test -json ./..." && side == "head":
		b := "pass"
		if attempt == 1 {
			b = "fail"
		}
		return 1, goEvents("ex/svc", map[string]string{"TestA": "pass", "TestB": b, "TestC": "fail", "TestD": "fail"})
	case cmd == "sh smoke.sh" && side == "head":
		return 3, "smoke: connection refused\n"
	case cmd == "sh smoke.sh":
		return 0, "smoke ok\n"
	case cmd == "vet":
		return 1, "./svc/svc.go:4:2: loop has an empty body\nsvc/use.go:3:1: unrelated finding\n"
	}
	return 127, "unknown command " + cmd
}

const runtimeYML = `runtime:
  enabled: true
  image: alpine:3.20
  setup: ["echo setup"]
  commands: ["go test -json ./...", "sh smoke.sh"]
  reruns: 2
static_analysis: ["vet"]
`

func runtimeRepo(t *testing.T, yml string) (src, head string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	src = t.TempDir()
	gitT(t, src, "init", "-q", "-b", "main")
	for p, c := range map[string]string{"svc/svc.go": svcBase, "svc/use.go": "package svc\n\nvar _ = Retry(3)\n", ".aireviewer.yml": yml} {
		_ = os.MkdirAll(filepath.Join(src, filepath.Dir(p)), 0o755)
		if err := os.WriteFile(filepath.Join(src, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitT(t, src, "add", "-A")
	gitT(t, src, "commit", "-q", "-m", "base")
	gitT(t, src, "checkout", "-q", "-b", "feat")
	// The PR tries to switch the network on and change the commands: the
	// merge-base config must win.
	if err := os.WriteFile(filepath.Join(src, ".aireviewer.yml"), []byte(strings.Replace(yml, "reruns: 2", "reruns: 2\n  network: true", 1)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "svc", "svc.go"), []byte(svcHead), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, src, "commit", "-q", "-am", "head")
	head = gitT(t, src, "rev-parse", "HEAD")
	gitT(t, src, "update-ref", "refs/pull/3/head", head)
	gitT(t, src, "checkout", "-q", "main")
	return src, head
}

func runtimeServer(t *testing.T, src, head string, r sandbox.Runner) *mcp.ClientSession {
	t.Helper()
	root := t.TempDir()
	gp := &gateProvider{prProvider: prProvider{head: head}, author: "mallory", user: "bob"}
	srv := New(Config{Version: "test", RepoRoot: root, Store: runctx.New(root), CacheDir: t.TempDir(), Providers: gateSource{gp}, Sandbox: r,
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
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func newFake() *fakeRunner {
	return &fakeRunner{side: map[string]string{}, attempts: map[string]int{}, volumes: map[string]bool{}, script: seededScript}
}

func TestRuntimeFlow(t *testing.T) {
	src, head := runtimeRepo(t, runtimeYML)
	fr := newFake()
	cs := runtimeServer(t, src, head, fr)
	co := call[CheckoutOut](t, cs, "pr_checkout", map[string]any{"url": "https://github.com/o/r/pull/3"})
	run := co.RunID
	call[DiffmapOut](t, cs, "diffmap_build", map[string]any{"run_id": run})

	// Nothing runs without a confirmed plan.
	if msg := callErr(t, cs, "runtime_run", map[string]any{"run_id": run}); !strings.Contains(msg, "no confirmed runtime plan") {
		t.Errorf("unconfirmed run: %s", msg)
	}
	if len(fr.specs) != 0 {
		t.Fatal("container started before confirmation")
	}

	prep := call[RuntimePrepareOut](t, cs, "runtime_prepare", map[string]any{"run_id": run})
	if !prep.Applicable || prep.Proposal == nil {
		t.Fatalf("prepare = %+v", prep)
	}
	pv := prep.Proposal.Preview
	for _, want := range []string{"@mallory's PR head (third-party code)", "`alpine:3.20`", "commands and analyzers off", "go test -json ./...",
		"failures re-run 2×", "`.aireviewer.yml` at the merge base"} {
		if !strings.Contains(pv, want) {
			t.Errorf("preview lacks %q:\n%s", want, pv)
		}
	}
	if len(prep.Proposal.Warnings) == 0 || !strings.Contains(prep.Proposal.Warnings[0], "digest") {
		t.Errorf("warnings = %v", prep.Proposal.Warnings)
	}
	sub := call[SubmitProposalOut](t, cs, "proposal_submit", map[string]any{"run_id": run, "proposal_id": prep.Proposal.ProposalID, "hash": prep.Proposal.Hash})
	if !sub.Posted || sub.Confirmation != "elicitation" || !strings.Contains(strings.Join(sub.Result.Notes, " "), "runtime_run") {
		t.Fatalf("allow = %+v", sub)
	}

	rr := call[RuntimeRunOut](t, cs, "runtime_run", map[string]any{"run_id": run})
	want := map[string]int{"tests_flaky": 1, "tests_still_failing": 1, "tests_new_failure": 1, "tests_pass": 1, "commands_new_failure": 1}
	for k, v := range want {
		if rr.Summary[k] != v {
			t.Errorf("summary[%s] = %d, want %d (%v)", k, rr.Summary[k], v, rr.Summary)
		}
	}
	if len(rr.Commands) != 2 || len(rr.Commands[0].Head.Runs) != 3 || len(rr.Commands[1].Base.Runs) != 1 || rr.Commands[1].Head.Class != sandbox.ClassFail {
		t.Fatalf("commands = %+v", rr.Commands)
	}
	for _, tr := range rr.Commands[0].Tests {
		if tr.Delta == "pass" {
			t.Errorf("passing test returned in summary view: %+v", tr)
		}
	}

	// Isolation: every repository command runs as the unprivileged user,
	// without network (the PR's network: true is ignored), and the copy
	// step is the only root step, with CHOWN only.
	for _, s := range fr.specs {
		if strings.HasPrefix(s.Cmd, "cp -R") {
			if s.User != "" || len(s.CapAdd) != 1 || s.CapAdd[0] != "CHOWN" || s.Network {
				t.Errorf("copy spec = %+v", s)
			}
			continue
		}
		if s.User != sandboxUser || s.Network || s.Image != "alpine:3.20" || s.Env["HOME"] != "/cache/home" {
			t.Errorf("command spec = %+v", s)
		}
	}
	if len(fr.volumes) != 0 {
		t.Errorf("volumes left behind: %v", fr.volumes)
	}

	// Results are evidence: metrics and trace logs verify; forged ones do not.
	log := rr.Commands[1].Head.Runs[0].Log
	ev := call[EvidenceOut](t, cs, "evidence_check", map[string]any{"run_id": run, "evidence": []map[string]any{
		{"kind": "metric", "source": "runtime", "name": "cmd2.delta", "value": "new_failure"},
		{"kind": "metric", "source": "runtime", "name": "tests_flaky", "value": 1},
		{"kind": "trace", "artifact": log},
		{"kind": "metric", "source": "runtime", "name": "tests_flaky", "value": 0},
	}})
	if !ev.Results[0].OK || !ev.Results[1].OK || !ev.Results[2].OK || ev.Results[3].OK {
		t.Errorf("evidence = %+v", ev.Results)
	}
	if msg := callErr(t, cs, "run_context_put", map[string]any{"run_id": run, "path": "runtime/fake.md", "content": "PASS"}); !strings.Contains(msg, "not writable") {
		t.Errorf("forged runtime artifact: %s", msg)
	}
	res := call[RuntimeResults](t, cs, "runtime_results", map[string]any{"run_id": run})
	if res.HeadSHA != head || len(res.Commands[0].Tests) != 4 {
		t.Errorf("results = %+v", res)
	}

	lo := call[LintOut](t, cs, "lint_run", map[string]any{"run_id": run})
	if lo.Counts["findings"] != 2 || lo.Counts["in_diff"] != 1 || len(lo.Findings) != 1 || !lo.Findings[0].Changed || lo.Findings[0].File != "svc/svc.go" {
		t.Errorf("lint = %+v", lo)
	}
}

func TestRuntimeNotApplicable(t *testing.T) {
	src, head := runtimeRepo(t, "comments:\n  toxicity: warn\n")
	cs := runtimeServer(t, src, head, newFake())
	run := call[CheckoutOut](t, cs, "pr_checkout", map[string]any{"url": "https://github.com/o/r/pull/3"}).RunID
	if out := call[RuntimePrepareOut](t, cs, "runtime_prepare", map[string]any{"run_id": run}); out.Applicable || !strings.Contains(out.Reason, "runtime.image") {
		t.Errorf("no config = %+v", out)
	}

	src, head = runtimeRepo(t, runtimeYML)
	fr := newFake()
	fr.unavail = errors.New("Cannot connect to the Docker daemon")
	cs = runtimeServer(t, src, head, fr)
	run = call[CheckoutOut](t, cs, "pr_checkout", map[string]any{"url": "https://github.com/o/r/pull/3"}).RunID
	if out := call[RuntimePrepareOut](t, cs, "runtime_prepare", map[string]any{"run_id": run}); out.Applicable || !strings.Contains(out.Reason, "Docker is not available") {
		t.Errorf("no docker = %+v", out)
	}

	// On-demand commands are allowed and shown as such.
	fr.unavail = nil
	out := call[RuntimePrepareOut](t, cs, "runtime_prepare", map[string]any{"run_id": run, "commands": []string{"go test -run TestSort -bench . ./sort"}})
	if !out.Applicable || !strings.Contains(out.Proposal.Preview, "on-demand request") || !strings.Contains(out.Proposal.Preview, "-bench") {
		t.Errorf("on-demand = %+v", out)
	}
}
