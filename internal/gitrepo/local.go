// SPDX-License-Identifier: MIT

package gitrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// M4 (/pr-create): the author's own repository.
//
// Analysis still reads git objects from the shared mirror (ADR 0005): the
// local branch is fetched into it with PrepareBranch. Writes to the author's
// repository (the fix commit and the push) go through Local, which uses the
// author's own git configuration and credentials, never an aireviewer token,
// and only ever run from proposal_submit after a human confirmed the frozen
// proposal (ADR 0007).

// PrepareBranch fetches baseBranch from the forge and branch from the local
// repository at localDir into the mirror, computes the merge base, and checks
// out a detached worktree at the branch head.
func (g *Git) PrepareBranch(ctx context.Context, localDir, branch, baseBranch, worktree string) (*Checkout, int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := g.ensureMirror(ctx); err != nil {
		return nil, 0, err
	}
	headRef := "refs/aireviewer/branch/" + branch + "/head"
	baseRef := "refs/aireviewer/branch/" + branch + "/base"
	if _, err := g.Run(ctx, "", "fetch", "--no-tags", "--force", "origin", "+refs/heads/"+baseBranch+":"+baseRef); err != nil {
		return nil, 0, fmt.Errorf("fetch base branch %q from the forge: %w", baseBranch, err)
	}
	abs, err := filepath.Abs(localDir)
	if err != nil {
		return nil, 0, err
	}
	if _, err := g.Run(ctx, "", "fetch", "--no-tags", "--force", abs, "+refs/heads/"+branch+":"+headRef); err != nil {
		return nil, 0, fmt.Errorf("fetch local branch %q: %w", branch, err)
	}
	head, err := g.Run(ctx, "", "rev-parse", headRef)
	if err != nil {
		return nil, 0, err
	}
	base, err := g.Run(ctx, "", "rev-parse", baseRef)
	if err != nil {
		return nil, 0, err
	}
	head, base = strings.TrimSpace(head), strings.TrimSpace(base)
	mb, err := g.Run(ctx, "", "merge-base", head, base)
	if err != nil {
		return nil, 0, fmt.Errorf("branch %q shares no history with %q: %w", branch, baseBranch, err)
	}
	mbs := strings.TrimSpace(mb)
	n, err := g.Run(ctx, "", "rev-list", "--count", mbs+".."+head)
	if err != nil {
		return nil, 0, err
	}
	ahead, _ := strconv.Atoi(strings.TrimSpace(n))
	co := &Checkout{Mirror: g.Dir, Worktree: worktree, HeadSHA: head, BaseSHA: base, MergeBase: mbs}
	if err := g.checkout(ctx, worktree, head); err != nil {
		return nil, 0, err
	}
	return co, ahead, nil
}

// Commits returns "sha\x1fsubject\x1fbody" records for from..to, oldest first.
func (g *Git) Commits(ctx context.Context, from, to string, limit int) ([]string, error) {
	out, err := g.Run(ctx, "", "log", "--reverse", "--no-color", "--format=%H%x1f%s%x1f%b%x1e", "-n", strconv.Itoa(limit), from+".."+to)
	if err != nil {
		return nil, err
	}
	var recs []string
	for _, r := range strings.Split(out, "\x1e") {
		if r = strings.Trim(r, "\n"); r != "" {
			recs = append(recs, r)
		}
	}
	return recs, nil
}

// Local runs git in the author's repository with their own configuration.
type Local struct {
	Dir string
}

func (l Local) run(ctx context.Context, dir, stdin string, extraEnv []string, args ...string) (string, error) {
	if dir == "" {
		dir = l.Dir
	}
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // fixed binary, argv not shell; args built internally
	cmd.Dir = dir
	// Never prompt: the MCP server has no terminal. Credentials must come
	// from the author's helper, agent or cached SSH key.
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"), extraEnv...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Run executes a git command in the repository.
func (l Local) Run(ctx context.Context, args ...string) (string, error) {
	out, err := l.run(ctx, "", "", nil, args...)
	return strings.TrimSpace(out), err
}

// CurrentBranch returns the checked-out branch name.
func (l Local) CurrentBranch(ctx context.Context) (string, error) {
	b, err := l.Run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", errors.New("HEAD is detached: name the branch to open a PR from")
	}
	return b, nil
}

// ValidBranch rejects names git would not accept as a branch.
func (l Local) ValidBranch(ctx context.Context, name string) error {
	if strings.HasPrefix(name, "-") {
		return fmt.Errorf("invalid branch name %q", name)
	}
	if _, err := l.Run(ctx, "check-ref-format", "--branch", name); err != nil {
		return fmt.Errorf("invalid branch name %q", name)
	}
	return nil
}

