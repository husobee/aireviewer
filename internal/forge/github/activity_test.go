// SPDX-License-Identifier: MIT

package github

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/husobee/aireviewer/internal/forge"
)

func TestActivity(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/octo/repo/pulls/12", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"number":12,"user":{"login":"alice"},"created_at":"2026-11-01T09:00:00Z",
			"merged_at":"2026-11-02T15:00:00Z","closed_at":"2026-11-02T15:00:00Z"}`)
	})
	mux.HandleFunc("GET /api/v3/repos/octo/repo/pulls/12/reviews", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"user":{"login":"bob"},"state":"CHANGES_REQUESTED","body":"see inline\n\n---\n<sub>Drafted with aireviewer (AI-assisted).</sub>",
			"submitted_at":"2026-11-01T13:00:00Z","html_url":"r1"},
			{"user":{"login":"carol"},"state":"APPROVED","body":"","submitted_at":"2026-11-02T14:00:00Z"}]`)
	})
	mux.HandleFunc("GET /api/v3/repos/octo/repo/pulls/12/comments", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"user":{"login":"bob"},"body":"nil check?","path":"a.go","line":7,"created_at":"2026-11-01T13:00:01Z","html_url":"c1"}]`)
	})
	mux.HandleFunc("GET /api/v3/repos/octo/repo/issues/12/comments", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"user":{"login":"bob"},"body":"<!-- aireviewer:v1 run=x head=y -->\nsummary","created_at":"2026-11-01T10:00:00Z"},
			{"user":{"login":"alice"},"body":"thanks","created_at":"2026-11-01T16:00:00Z"}]`)
	})
	mux.HandleFunc("GET /api/v3/repos/octo/repo/pulls/12/commits", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"sha":"aaa","author":{"login":"alice"},"commit":{"message":"first\n\nbody","committer":{"date":"2026-11-01T08:00:00Z"}}},
			{"sha":"bbb","author":{"login":"alice"},"commit":{"message":"address review","committer":{"date":"2026-11-02T10:00:00Z"}}}]`)
	})
	a, err := newTestProvider(t, mux).Activity(context.Background(), &forge.PR{Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	if a.Author != "alice" || a.MergedAt == nil || a.CreatedAt.Hour() != 9 || len(a.Events) != 7 {
		t.Fatalf("activity = %+v", a)
	}
	kinds := ""
	for i, e := range a.Events {
		if i > 0 && e.At.Before(a.Events[i-1].At) {
			t.Errorf("events not sorted at %d", i)
		}
		kinds += e.Kind + " "
	}
	if kinds != "commit summary review inline comment commit review " {
		t.Errorf("kinds = %q", kinds)
	}
	rev := a.Events[2]
	if rev.State != "changes_requested" || !rev.AIAssisted || rev.Author != "bob" {
		t.Errorf("review = %+v", rev)
	}
	if in := a.Events[3]; in.Path != "a.go" || in.Line != 7 || in.AIAssisted {
		t.Errorf("inline = %+v", in)
	}
	if c := a.Events[0]; c.SHA != "aaa" || c.Body != "first" {
		t.Errorf("commit = %+v", c)
	}
	if last := a.Events[6]; last.State != "approved" || last.Author != "carol" {
		t.Errorf("approval = %+v", last)
	}
}
