// SPDX-License-Identifier: MIT

package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestListIssuesUpdatedSinceGitLab(t *testing.T) {
	var q map[string]string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != projPath+"/issues" {
			http.NotFound(w, r)
			return
		}
		q = map[string]string{}
		for k, v := range r.URL.Query() {
			q[k] = v[0]
		}
		w.Header().Set("X-Next-Page", "")
		fmt.Fprint(w, `[{"id":40,"iid":4,"title":"Retry","state":"opened","updated_at":"2026-11-01T00:00:00Z"},
			{"id":50,"iid":5,"title":"Old","state":"closed","updated_at":"2026-11-02T00:00:00Z","closed_at":"2026-11-02T00:00:00Z"}]`)
	})
	p := newTestProvider(t, h)
	is, up, cl, more, err := p.ListIssuesUpdatedSince(context.Background(), ref.Repo, time.Time{}, 1)
	if err != nil || len(is) != 2 || is[0].State != "open" || more || cl[0] != nil || cl[1] == nil || up[0].Day() != 1 {
		t.Fatalf("issues = %+v %v %v more=%v err=%v", is, up, cl, more, err)
	}
	if q["order_by"] != "updated_at" || q["sort"] != "asc" || q["updated_after"] != "" {
		t.Errorf("first sync query = %v", q)
	}
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	if _, _, _, _, err := p.ListIssuesUpdatedSince(context.Background(), ref.Repo, since, 1); err != nil || q["updated_after"] == "" {
		t.Errorf("incremental query = %v, %v", q, err)
	}
}
