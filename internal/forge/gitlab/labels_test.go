// SPDX-License-Identifier: MIT

package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestLabelledFixCommitsGitLab(t *testing.T) {
	var labelsQueried []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		switch p {
		case projPath + "/merge_requests":
			l := r.URL.Query().Get("labels")
			labelsQueried = append(labelsQueried, l)
			if l == "bug" {
				// MR 5 merged in the window (both labels), MR 6 merged too late.
				fmt.Fprint(w, `[{"iid":5,"merged_at":"2026-03-01T00:00:00Z","merge_commit_sha":"m5","squash_commit_sha":""},
					{"iid":6,"merged_at":"2027-01-01T00:00:00Z","merge_commit_sha":"m6"}]`)
				return
			}
			fmt.Fprint(w, `[{"iid":5,"merged_at":"2026-03-01T00:00:00Z","merge_commit_sha":"m5"}]`)
		case projPath + "/merge_requests/5/commits":
			fmt.Fprint(w, `[{"id":"c5a"},{"id":"c5b"}]`)
		default:
			http.NotFound(w, r)
		}
	})
	since := time.Date(2025, 11, 20, 0, 0, 0, 0, time.UTC)
	got, prs, err := newTestProvider(t, h).LabelledFixCommits(context.Background(), ref.Repo, []string{"bug", "incident"}, since, since.AddDate(1, 0, 0), 100)
	if err != nil || prs != 1 || !got["m5"] || !got["c5a"] || !got["c5b"] || got["m6"] || len(got) != 3 {
		t.Fatalf("got %v prs=%d err=%v", got, prs, err)
	}
	if len(labelsQueried) != 2 || labelsQueried[0] != "bug" || labelsQueried[1] != "incident" {
		t.Errorf("labels queried separately (OR) = %v", labelsQueried)
	}
}
