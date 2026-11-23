// SPDX-License-Identifier: MIT

package github

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestListIssuesUpdatedSince(t *testing.T) {
	var query map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/octo/repo/issues", func(w http.ResponseWriter, r *http.Request) {
		query = map[string]string{}
		for k, v := range r.URL.Query() {
			query[k] = v[0]
		}
		w.Header().Set("Link", `<https://x/api/v3/repos/octo/repo/issues?page=2>; rel="next"`)
		fmt.Fprint(w, `[{"number":1,"title":"A","state":"open","updated_at":"2026-11-01T00:00:00Z"},
			{"number":2,"title":"PR","pull_request":{"url":"x"},"updated_at":"2026-11-02T00:00:00Z"},
			{"number":3,"title":"C","state":"closed","updated_at":"2026-11-03T00:00:00Z","closed_at":"2026-11-03T00:00:00Z"}]`)
	})
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	is, up, cl, more, err := newTestProvider(t, mux).ListIssuesUpdatedSince(context.Background(), ref.Repo, since, 1)
	if err != nil || len(is) != 2 || is[0].Number != 1 || is[1].Number != 3 || !more {
		t.Fatalf("issues = %+v more=%v err=%v", is, more, err)
	}
	if up[1].Day() != 3 || cl[0] != nil || cl[1] == nil {
		t.Errorf("times = %v %v", up, cl)
	}
	if query["state"] != "all" || query["sort"] != "updated" || query["direction"] != "asc" || query["since"] != "2026-10-01T00:00:00Z" || query["per_page"] != "100" {
		t.Errorf("query = %v", query)
	}
}