// BranchSHA returns the commit a local branch points at.
func (l Local) BranchSHA(ctx context.Context, branch string) (string, error) {
	return l.Run(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
}

// RemoteURL returns the URL of a named remote as configured, before any
// url.*.insteadOf rewriting (which `git remote get-url` applies), so the
// forge project is recognised even when the author rewrites HTTPS to SSH.
func (l Local) RemoteURL(ctx context.Context, name string) (string, error) {
	if strings.HasPrefix(name, "-") || strings.ContainsAny(name, " \t\n") {
		return "", fmt.Errorf("invalid remote name %q", name)
	}
	u, err := l.Run(ctx, "config", "--get", "remote."+name+".url")
	if err != nil || u == "" {
		return "", fmt.Errorf("no git remote named %q", name)
	}
	return u, nil
}

// Dirty reports whether the working tree has uncommitted changes.
func (l Local) Dirty(ctx context.Context) (bool, error) {
	out, err := l.Run(ctx, "status", "--porcelain", "--untracked-files=no")
	return out != "", err
}

// CheckedOutAt returns the worktree path where branch is checked out, if any.
func (l Local) CheckedOutAt(ctx context.Context, branch string) (string, error) {
	out, err := l.Run(ctx, "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	var cur string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(line, "worktree "):
			cur = strings.TrimPrefix(line, "worktree ")
		case line == "branch refs/heads/"+branch:
			return cur, nil
		}
	}
	return "", nil
}

// BuildTree applies patches in order on top of head in a private index and
// returns the resulting tree. Nothing in the repository changes except new
// loose objects. An error names the first patch that does not apply.
func (l Local) BuildTree(ctx context.Context, head string, patches []string) (string, error) {
	f, err := os.CreateTemp("", "aireviewer-index-*")
	if err != nil {
		return "", err
	}
	idx := f.Name()
	_ = f.Close()
	_ = os.Remove(idx) // git creates it; only the unique name is needed
	defer func() { _ = os.Remove(idx) }()
	env := []string{"GIT_INDEX_FILE=" + idx}
	if _, err := l.run(ctx, "", "", env, "read-tree", head); err != nil {
		return "", err
	}
	for i, p := range patches {
		if _, err := l.run(ctx, "", p, env, "apply", "--cached", "--recount", "-"); err != nil {
			return "", fmt.Errorf("patch %d does not apply on top of the earlier ones: %w", i+1, err)
		}
	}
	tree, err := l.run(ctx, "", "", env, "write-tree")
	return strings.TrimSpace(tree), err
}

// CommitOptions controls Commit.
type CommitOptions struct {
	Message string
	Sign    bool // GPG/SSH-sign with the author's configured key
	Signoff bool // add a DCO Signed-off-by trailer
}

// SignsByDefault reports the repository's commit.gpgSign setting.
func (l Local) SignsByDefault(ctx context.Context) bool {
	v, err := l.Run(ctx, "config", "--bool", "commit.gpgSign")
	return err == nil && v == "true"
}

// Commit records patches as one new commit on branch, whose tip must still be
// expectedHead. If the branch is checked out somewhere, that worktree is
// fast-forwarded (git refuses if local changes would be overwritten);
// otherwise the ref is moved with a compare-and-swap.
func (l Local) Commit(ctx context.Context, branch, expectedHead string, patches []string, o CommitOptions) (string, error) {
	cur, err := l.BranchSHA(ctx, branch)
	if err != nil || cur != expectedHead {
		return "", fmt.Errorf("branch %q moved (now %s, expected %s): re-run the checkout", branch, short(cur), short(expectedHead))
	}
	tree, err := l.BuildTree(ctx, expectedHead, patches)
	if err != nil {
		return "", err
	}
	msg := strings.TrimSpace(o.Message) + "\n"
	if o.Signoff {
		name, _ := l.Run(ctx, "config", "user.name")
		email, _ := l.Run(ctx, "config", "user.email")
		if name == "" || email == "" {
			return "", errors.New("signoff requested but user.name/user.email are not configured")
		}
		trailer := "Signed-off-by: " + name + " <" + email + ">"
		if !strings.Contains(msg, trailer) {
			msg += "\n" + trailer + "\n"
		}
	}
	args := []string{"commit-tree", tree, "-p", expectedHead, "-F", "-"}
	if o.Sign {
		args = append(args, "-S")
	}
	out, err := l.run(ctx, "", msg, nil, args...)
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(out)
	wt, err := l.CheckedOutAt(ctx, branch)
	if err != nil {
		return "", err
	}
	if wt != "" {
		if _, err := l.run(ctx, wt, "", nil, "merge", "--ff-only", "--quiet", commit); err != nil {
			return "", fmt.Errorf("could not fast-forward the checked-out branch in %s (commit %s was created but not applied): %w", wt, short(commit), err)
		}
		return commit, nil
	}
	if _, err := l.Run(ctx, "update-ref", "-m", "aireviewer: apply review fixes", "refs/heads/"+branch, commit, expectedHead); err != nil {
		return "", err
	}
	return commit, nil
}

// Push pushes branch to the remote with the author's credentials and sets
// its upstream. It never forces.
func (l Local) Push(ctx context.Context, remote, branch string) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	// No terminal and stdin is /dev/null, so neither git nor ssh can prompt;
	// core.sshCommand and credential helpers are honoured as configured.
	_, err := l.run(ctx, "", "", nil, "push", "--set-upstream", remote, "refs/heads/"+branch+":refs/heads/"+branch)
	return err
}

func short(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	if s == "" {
		return "?"
	}
	return s
}
