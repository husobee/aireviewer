// SPDX-License-Identifier: MIT

// Package memory keeps a local, per-project record of past PR
// retrospectives (paper §4.6, SPEC §11.2) that later reviews can query:
// "past decisions about this module". It lives on the reviewer's machine,
// never on the forge, and follows the repository's retention setting
// (paper §5.1.7). Entries are derived from PR text and are untrusted data.
package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Entry is one PR's retrospective.
type Entry struct {
	Ref       string         `json:"ref"`
	Host      string         `json:"host"`
	Project   string         `json:"project"`
	Number    int            `json:"number"`
	Title     string         `json:"title"`
	URL       string         `json:"url"`
	Author    string         `json:"author"`
	State     string         `json:"state"`
	ClosedAt  *time.Time     `json:"closed_at,omitempty"`
	SavedAt   time.Time      `json:"saved_at"`
	Files     []string       `json:"files"`
	Units     []string       `json:"units,omitempty"`
	Issues    []string       `json:"issues,omitempty"`
	Summary   string         `json:"summary"`
	Decisions []string       `json:"decisions,omitempty" jsonschema:"key points from the retrospective"`
	Metrics   map[string]any `json:"metrics,omitempty"`
}

// Store is the memory root (one directory per host/project).
type Store struct {
	Dir string
}

func (s Store) projectDir(host, project string) (string, error) {
	if s.Dir == "" {
		return "", errors.New("memory is not configured")
	}
	clean := path.Clean(project)
	if clean != project || strings.HasPrefix(clean, "..") || strings.HasPrefix(clean, "/") || strings.Contains(host, "/") || host == "" || host == ".." {
		return "", fmt.Errorf("invalid project %q on %q", project, host)
	}
	return filepath.Join(s.Dir, host, filepath.FromSlash(clean), "prs"), nil
}

// Save writes the entry as JSON and a readable Markdown rendering.
func (s Store) Save(e Entry) (string, error) {
	dir, err := s.projectDir(e.Host, e.Project)
	if err != nil {
		return "", err
	}
	if e.Number <= 0 {
		return "", errors.New("entry needs a PR number")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b, _ := json.MarshalIndent(e, "", "  ")
	base := filepath.Join(dir, strconv.Itoa(e.Number))
	if err := os.WriteFile(base+".json", b, 0o600); err != nil {
		return "", err
	}
	return base + ".json", os.WriteFile(base+".md", []byte(Render(e)), 0o600)
}

// Render formats an entry for humans.
func Render(e Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — %s\n\n%s · %s · by %s\n\n%s\n", e.Ref, e.Title, e.State, e.URL, e.Author, e.Summary)
	if len(e.Decisions) > 0 {
		b.WriteString("\n## Key points\n")
		for _, d := range e.Decisions {
			fmt.Fprintf(&b, "- %s\n", d)
		}
	}
	if len(e.Files) > 0 {
		fmt.Fprintf(&b, "\n## Files\n%s\n", strings.Join(e.Files, ", "))
	}
	return b.String()
}

// List returns every entry of a project.
func (s Store) List(host, project string) ([]Entry, error) {
	dir, err := s.projectDir(host, project)
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, de := range ents {
		if !strings.HasSuffix(de.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, de.Name())) //nolint:gosec // within the memory dir
		if err != nil {
			continue
		}
		var e Entry
		if json.Unmarshal(b, &e) == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

// Forget erases one PR's entry, or the whole project when number is 0.
func (s Store) Forget(host, project string, number int) (int, error) {
	dir, err := s.projectDir(host, project)
	if err != nil {
		return 0, err
	}
	if number == 0 {
		es, _ := s.List(host, project)
		return len(es), os.RemoveAll(dir)
	}
	base := filepath.Join(dir, strconv.Itoa(number))
	err = os.Remove(base + ".json")
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	_ = os.Remove(base + ".md")
	return 1, err
}

// Prune removes a project's entries older than days (by close time, else
// save time). days <= 0 keeps everything.
func (s Store) Prune(host, project string, days int, now time.Time) (int, error) {
	if days <= 0 {
		return 0, nil
	}
	es, err := s.List(host, project)
	if err != nil {
		return 0, err
	}
	cut := now.Add(-time.Duration(days) * 24 * time.Hour)
	n := 0
	for _, e := range es {
		t := e.SavedAt
		if e.ClosedAt != nil {
			t = *e.ClosedAt
		}
		if t.Before(cut) {
			if _, err := s.Forget(host, project, e.Number); err == nil {
				n++
			}
		}
	}
	return n, nil
}

// Hit is a query result.
type Hit struct {
	Entry   Entry    `json:"entry"`
	Score   float64  `json:"score"`
	Matched []string `json:"matched" jsonschema:"query terms and files that matched"`
}

func terms(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_'
	}) {
		if len(w) >= 3 && !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// Query ranks a project's entries by query-term matches in title, summary
// and key points, and by overlap with files (same file 3, same directory 1).
func (s Store) Query(host, project, query string, files []string, limit int) ([]Hit, error) {
	es, err := s.List(host, project)
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 20 {
		limit = 5
	}
	qt := terms(query)
	var hits []Hit
	for _, e := range es {
		text := strings.ToLower(e.Title + "\n" + e.Summary + "\n" + strings.Join(e.Decisions, "\n"))
		h := Hit{Entry: e}
		for _, t := range qt {
			if strings.Contains(text, t) {
				h.Score++
				h.Matched = append(h.Matched, t)
			}
		}
		for _, f := range files {
			for _, ef := range e.Files {
				switch {
				case ef == f:
					h.Score += 3
					h.Matched = append(h.Matched, ef)
				case path.Dir(ef) == path.Dir(f):
					h.Score++
					h.Matched = append(h.Matched, path.Dir(ef)+"/")
				default:
					continue
				}
				break
			}
		}
		if h.Score > 0 {
			hits = append(hits, h)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Entry.SavedAt.After(hits[j].Entry.SavedAt)
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// DefaultDir is $AIREVIEWER_DATA_DIR/memory or $XDG_DATA_HOME/aireviewer/memory
// (default ~/.local/share/aireviewer/memory).
func DefaultDir() string {
	if d := os.Getenv("AIREVIEWER_DATA_DIR"); d != "" {
		return filepath.Join(d, "memory")
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "aireviewer", "memory")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "aireviewer", "memory")
}
