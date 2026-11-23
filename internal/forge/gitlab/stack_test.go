// SPDX-License-Identifier: MIT

package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestListOpenPRsByBaseGitLab(t *testing.T) {
	var target string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != projPath+"/merge_requests" {
			http.NotFound(w, r)
			return
		}
		target = r.URL.Query().Get("target_branch")
		fmt.Fprint(w, `[{"id":1,"iid":8,"title":"part 2","project_id":1,"source_project_id":1,"source_branch":"feat-2","target_branch":"feat-1","web_url":"u8","author":{"username":"dee"}},
			{"id":2,"iid":9,"title":"fork","project_id":1,"source_project_id":77,"source_branch":"x","target_branch":"feat-1"}]`)
	})
	prs, err := newTestProvider(t, h).ListOpenPRsByBase(context.Background(), ref.Repo, "feat-1")
	if err != nil || len(prs) != 1 || prs[0].Ref.Number != 8 || prs[0].HeadBranch != "feat-2" || prs[0].Author != "dee" || target != "feat-1" {
		t.Fatalf("prs = %+v err=%v target=%q", prs, err, target)
	}
}
