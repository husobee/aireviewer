// SPDX-License-Identifier: MIT

// Package runctx is the local, cross-stage run context store
// (.aireviewer/runs/<run-id>/...). See SPEC.md §5.
package runctx

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/husobee/aireviewer/internal/forge"
)

// MaxFileBytes caps a single stored artifact.
const MaxFileBytes = 8 << 20

// Store persists run artifacts under Root.
type Store struct {
	Root string // e.g. <repo>/.aireviewer/runs
}

// New returns a store rooted at <base>/.aireviewer/runs.
func New(base string) *Store {
	return &Store{Root: filepath.Join(base, ".aireviewer", "runs")}
}

var unsafeChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// RunIDForPR returns the stable run id for a PR, e.g.
// "github-github.com-octo-repo-pr-12".
func RunIDForPR(ref forge.PRRef) string {
	return sanitize(fmt.Sprintf("%s-%s-%s-pr-%d", ref.Forge, ref.Host, ref.Project, ref.Number))
}

// RunIDForBranch returns the stable run id for a not-yet-created PR.
func RunIDForBranch(repo forge.Repo, branch string) string {
	return sanitize(fmt.Sprintf("%s-%s-%s-branch-%s", repo.Forge, repo.Host, repo.Project, branch))
}

func sanitize(s string) string {
	return strings.Trim(unsafeChars.ReplaceAllString(s, "-"), "-.")
}

// ErrBadPath is returned for run ids or paths that would escape the store.
var ErrBadPath = errors.New("invalid run id or path")

func (s *Store) resolve(runID, rel string) (string, error) {
	if runID == "" || runID != sanitize(runID) {
		return "", fmt.Errorf("%w: run id %q", ErrBadPath, runID)
	}
	if rel == "" || filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%w: %q", ErrBadPath, rel)
	}
	return filepath.Join(s.Root, runID, filepath.Clean(rel)), nil
}

// Put writes data to <run>/<rel>, creating directories as needed.
func (s *Store) Put(runID, rel string, data []byte) (string, error) {
	if len(data) > MaxFileBytes {
		return "", fmt.Errorf("artifact %s is %d bytes, limit %d", rel, len(data), MaxFileBytes)
	}
	p, err := s.resolve(runID, rel)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return "", err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return "", err
	}
	return p, os.Rename(tmp, p)
}

// Get reads <run>/<rel>. It returns fs.ErrNotExist when absent.
func (s *Store) Get(runID, rel string) ([]byte, error) {
	p, err := s.resolve(runID, rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p) //nolint:gosec // p is confined to the store by resolve()
}

// List returns the relative paths of all files stored for a run.
func (s *Store) List(runID string) ([]string, error) {
	if runID == "" || runID != sanitize(runID) {
		return nil, fmt.Errorf("%w: run id %q", ErrBadPath, runID)
	}
	dir := filepath.Join(s.Root, runID)
	var out []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !strings.HasSuffix(path, ".tmp") {
			rel, _ := filepath.Rel(dir, path)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return []string{}, nil
	}
	return out, err
}
