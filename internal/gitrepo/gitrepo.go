// SPDX-License-Identifier: MIT

// Package gitrepo manages local checkouts of PR heads: a shared, blobless,
// bare mirror per project in the user cache directory, and one detached
// worktree per run inside the workspace (.aireviewer/worktrees/<run_id>) so
// that agents can read and grep the code without leaving the project.
//
// Credentials are passed to git through GIT_CONFIG_* environment variables,
// never through argv, so they do not appear in process listings or in the
// mirror's config.
package gitrepo

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/husobee/aireviewer/internal/forge"
)

// Remote describes where to fetch from and how to authenticate.
type Remote struct {
	URL   string // clone URL (https://host/project.git, or a local path in tests)
	Kind  forge.Kind
	Token string // optional
}

// RemoteFor builds the HTTPS clone URL for a forge repo.
func RemoteFor(repo forge.Repo, token string) Remote {
	return Remote{URL: "https://" + repo.Host + "/" + repo.Project + ".git", Kind: repo.Forge, Token: token}
}

// Checkout is the result of preparing a PR for local analysis.
type Checkout struct {
	Mirror    string `json:"mirror"`
	Worktree  string `json:"worktree"`
	HeadSHA   string `json:"head_sha"`
	BaseSHA   string `json:"base_sha"`
	MergeBase string `json:"merge_base"`
}

// Git runs git commands against one mirror.
type Git struct {
	Dir    string // bare mirror directory
	remote Remote
}

func (g *Git) env() []string {
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true", "LC_ALL=C")
	var kv [][2]string
	if strings.HasPrefix(g.remote.URL, "https://") || strings.HasPrefix(g.remote.URL, "http://") {
		// Users commonly rewrite https://<forge>/ to SSH via url.*.insteadOf.
		// aireviewer authenticates over HTTPS with a token, so pin the exact
		// remote URL to itself: git applies the longest matching insteadOf,
		// and this one is at least as long as any user rule for the host.
		kv = append(kv, [2]string{"url." + g.remote.URL + ".insteadOf", g.remote.URL})
	}
	if g.remote.Token != "" {
		user := "x-access-token"
		if g.remote.Kind == forge.GitLab {
			user = "oauth2"
		}
		cred := base64.StdEncoding.EncodeToString([]byte(user + ":" + g.remote.Token))
		kv = append(kv, [2]string{"http.extraHeader", "Authorization: Basic " + cred})
	}
	if len(kv) == 0 {
		return env
	}
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(kv)))
	for i, p := range kv {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, p[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, p[1]))
	}
	return env
}

// Run executes git with the mirror as GIT_DIR (or in dir when non-empty).
func (g *Git) Run(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // fixed binary, argv not shell; args built internally
	cmd.Env = g.env()
	if dir != "" {
		cmd.Dir = dir
	} else {
		cmd.Env = append(cmd.Env, "GIT_DIR="+g.Dir)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if g.remote.Token != "" {
			msg = strings.ReplaceAll(msg, g.remote.Token, "***")
		}
		return stdout.String(), fmt.Errorf("git %s: %w: %s", args[0], err, msg)
	}
	return stdout.String(), nil
}

// Open returns a Git for an existing or new mirror under cacheDir.
func Open(cacheDir string, repo forge.Repo, remote Remote) *Git {
	dir := filepath.Join(cacheDir, "mirrors", repo.Host, filepath.FromSlash(repo.Project)+".git")
	return &Git{Dir: dir, remote: remote}
}

// ensureMirror creates the bare blobless mirror if missing.
func (g *Git) ensureMirror(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(g.Dir, "HEAD")); err == nil {
		// Keep the remote URL current (e.g. host moved, test fixtures).
		_, err := g.Run(ctx, "", "remote", "set-url", "origin", g.remote.URL)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(g.Dir), 0o750); err != nil {
		return err
	}
	tmp := g.Dir + ".tmp-" + strconv.Itoa(os.Getpid())
	_ = os.RemoveAll(tmp)
	cmd := exec.CommandContext(ctx, "git", "clone", "--bare", "--filter=blob:none", "--no-tags", "--", g.remote.URL, tmp) //nolint:gosec // argv, no shell
	cmd.Env = g.env()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(tmp)
		msg := stderr.String()
		if g.remote.Token != "" {
			msg = strings.ReplaceAll(msg, g.remote.Token, "***")
		}
		return fmt.Errorf("clone %s: %w: %s", g.remote.URL, err, strings.TrimSpace(msg))
	}
	return os.Rename(tmp, g.Dir)
}

