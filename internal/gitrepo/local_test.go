// SPDX-License-Identifier: MIT

package gitrepo

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/husobee/aireviewer/internal/forge"
)

const patchTwo = `diff --git a/a.txt b/a.txt
--- a/a.txt
+++ b/a.txt
@@ -1,2 +1,2 @@
 one
-two
+TWO
`

const patchTwoAgain = `diff --git a/a.txt b/a.txt
--- a/a.txt
+++ b/a.txt
@@ -1,2 +1,2 @@
 one
-two
+deux
`

// authorRepo is a local clone with a feature branch one commit ahead of main,
// pushing to a bare "forge" repository.
func authorRepo(t *testing.T) (dir, bare, head string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	bare = t.TempDir()
	run(t, bare, "init", "-q", "--bare", "-b", "main")
	dir = t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "config", "user.name", "Ada")
	run(t, dir, "config", "user.email", "ada@example.com")
	writeFile(t, filepath.Join(dir, "a.txt"), []byte("one\n"))
	run(t, dir, "add", "-A")
	run(t, dir, "commit", "-q", "-m", "base")
	run(t, dir, "remote", "add", "origin", bare)
	run(t, dir, "push", "-q", "origin", "main")
	run(t, dir, "checkout", "-q", "-b", "feat")
	writeFile(t, filepath.Join(dir, "a.txt"), []byte("one\ntwo\n"))
	run(t, dir, "commit", "-q", "-am", "add two")
	head = run(t, dir, "rev-parse", "HEAD")
	return dir, bare, head
}

func TestLocalCommitAndPush(t *testing.T) {
	ctx := context.Background()
	dir, bare, head := authorRepo(t)
	l := Local{Dir: dir}
	if b, err := l.CurrentBranch(ctx); err != nil || b != "feat" {
		t.Fatalf("current branch = %q, %v", b, err)
	}
	if err := l.ValidBranch(ctx, "-x"); err == nil {
		t.Error("option-like branch accepted")
	}
	if _, err := l.BuildTree(ctx, head, []string{patchTwo, patchTwoAgain}); err == nil || !strings.Contains(err.Error(), "patch 2") {
		t.Errorf("conflicting patches: %v", err)
	}
	if _, err := l.Commit(ctx, "feat", "0000000", []string{patchTwo}, CommitOptions{Message: "m"}); err == nil || !strings.Contains(err.Error(), "moved") {
		t.Errorf("stale head: %v", err)
	}

	// Checked out and clean: the worktree is fast-forwarded.
	c, err := l.Commit(ctx, "feat", head, []string{patchTwo}, CommitOptions{Message: "aireviewer: apply review fixes", Signoff: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := run(t, dir, "rev-parse", "HEAD"); got != c {
		t.Errorf("HEAD = %s, want %s", got, c)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "one\nTWO\n" {
		t.Errorf("worktree not updated: %q", b)
	}
	if msg := run(t, dir, "log", "-1", "--format=%B"); !strings.Contains(msg, "Signed-off-by: Ada <ada@example.com>") {
		t.Errorf("message = %q", msg)
	}
	if st := run(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("worktree dirty after commit: %q", st)
	}

	if err := l.Push(ctx, "origin", "feat"); err != nil {
		t.Fatal(err)
	}
	if got := run(t, bare, "rev-parse", "refs/heads/feat"); got != c {
		t.Errorf("pushed %s, want %s", got, c)
	}
	if up := run(t, dir, "rev-parse", "--abbrev-ref", "feat@{upstream}"); up != "origin/feat" {
		t.Errorf("upstream = %q", up)
	}
}

func TestLocalCommitNotCheckedOut(t *testing.T) {
	ctx := context.Background()
	dir, _, head := authorRepo(t)
	run(t, dir, "checkout", "-q", "main")
	writeFile(t, filepath.Join(dir, "scratch.txt"), []byte("uncommitted work on main\n"))
	l := Local{Dir: dir}
	c, err := l.Commit(ctx, "feat", head, []string{patchTwo}, CommitOptions{Message: "fix"})
	if err != nil {
		t.Fatal(err)
	}
	if got := run(t, dir, "rev-parse", "refs/heads/feat"); got != c {
		t.Errorf("feat = %s, want %s", got, c)
	}
	if got := run(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != "main" {
		t.Errorf("HEAD moved to %s", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "scratch.txt")); !strings.Contains(string(b), "uncommitted") {
		t.Error("unrelated work touched")
	}
	if show := run(t, dir, "show", "feat:a.txt"); show != "one\nTWO" {
		t.Errorf("feat content = %q", show)
	}
}

func TestLocalCommitRefusesConflictingWorktree(t *testing.T) {
	ctx := context.Background()
	dir, _, head := authorRepo(t)
	writeFile(t, filepath.Join(dir, "a.txt"), []byte("one\ntwo\nlocal edit\n"))
	l := Local{Dir: dir}
	if _, err := l.Commit(ctx, "feat", head, []string{patchTwo}, CommitOptions{Message: "fix"}); err == nil || !strings.Contains(err.Error(), "fast-forward") {
		t.Fatalf("expected fast-forward refusal, got %v", err)
	}
	if got := run(t, dir, "rev-parse", "refs/heads/feat"); got != head {
		t.Errorf("branch moved despite refusal: %s", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); !strings.Contains(string(b), "local edit") {
		t.Error("local edit lost")
	}
}

func TestPrepareBranch(t *testing.T) {
	ctx := context.Background()
	dir, bare, head := authorRepo(t)
	g := Open(t.TempDir(), forge.Repo{Forge: forge.GitHub, Host: "github.com", Project: "o/r"}, Remote{URL: bare, Kind: forge.GitHub})
	co, ahead, err := g.PrepareBranch(ctx, dir, "feat", "main", filepath.Join(t.TempDir(), "wt"))
	if err != nil {
		t.Fatal(err)
	}
	if co.HeadSHA != head || ahead != 1 || co.MergeBase == "" {
		t.Errorf("checkout = %+v ahead=%d", co, ahead)
	}
	recs, err := g.Commits(ctx, co.MergeBase, co.HeadSHA, 10)
	if err != nil || len(recs) != 1 || !strings.Contains(recs[0], "add two") {
		t.Errorf("commits = %q, %v", recs, err)
	}
}
