// SPDX-License-Identifier: MIT

package gitrepo

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/husobee/aireviewer/internal/forge"
)

func run(t *testing.T, dir string, args ...string) string {
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

func writeFile(t *testing.T, p string, b []byte) {
	t.Helper()
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeForge creates a repo whose PR head lives only at refs/pull/7/head (as
// on GitHub) or refs/merge-requests/7/head (GitLab), and main has moved on.
func fakeForge(t *testing.T, kind forge.Kind) (dir, base, head string) {
	t.Helper()
	dir = t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "config", "uploadpack.allowFilter", "true")
	writeFile(t, filepath.Join(dir, "a.txt"), []byte("one\n"))
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "base")
	base = run(t, dir, "rev-parse", "HEAD")

	run(t, dir, "checkout", "-q", "-b", "feature")
	writeFile(t, filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"))
	run(t, dir, "commit", "-q", "-am", "feature")
	head = run(t, dir, "rev-parse", "HEAD")
	ref := "refs/pull/7/head"
	if kind == forge.GitLab {
		ref = "refs/merge-requests/7/head"
	}
	run(t, dir, "update-ref", ref, head)

	run(t, dir, "checkout", "-q", "main")
	writeFile(t, filepath.Join(dir, "b.txt"), []byte("main moved\n"))
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "main moves on")
	run(t, dir, "branch", "-D", "feature")
	return dir, base, head
}

func TestPrepare(t *testing.T) {
	for _, kind := range []forge.Kind{forge.GitHub, forge.GitLab} {
		t.Run(string(kind), func(t *testing.T) {
			t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
			src, base, head := fakeForge(t, kind)
			repo := forge.Repo{Forge: kind, Host: "example.com", Project: "grp/proj"}
			g := Open(t.TempDir(), repo, Remote{URL: "file://" + src, Kind: kind, Token: "s3cret"})
			wt := filepath.Join(t.TempDir(), "wt")
			pr := &forge.PR{Ref: forge.PRRef{Repo: repo, Number: 7}, BaseBranch: "main", HeadSHA: head}

			co, err := g.Prepare(context.Background(), pr, wt)
			if err != nil {
				t.Fatal(err)
			}
			if co.MergeBase != base || co.HeadSHA != head {
				t.Errorf("checkout = %+v, want merge base %s head %s", co, base, head)
			}
			b, err := os.ReadFile(filepath.Join(wt, "a.txt"))
			if err != nil || string(b) != "one\ntwo\n" {
				t.Errorf("worktree content = %q, %v", b, err)
			}
			if _, err := os.Stat(filepath.Join(wt, "b.txt")); err == nil {
				t.Error("worktree should be at PR head, not main")
			}
			diff, err := g.Diff(context.Background(), co.MergeBase, co.HeadSHA, 0)
			if err != nil || !strings.Contains(diff, "+two") || strings.Contains(diff, "b.txt") {
				t.Errorf("diff = %q, %v", diff, err)
			}
			if _, err := g.Show(context.Background(), co.MergeBase, "nope.txt"); err != ErrNoFile {
				t.Errorf("Show missing = %v", err)
			}
			// A merged PR (head reachable from main) must still diff against
			// the recorded base SHA, not collapse to an empty diff.
			run(t, src, "merge", "-q", "--no-ff", "-m", "merge", head)
			merged := &forge.PR{Ref: pr.Ref, BaseBranch: "main", HeadSHA: head, BaseSHA: base}
			co2, err := g.Prepare(context.Background(), merged, wt)
			if err != nil || co2.MergeBase != base {
				t.Errorf("merged PR merge base = %v, %v; want %s", co2, err, base)
			}
			// Idempotent re-prepare reuses the worktree.
			if _, err := g.Prepare(context.Background(), pr, wt); err != nil {
				t.Fatalf("second prepare: %v", err)
			}
			cfg, _ := os.ReadFile(filepath.Join(g.Dir, "config"))
			if strings.Contains(string(cfg), "s3cret") {
				t.Error("token persisted in mirror config")
			}
		})
	}
}
