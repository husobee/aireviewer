// SPDX-License-Identifier: MIT

package memory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStore(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	old := now.AddDate(-2, 0, 0)
	for _, e := range []Entry{
		{Number: 1, Title: "Add retry backoff", Summary: "Reviewers asked for jitter on the retry loop.", Files: []string{"pkg/http/client.go"},
			Decisions: []string{"Backoff must cap at 30s"}, SavedAt: now},
		{Number: 2, Title: "Docs", Summary: "Typo fixes.", Files: []string{"README.md"}, SavedAt: now},
		{Number: 3, Title: "Old retry change", Summary: "retry", Files: []string{"pkg/http/client.go"}, SavedAt: old, ClosedAt: &old},
	} {
		e.Host, e.Project, e.Ref = "github.com", "o/r", "o/r#"+string(rune('0'+e.Number))
		if _, err := s.Save(e); err != nil {
			t.Fatal(err)
		}
	}
	if fi, err := os.Stat(filepath.Join(s.Dir, "github.com", "o", "r", "prs", "1.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("entry file = %v, %v", fi, err)
	}
	if md, _ := os.ReadFile(filepath.Join(s.Dir, "github.com", "o", "r", "prs", "1.md")); !strings.Contains(string(md), "Backoff must cap") {
		t.Errorf("markdown = %s", md)
	}

	hits, err := s.Query("github.com", "o/r", "retry jitter", []string{"pkg/http/transport.go"}, 5)
	if err != nil || len(hits) != 2 || hits[0].Entry.Number != 1 {
		t.Fatalf("hits = %+v, %v", hits, err)
	}
	if hits, _ := s.Query("github.com", "o/r", "", []string{"pkg/http/client.go"}, 5); len(hits) != 2 || hits[0].Score != 3 {
		t.Errorf("file hits = %+v", hits)
	}
	if hits, _ := s.Query("github.com", "other/repo", "retry", nil, 5); len(hits) != 0 {
		t.Error("memory leaked across projects")
	}

	if n, _ := s.Prune("github.com", "o/r", 365, now); n != 1 {
		t.Errorf("pruned %d, want 1", n)
	}
	if n, _ := s.Forget("github.com", "o/r", 2); n != 1 {
		t.Errorf("forget one = %d", n)
	}
	if es, _ := s.List("github.com", "o/r"); len(es) != 1 || es[0].Number != 1 {
		t.Errorf("remaining = %+v", es)
	}
	if n, _ := s.Forget("github.com", "o/r", 0); n != 1 {
		t.Errorf("forget all = %d", n)
	}
	for _, bad := range []string{"../x", "/abs", "a/../../b"} {
		if _, err := s.Save(Entry{Host: "github.com", Project: bad, Number: 1}); err == nil {
			t.Errorf("project %q accepted", bad)
		}
	}
	if _, err := (Store{}).Save(Entry{Host: "h", Project: "p", Number: 1}); err == nil {
		t.Error("saved without a directory")
	}
}