// prRefspec returns the forge's ref for a PR head.
func prRefspec(kind forge.Kind, number int) string {
	if kind == forge.GitLab {
		return fmt.Sprintf("refs/merge-requests/%d/head", number)
	}
	return fmt.Sprintf("refs/pull/%d/head", number)
}

// Prepare fetches the PR head and base branch into the mirror, computes the
// merge base, and checks out a detached worktree at the head SHA.
func (g *Git) Prepare(ctx context.Context, pr *forge.PR, worktree string) (*Checkout, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if err := g.ensureMirror(ctx); err != nil {
		return nil, err
	}
	n := pr.Ref.Number
	headRef := fmt.Sprintf("refs/aireviewer/pr-%d/head", n)
	baseRef := fmt.Sprintf("refs/aireviewer/pr-%d/base", n)
	if _, err := g.Run(ctx, "", "fetch", "--no-tags", "--force", "origin",
		"+refs/heads/"+pr.BaseBranch+":"+baseRef); err != nil {
		return nil, err
	}
	// Prefer the forge's PR ref; forges may prune it for old or closed PRs,
	// so fall back to fetching the head commit by SHA.
	if _, err := g.Run(ctx, "", "fetch", "--no-tags", "--force", "origin",
		"+"+prRefspec(pr.Ref.Forge, n)+":"+headRef); err != nil {
		if pr.HeadSHA == "" {
			return nil, err
		}
		if _, err2 := g.Run(ctx, "", "fetch", "--no-tags", "origin", pr.HeadSHA); err2 != nil {
			return nil, fmt.Errorf("%w; fetching head %s by SHA also failed: %v", err, pr.HeadSHA, err2)
		}
	}
	head := pr.HeadSHA
	if head == "" {
		out, err := g.Run(ctx, "", "rev-parse", headRef)
		if err != nil {
			return nil, err
		}
		head = strings.TrimSpace(out)
	}
	if _, err := g.Run(ctx, "", "cat-file", "-e", head+"^{commit}"); err != nil {
		return nil, fmt.Errorf("head %s not available after fetch (was the %s force-pushed since it was loaded?): %w", head, pr.Ref.Forge.Abbrev(), err)
	}
	// Diff against the base the forge recorded for the PR (GitHub base.sha,
	// GitLab diff_refs.base_sha) so merged PRs and moved base branches still
	// produce the PR's own diff; fall back to the current base branch tip.
	baseCommit := baseRef
	if pr.BaseSHA != "" {
		if _, err := g.Run(ctx, "", "cat-file", "-e", pr.BaseSHA+"^{commit}"); err != nil {
			_, _ = g.Run(ctx, "", "fetch", "--no-tags", "origin", pr.BaseSHA)
		}
		if _, err := g.Run(ctx, "", "cat-file", "-e", pr.BaseSHA+"^{commit}"); err == nil {
			baseCommit = pr.BaseSHA
		}
	}
	mb, err := g.Run(ctx, "", "merge-base", head, baseCommit)
	if err != nil {
		return nil, err
	}
	co := &Checkout{Mirror: g.Dir, Worktree: worktree, HeadSHA: head, BaseSHA: pr.BaseSHA, MergeBase: strings.TrimSpace(mb)}
	if err := g.checkout(ctx, worktree, head); err != nil {
		return nil, err
	}
	return co, nil
}

func (g *Git) checkout(ctx context.Context, worktree, sha string) error {
	if _, err := os.Stat(filepath.Join(worktree, ".git")); err == nil {
		_, err := g.Run(ctx, worktree, "checkout", "--detach", "--force", sha)
		if err == nil {
			_, err = g.Run(ctx, worktree, "clean", "-fdx", "-q")
		}
		return err
	}
	if err := os.MkdirAll(filepath.Dir(worktree), 0o750); err != nil {
		return err
	}
	_, _ = g.Run(ctx, "", "worktree", "prune")
	_, err := g.Run(ctx, "", "worktree", "add", "--detach", "--force", worktree, sha)
	return err
}

