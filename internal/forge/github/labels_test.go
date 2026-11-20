// SPDX-License-Identifier: MIT

package github

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestLabelledFixCommits(t *testing.T) {
	var q string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/search/issues", func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query().Get("q")
		fmt.Fprint(w, `{"total_count":1,"items":[{"number":7,"pull_request":{"url":"x"}}]}`)
	})
	mux.HandleFunc("GET /api/v3/repos/octo/repo/pulls/7", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"number":7,"merge_commit_sha":"squash1"}`)
	})
	mux.HandleFunc("GET /api/v3/repos/octo/repo/pulls/7/commits", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"sha":"c1"},{"sha":"c2"}]`)
	})
	since := time.Date(2025, 11, 20, 0, 0, 0, 0, time.UTC)
	got, prs, err := newTestProvider(t, mux).LabelledFixCommits(context.Background(), ref.Repo, []string{"bug", "incident"}, since, since.AddDate(1, 0, 0), 100)
	if err != nil || prs != 1 || !got["squash1"] || !got["c1"] || !got["c2"] || len(got) != 3 {
		t.Fatalf("got %v prs=%d err=%v", got, prs, err)
	}
	want := `repo:octo/repo is:pr is:merged label:"bug","incident" merged:2025-11-20..2026-11-20`
	if q != want {
		t.Errorf("query = %q, want %q", q, want)
	}
}
