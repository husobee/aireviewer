// SPDX-License-Identifier: MIT

package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/husobee/aireviewer/internal/forge"
)

func TestSubmitReview(t *testing.T) {
	var got map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v3/repos/octo/repo/pulls/12/reviews", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		fmt.Fprint(w, `{"id":1,"html_url":"https://github.com/octo/repo/pull/12#pullrequestreview-1"}`)
	})
	pr := &forge.PR{Ref: ref}
	res, err := newTestProvider(t, mux).SubmitReview(context.Background(), pr, forge.Review{
		Verdict: forge.VerdictRequestChanges, Body: "summary", HeadSHA: "bbb",
		Comments: []forge.ReviewComment{
			{Path: "a.go", Side: "new", StartLine: 4, Line: 5, Body: "multi"},
			{Path: "b.go", OldPath: "old_b.go", Side: "old", Line: 9, Body: "deleted line"},
		}})
	if err != nil || !strings.Contains(res.URL, "pullrequestreview-1") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if got["event"] != "REQUEST_CHANGES" || got["commit_id"] != "bbb" || got["body"] != "summary" {
		t.Errorf("review = %v", got)
	}
	cs := got["comments"].([]any)
	c0, c1 := cs[0].(map[string]any), cs[1].(map[string]any)
	if c0["path"] != "a.go" || c0["line"] != 5.0 || c0["start_line"] != 4.0 || c0["side"] != "RIGHT" || c0["start_side"] != "RIGHT" {
		t.Errorf("comment 0 = %v", c0)
	}
	if c1["path"] != "old_b.go" || c1["side"] != "LEFT" || c1["start_line"] != nil {
		t.Errorf("comment 1 = %v", c1)
	}
}

func TestMarkerCommentAndReviewers(t *testing.T) {
	var edited, created, requested string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/octo/repo/issues/12/comments", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"id":5,"body":"hello","user":{"login":"x"}},{"id":7,"body":"<!-- aireviewer:v1 --> old","user":{"login":"me"},"html_url":"u7"}]`)
	})
	mux.HandleFunc("PATCH /api/v3/repos/octo/repo/issues/comments/7", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		edited = string(b)
		fmt.Fprint(w, `{"id":7,"html_url":"u7"}`)
	})
	mux.HandleFunc("POST /api/v3/repos/octo/repo/issues/12/comments", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		created = string(b)
		fmt.Fprint(w, `{"id":8,"html_url":"u8"}`)
	})
	mux.HandleFunc("POST /api/v3/repos/octo/repo/pulls/12/requested_reviewers", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		requested = string(b)
		fmt.Fprint(w, `{"number":12,"html_url":"pr12"}`)
	})
	mux.HandleFunc("GET /api/v3/user", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"login":"me"}`) })
	p := newTestProvider(t, mux)
	pr := &forge.PR{Ref: ref}
	ctx := context.Background()

	mc, err := p.FindMarkerComment(ctx, pr, "<!-- aireviewer:v1 -->")
	if err != nil || mc == nil || mc.ID != 7 || mc.Author != "me" {
		t.Fatalf("find = %+v, %v", mc, err)
	}
	if res, err := p.UpsertMarkerComment(ctx, pr, "<!-- aireviewer:v1 -->", "new body"); err != nil || res.URL != "u7" || !strings.Contains(edited, "new body") || created != "" {
		t.Errorf("upsert existing: %+v %v edited=%q created=%q", res, err, edited, created)
	}
	if _, err := p.UpsertMarkerComment(ctx, pr, "<!-- other -->", "fresh"); err != nil || !strings.Contains(created, "fresh") {
		t.Errorf("upsert new: %v created=%q", err, created)
	}
	if _, err := p.RequestReviewers(ctx, pr, []string{"@alice", "@org/team-x"}); err != nil ||
		!strings.Contains(requested, `"reviewers":["alice"]`) || !strings.Contains(requested, `"team_reviewers":["team-x"]`) {
		t.Errorf("request reviewers: %v body=%s", err, requested)
	}
	if u, err := p.CurrentUser(ctx, ref.Repo); err != nil || u != "me" {
		t.Errorf("current user = %q, %v", u, err)
	}
}

func TestSuggestionBlock(t *testing.T) {
	if got := forge.SuggestionBlock(forge.GitHub, "a\nb\n", 4, 5); got != "```suggestion\na\nb\n```" {
		t.Errorf("github = %q", got)
	}
	if got := forge.SuggestionBlock(forge.GitLab, "a", 4, 6); got != "```suggestion:-2+0\na\n```" {
		t.Errorf("gitlab = %q", got)
	}
	if got := forge.SuggestionBlock(forge.GitHub, "x := \"```\"", 1, 1); !strings.HasPrefix(got, "````suggestion") {
		t.Errorf("fence not escalated: %q", got)
	}
}
