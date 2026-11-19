// SPDX-License-Identifier: MIT

package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/husobee/aireviewer/internal/forge"
)

type recorder struct {
	mu    sync.Mutex
	calls []string
	notes []map[string]any
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.calls = append(r.calls, s)
	r.mu.Unlock()
}

func TestSubmitReviewGitLab(t *testing.T) {
	rec := &recorder{}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.EscapedPath(), mrPath)
		rec.add(r.Method + " " + p)
		switch {
		case r.Method == "POST" && p == "/draft_notes":
			b, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			rec.notes = append(rec.notes, m)
			fmt.Fprint(w, `{"id":1}`)
		case r.Method == "POST" && p == "/draft_notes/bulk_publish":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == "POST" && p == "/approve":
			fmt.Fprint(w, `{"id":1}`)
		default:
			http.NotFound(w, r)
		}
	})
	pr := &forge.PR{Ref: ref, WebURL: "https://gitlab.example.com/grp/sub/proj/-/merge_requests/7"}
	review := forge.Review{Verdict: forge.VerdictApprove, Body: "LGTM", HeadSHA: "hhh", BaseSHA: "bbb", StartSHA: "sss",
		Comments: []forge.ReviewComment{
			{Path: "a.go", Side: "new", Line: 5, Body: "added line"},
			{Path: "a.go", Side: "new", Line: 9, ContextOldLine: 8, Body: "context line"},
			{Path: "b.go", Side: "old", Line: 3, Body: "deleted line"},
		}}
	res, err := newTestProvider(t, h).SubmitReview(context.Background(), pr, review)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /draft_notes", "POST /draft_notes", "POST /draft_notes", "POST /draft_notes", "POST /draft_notes/bulk_publish", "POST /approve"}
	if strings.Join(rec.calls, ",") != strings.Join(want, ",") {
		t.Errorf("calls = %v", rec.calls)
	}
	pos := func(i int) map[string]any { return rec.notes[i]["position"].(map[string]any) }
	if p := pos(0); p["new_line"] != 5.0 || p["old_line"] != nil || p["head_sha"] != "hhh" || p["start_sha"] != "sss" || p["position_type"] != "text" {
		t.Errorf("note 0 position = %v", p)
	}
	if p := pos(1); p["new_line"] != 9.0 || p["old_line"] != 8.0 {
		t.Errorf("context position = %v", p)
	}
	if p := pos(2); p["old_line"] != 3.0 || p["new_line"] != nil {
		t.Errorf("old-side position = %v", p)
	}
	if rec.notes[3]["note"] != "LGTM" || rec.notes[3]["position"] != nil {
		t.Errorf("summary note = %v", rec.notes[3])
	}
	if len(res.Notes) != 1 || res.Notes[0] != "approved" {
		t.Errorf("notes = %v", res.Notes)
	}

	// request_changes: no approve call, explicit note.
	rec2 := &recorder{}
	h2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.EscapedPath(), mrPath)
		rec2.add(r.Method + " " + p)
		if p == "/draft_notes" {
			b, _ := io.ReadAll(r.Body)
			var m map[string]any
			_ = json.Unmarshal(b, &m)
			rec2.notes = append(rec2.notes, m)
			fmt.Fprint(w, `{"id":1}`)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	review.Verdict, review.Comments = forge.VerdictRequestChanges, nil
	res, err = newTestProvider(t, h2).SubmitReview(context.Background(), pr, review)
	if err != nil || strings.Contains(strings.Join(rec2.calls, ","), "approve") ||
		!strings.HasPrefix(rec2.notes[0]["note"].(string), "**Changes requested.**") || !strings.Contains(res.Notes[0], "no request-changes call") {
		t.Errorf("request changes: %v calls=%v notes=%v res=%+v", err, rec2.calls, rec2.notes, res)
	}
}

func TestMarkerAndReviewersGitLab(t *testing.T) {
	var updated, reviewerIDs string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		switch {
		case r.Method == "GET" && p == mrPath+"/notes":
			fmt.Fprint(w, `[{"id":3,"body":"<!-- aireviewer:v1 --> x","system":false,"author":{"username":"me"}},
				{"id":4,"body":"<!-- aireviewer:v1 --> mentioned","system":true,"author":{"username":"bot"}}]`)
		case r.Method == "PUT" && p == mrPath+"/notes/3":
			b, _ := io.ReadAll(r.Body)
			updated = string(b)
			fmt.Fprint(w, `{"id":3}`)
		case r.Method == "GET" && p == mrPath:
			fmt.Fprint(w, `{"iid":7,"reviewers":[{"id":11,"username":"old"}]}`)
		case r.Method == "GET" && p == "/api/v4/users":
			if r.URL.Query().Get("username") == "alice" {
				fmt.Fprint(w, `[{"id":22,"username":"alice"}]`)
				return
			}
			fmt.Fprint(w, `[]`)
		case r.Method == "PUT" && p == mrPath:
			b, _ := io.ReadAll(r.Body)
			reviewerIDs = string(b)
			fmt.Fprint(w, `{"iid":7}`)
		case r.Method == "GET" && p == "/api/v4/user":
			fmt.Fprint(w, `{"id":1,"username":"me"}`)
		default:
			http.NotFound(w, r)
		}
	})
	p := newTestProvider(t, h)
	pr := &forge.PR{Ref: ref, WebURL: "https://gl/mr/7"}
	ctx := context.Background()
	mc, err := p.FindMarkerComment(ctx, pr, "<!-- aireviewer:v1 -->")
	if err != nil || mc == nil || mc.ID != 3 || mc.URL != "https://gl/mr/7#note_3" {
		t.Fatalf("find = %+v, %v", mc, err)
	}
	if _, err := p.UpsertMarkerComment(ctx, pr, "<!-- aireviewer:v1 -->", "fresh"); err != nil || !strings.Contains(updated, "fresh") {
		t.Errorf("upsert: %v %q", err, updated)
	}
	res, err := p.RequestReviewers(ctx, pr, []string{"@alice", "@ghost", "@grp/team"})
	if err != nil || !strings.Contains(reviewerIDs, "[11,22]") || len(res.Notes) != 2 {
		t.Errorf("reviewers: %v body=%s notes=%v", err, reviewerIDs, res.Notes)
	}
	if u, err := p.CurrentUser(ctx, ref.Repo); err != nil || u != "me" {
		t.Errorf("current user = %q %v", u, err)
	}
}
