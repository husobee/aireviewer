// SPDX-License-Identifier: MIT

package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/husobee/aireviewer/internal/forge"
)

func newTestProvider(t *testing.T, h http.Handler) *Provider {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	p, err := New(Options{Host: "gitlab.example.com", BaseURL: srv.URL + "/api/v4"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var ref = forge.PRRef{Repo: forge.Repo{Forge: forge.GitLab, Host: "gitlab.example.com", Project: "grp/sub/proj"}, Number: 7}

// The project path must be URL-encoded as a single segment.
const mrPath = "/api/v4/projects/grp%2Fsub%2Fproj/merge_requests/7"

func TestGetPR(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != mrPath {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"iid":7,"title":"Draft: retry","description":"Closes #3","state":"opened","draft":true,
			"source_branch":"feat/retry","target_branch":"main","sha":"bbb","web_url":"https://gitlab.example.com/grp/sub/proj/-/merge_requests/7",
			"author":{"username":"carol"},"reviewers":[{"username":"dave"}],"labels":["backend"],"created_at":"2026-11-01T10:00:00Z",
			"diff_refs":{"base_sha":"aaa","head_sha":"bbb","start_sha":"ccc"}}`)
	})
	pr, err := newTestProvider(t, h).GetPR(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if pr.State != forge.StateOpen || !pr.Draft || pr.Author != "carol" || pr.StartSHA != "ccc" || pr.BaseSHA != "aaa" ||
		pr.HeadBranch != "feat/retry" || pr.Noun != "merge request" || pr.Reviewers[0] != "dave" {
		t.Errorf("unexpected PR: %+v", pr)
	}
}

func TestGetDiff(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != mrPath+"/diffs" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `[{"old_path":"gone.go","new_path":"gone.go","deleted_file":true,"diff":"@@ -1,2 +0,0 @@\n-a\n-b\n"}]`)
			return
		}
		w.Header().Set("X-Next-Page", "2")
		fmt.Fprint(w, `[{"old_path":"a.go","new_path":"a.go","diff":"@@ -1,2 +1,3 @@\n ctx\n-x\n+y\n+z\n"},
			{"old_path":"old.go","new_path":"new.go","renamed_file":true,"diff":""},
			{"old_path":"huge.json","new_path":"huge.json","new_file":true,"too_large":true,"diff":""}]`)
	})
	d, err := newTestProvider(t, h).GetDiff(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if d.Stats != (forge.DiffStats{Files: 4, Additions: 2, Deletions: 3}) {
		t.Errorf("stats = %+v", d.Stats)
	}
	if d.Files[1].Status != forge.FileRenamed || d.Files[1].OldPath != "old.go" {
		t.Errorf("rename = %+v", d.Files[1])
	}
	if !d.Files[2].Truncated || d.Files[2].Status != forge.FileAdded {
		t.Errorf("huge = %+v", d.Files[2])
	}
	if d.Files[3].Status != forge.FileDeleted || d.Files[3].Path != "gone.go" {
		t.Errorf("deleted = %+v", d.Files[3])
	}
}

func TestNotFound(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"404 Not found"}`, http.StatusNotFound)
	})
	_, err := newTestProvider(t, h).GetPR(context.Background(), ref)
	if !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestLinkedIssues(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case mrPath + "/closes_issues":
			fmt.Fprint(w, `[{"id":103,"iid":3,"title":"Retries","description":"AC: backoff","state":"opened","web_url":"u3",
				"labels":["bug"],"references":{"full":"grp/sub/proj#3"}}]`)
		case "/api/v4/projects/grp%2Fsub%2Fproj/issues/8":
			fmt.Fprint(w, `{"id":108,"iid":8,"title":"Docs","description":"d","state":"closed","web_url":"u8","references":{"full":"grp/sub/proj#8"}}`)
		default:
			http.NotFound(w, r)
		}
	})
	pr := &forge.PR{Ref: ref, Body: "Closes #3 and fixes #8; fixes #999"}
	got, err := newTestProvider(t, h).LinkedIssues(context.Background(), pr)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Ref != "grp/sub/proj#3" || got[0].State != "open" || got[0].Source != "closing_reference" ||
		got[1].Ref != "grp/sub/proj#8" || got[1].Source != "body_keyword" {
		t.Errorf("linked = %+v", got)
	}
}