// Diff returns `git diff` output between two commits with rename detection
// and the given amount of context.
func (g *Git) Diff(ctx context.Context, from, to string, context int) (string, error) {
	return g.Run(ctx, "", "-c", "core.quotePath=false", "diff", "--no-color", "--no-ext-diff", "-M",
		"-U"+strconv.Itoa(context), from, to)
}

// ErrNoFile is returned by Show when the path does not exist at the commit.
var ErrNoFile = errors.New("file not present at commit")

// Show returns a file's content at a commit.
func (g *Git) Show(ctx context.Context, sha, path string) ([]byte, error) {
	out, err := g.Run(ctx, "", "show", sha+":"+path)
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") || strings.Contains(err.Error(), "exists on disk, but not in") {
			return nil, ErrNoFile
		}
		return nil, err
	}
	return []byte(out), nil
}

// ApplyCheck reports whether patch applies cleanly to the worktree (which is
// checked out at the PR head). Nothing is modified.
func (g *Git) ApplyCheck(ctx context.Context, worktree, patch string) error {
	cmd := exec.CommandContext(ctx, "git", "apply", "--check", "--recount", "-") //nolint:gosec // fixed argv
	cmd.Dir = worktree
	cmd.Env = g.env()
	cmd.Stdin = strings.NewReader(patch)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("patch does not apply to the PR head: %s", strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Grep runs `git grep` against a commit (never the filesystem), returning
// "path:line:text" lines. No match is not an error.
func (g *Git) Grep(ctx context.Context, sha, pattern string, pathspecs []string, ignoreCase bool) ([]string, error) {
	args := []string{"grep", "-n", "-I", "-E", "--no-color", "--full-name"}
	if ignoreCase {
		args = append(args, "-i")
	}
	args = append(args, "-e", pattern, sha, "--")
	args = append(args, pathspecs...)
	out, err := g.Run(ctx, "", args...)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	var lines []string
	prefix := sha + ":"
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if l != "" {
			lines = append(lines, strings.TrimPrefix(l, prefix))
		}
	}
	return lines, nil
}

// LsTree lists file paths in a commit under an optional directory prefix.
func (g *Git) LsTree(ctx context.Context, sha, prefix string) ([]string, error) {
	args := []string{"-c", "core.quotePath=false", "ls-tree", "-r", "--name-only", sha}
	if prefix != "" {
		args = append(args, "--", prefix)
	}
	out, err := g.Run(ctx, "", args...)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if l != "" {
			files = append(files, l)
		}
	}
	return files, nil
}

// Archive exports the tree at sha into dir (which must exist and be empty)
// with `git archive | tar -x`, never touching a worktree. Used to give the
// sandbox a copy of the code (ADR 0008).
func (g *Git) Archive(ctx context.Context, sha, dir string) error {
	ar := exec.CommandContext(ctx, "git", "archive", "--format=tar", sha) //nolint:gosec // fixed argv
	ar.Env = append(g.env(), "GIT_DIR="+g.Dir)
	tx := exec.CommandContext(ctx, "tar", "-x", "--no-same-owner", "-C", dir) //nolint:gosec // fixed argv
	pipe, err := ar.StdoutPipe()
	if err != nil {
		return err
	}
	tx.Stdin = pipe
	var aerr, terr bytes.Buffer
	ar.Stderr, tx.Stderr = &aerr, &terr
	if err := tx.Start(); err != nil {
		return err
	}
	if err := ar.Run(); err != nil {
		_ = tx.Wait()
		return fmt.Errorf("git archive %s: %w: %s", sha, err, strings.TrimSpace(aerr.String()))
	}
	if err := tx.Wait(); err != nil {
		return fmt.Errorf("extract archive: %w: %s", err, strings.TrimSpace(terr.String()))
	}
	return nil
}

// CacheDir returns the cache root: $AIREVIEWER_CACHE_DIR or <user cache>/aireviewer.
func CacheDir() (string, error) {
	if d := os.Getenv("AIREVIEWER_CACHE_DIR"); d != "" {
		return d, nil
	}
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "aireviewer"), nil
}
