// SPDX-License-Identifier: MIT

package github

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestListOpenPRsByBase(t *testing.T) {
	var base, state string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/octo/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		base, state = r.URL.Query().Get("base"), r.URL.Query().Get("state")
		fmt.Fprint(w, `[{"number":5,"title":"part 2","html_url":"u5","head":{"ref":"feat-2","repo":{"full_name":"octo/repo"}},"base":{"ref":"feat-1"}},
			{"number":6,"title":"fork","head":{"ref":"x","repo":{"full_name":"someone/repo"}},"base":{"ref":"feat-1"}}]`)
	})
	prs, err := newTestProvider(t, mux).ListOpenPRsByBase(context.Background(), ref.Repo, "feat-1")
	if err != nil || len(prs) != 1 || prs[0].Ref.Number != 5 || prs[0].BaseBranch != "feat-1" || base != "feat-1" || state != "open" {
		t.Fatalf("prs = %+v err=%v base=%q", prs, err, base)
	}
}
