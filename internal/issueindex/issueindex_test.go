// SPDX-License-Identifier: MIT

package issueindex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/husobee/aireviewer/internal/forge"
)

type rec struct {
	is      forge.Issue
	updated time.Time
	closed  *time.Time
}

// fakeLister serves issues updated after since, oldest first, 2 per page.
type fakeLister struct {
	all   []rec
	calls []time.Time
}

func (f *fakeLister) ListIssuesUpdatedSince(_ context.Context, _ forge.Repo, since time.Time, page int) ([]forge.Issue, []time.Time, []*time.Time, bool, error) {
	f.calls = append(f.calls, since)
	var sel []rec
	for _, r := range f.all {
		if r.updated.After(since) {
			sel = append(sel, r)
		}
	}
	start, end := (page-1)*2, page*2
	if start >= len(sel) {
		return nil, nil, nil, false, nil
	}
	if end > len(sel) {
		end = len(sel)
	}
	var is []forge.Issue
	var up []time.Time
	var cl []*time.Time
	for _, r := range sel[start:end] {
		is, up, cl = append(is, r.is), append(up, r.updated), append(cl, r.closed)
	}
	return is, up, cl, end < len(sel), nil
}

func TestSyncAndSearch(t *testing.T) {
	now := time.Date(2026, 11, 23, 0, 0, 0, 0, time.UTC)
	day := func(d int) time.Time { return now.AddDate(0, 0, -d) }
	oldClose, recentClose := day(200), day(10)
	l := &fakeLister{all: []rec{
		{forge.Issue{Number: 1, Ref: "o/r#1", Title: "Retries hammer upstream", Body: "The HTTP client retries without backoff.", State: "open", Labels: []string{"bug"}}, day(50), nil},
		{forge.Issue{Number: 2, Ref: "o/r#2", Title: "Docs typo", Body: "README says retrys.", State: "open"}, day(40), nil},
		{forge.Issue{Number: 3, Ref: "o/r#3", Title: "Old retry crash", Body: "retry panics", State: "closed"}, day(200), &oldClose},
		{forge.Issue{Number: 4, Ref: "o/r#4", Title: "Client timeout regression", Body: "backoff resets the timer", State: "closed"}, day(10), &recentClose},
		{forge.Issue{Number: 5, Ref: "o/r#5", Title: "Add metrics", Body: "Expose retry counters.", State: "open"}, day(5), nil},
	}}
	path := filepath.Join(t.TempDir(), "github.com", "o", "r.db")
	ix, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ix.Close() }()
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("index file mode = %v, %v", fi.Mode().Perm(), err)
	}
	ctx := context.Background()

	res, err := ix.Sync(ctx, l, forge.Repo{}, now)
	if err != nil || !res.Full || res.Fetched != 5 || res.Issues != 5 || res.Open != 3 || !res.Cursor.Equal(day(5)) {
		t.Fatalf("first sync = %+v, %v", res, err)
	}

	hits, err := ix.Search(ctx, "retry backoff", 90*24*time.Hour, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]bool{}
	for _, h := range hits {
		got[h.Issue.Number] = true
		if h.Issue.Source != "index" || h.Score <= 0 {
			t.Errorf("hit = %+v", h)
		}
	}
	// Both "backoff" issues rank first (the rarer term dominates BM25);
	// "retrys" stems to the same token as "retry" (porter); #3 was closed
	// too long ago.
	top := map[int]bool{hits[0].Issue.Number: true, hits[1].Issue.Number: true}
	if !top[1] || !top[4] || !got[2] || !got[5] || got[3] || len(hits) != 4 {
		t.Errorf("ranking = %+v", hits)
	}
	if open, _ := ix.Search(ctx, "backoff", 0, now, 10); len(open) != 1 || open[0].Issue.Number != 1 {
		t.Errorf("open only = %+v", open)
	}

	// Incremental: only updates after the cursor are fetched; edits replace.
	l.all[1].is.Title, l.all[1].is.Body, l.all[1].updated = "Retry backoff is too aggressive", "jitter please", day(1)
	l.calls = nil
	res, err = ix.Sync(ctx, l, forge.Repo{}, now)
	if err != nil || res.Full || res.Fetched != 1 || res.Issues != 5 || !l.calls[0].Equal(day(5)) {
		t.Fatalf("incremental = %+v, %v (calls %v)", res, err, l.calls)
	}
	if hits, _ := ix.Search(ctx, "jitter", 0, now, 10); len(hits) != 1 || hits[0].Issue.Number != 2 {
		t.Errorf("updated issue not re-indexed: %+v", hits)
	}
	if hits, _ := ix.Search(ctx, "typo", 0, now, 10); len(hits) != 0 {
		t.Errorf("stale text still indexed: %+v", hits)
	}
}

func TestMatchQuery(t *testing.T) {
	cases := map[string]string{
		"retry backoff":                 `"retry" OR "backoff"`,
		`title:x OR "y" NEAR(a b) -c *`: `"title" OR "or" OR "near"`,
		"a I x":                         ``,
		"Retry retry RETRY":             `"retry"`,
		"ünïcode fix-412":               `"ünïcode" OR "fix" OR "412"`,
	}
	for in, want := range cases {
		if got := MatchQuery(in); got != want {
			t.Errorf("MatchQuery(%q) = %s, want %s", in, got, want)
		}
	}
	ix, err := Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ix.Close() }()
	if _, err := ix.Search(context.Background(), "? !", 0, time.Now(), 5); err != ErrEmptyQuery {
		t.Errorf("empty query err = %v", err)
	}
	// Hostile input is inert, not an FTS syntax error.
	if _, err := ix.Search(context.Background(), `") OR 1=1; DROP TABLE issues; --`, 0, time.Now(), 5); err != nil {
		t.Errorf("hostile query: %v", err)
	}
}
