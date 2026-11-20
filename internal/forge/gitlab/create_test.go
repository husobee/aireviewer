// SPDX-License-Identifier: MIT

package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/husobee/aireviewer/internal/forge"
)

const projPath = "/api/v4/projects/grp%2Fsub%2Fproj"

func TestCreateSideGitLab(t *testing.T) {
	var mrReq, issueReq map[string]any
	var searchQ, searchState, srcBranch string
	old := time.Now().Add(-400 * 24 * time.Hour).UTC().Format(time.RFC3339)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.EscapedPath()
		switch {
		case r.Method == "GET" && p == projPath:
			fmt.Fprint(w, `{"id":1,"default_branch":"develop"}`)
		case r.Method == "GET" && p == projPath+"/merge_requests":
			srcBranch = r.URL.Query().Get("source_branch")
			fmt.Fprint(w, `[{"iid":7,"project_id":1,"source_project_id":1,"source_branch":"feat"}]`)
		case r.Method == "GET" && p == mrPath:
			fmt.Fprint(w, `{"iid":7,"title":"Draft: retry","state":"opened","source_branch":"feat","target_branch":"main","web_url":"https://gitlab.example.com/grp/sub/proj/-/merge_requests/7"}`)
		case r.Method == "GET" && p == projPath+"/issues":
			searchQ, searchState = r.URL.Query().Get("search"), r.URL.Query().Get("state")
			fmt.Fprintf(w, `[{"id":10,"iid":3,"title":"Retry storm","state":"opened","web_url":"u3"},
				{"id":11,"iid":4,"title":"Old","state":"closed","closed_at":%q}]`, old)
		case r.Method == "POST" && p == projPath+"/issues":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &issueReq)
			fmt.Fprint(w, `{"id":12,"iid":42,"title":"Track metrics","state":"opened","web_url":"u42"}`)
		case r.Method == "POST" && p == projPath+"/merge_requests":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &mrReq)
			fmt.Fprint(w, `{"id":99,"iid":7,"title":"Draft: Add backoff","web_url":"https://gitlab.example.com/grp/sub/proj/-/merge_requests/7"}`)
		default:
			http.NotFound(w, r)
		}
	})
	p := newTestProvider(t, h)
	ctx := context.Background()

	if b, err := p.DefaultBranch(ctx, ref.Repo); err != nil || b != "develop" {
		t.Errorf("default branch = %q, %v", b, err)
	}
	if pr, err := p.FindOpenPR(ctx, ref.Repo, "feat"); err != nil || pr == nil || pr.Ref.Number != 7 || srcBranch != "feat" {
		t.Errorf("find = %+v, %v", pr, err)
	}
	is, err := p.SearchIssues(ctx, ref.Repo, forge.IssueSearch{Query: "retry", ClosedWithin: 90 * 24 * time.Hour})
	if err != nil || len(is) != 1 || is[0].Number != 3 || is[0].State != "open" || searchQ != "retry" || searchState != "" {
		t.Errorf("search = %+v, %v (q=%q state=%q)", is, err, searchQ, searchState)
	}
	if _, err := p.SearchIssues(ctx, ref.Repo, forge.IssueSearch{Query: "retry"}); err != nil || searchState != "opened" {
		t.Errorf("open-only search state = %q, %v", searchState, err)
	}
	ni, err := p.CreateIssue(ctx, ref.Repo, forge.NewIssue{Title: "Track metrics", Body: "b", Labels: []string{"metrics"}})
	if err != nil || ni.Number != 42 || issueReq["title"] != "Track metrics" || issueReq["labels"] != "metrics" {
		t.Errorf("issue = %+v, %v, req=%v", ni, err, issueReq)
	}
	pr, err := p.CreatePR(ctx, ref.Repo, forge.PRSpec{Title: "Add backoff", Body: "Closes #42", Base: "main", Head: "feat", Draft: true})
	if err != nil || pr.Ref.Number != 7 {
		t.Fatalf("mr = %+v, %v", pr, err)
	}
	if mrReq["title"] != "Draft: Add backoff" || mrReq["source_branch"] != "feat" || mrReq["target_branch"] != "main" || mrReq["description"] != "Closes #42" {
		t.Errorf("mr request = %v", mrReq)
	}
	if draftTitle("Draft: x", true) != "Draft: x" || draftTitle("x", false) != "x" {
		t.Error("draftTitle")
	}
}
