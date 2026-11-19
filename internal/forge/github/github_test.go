// SPDX-License-Identifier: MIT

package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/husobee/aireviewer/internal/forge"
)

func newTestProvider(t *testing.T, mux *http.ServeMux) *Provider {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	p, err := New(Options{Host: "github.com", BaseURL: srv.URL + "/api/v3/"})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var ref = forge.PRRef{Repo: forge.Repo{Forge: forge.GitHub, Host: "github.com", Project: "octo/repo"}, Number: 12}

func TestGetPR(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/octo/repo/pulls/12", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"number":12,"title":"Add backoff","body":"Fixes #3","state":"closed","merged":true,"draft":false,
			"user":{"login":"alice"},"html_url":"https://github.com/octo/repo/pull/12","created_at":"2026-11-01T10:00:00Z",
			"base":{"ref":"main","sha":"aaa"},"head":{"ref":"feat/backoff","sha":"bbb"},
			"labels":[{"name":"enhancement"}],"requested_reviewers":[{"login":"bob"}]}`)
	})
	pr, err := newTestProvider(t, mux).GetPR(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if pr.State != forge.StateMerged || pr.Author != "alice" || pr.BaseSHA != "aaa" || pr.HeadSHA != "bbb" ||
		pr.HeadBranch != "feat/backoff" || pr.Noun != "pull request" || len(pr.Labels) != 1 || pr.Reviewers[0] != "bob" {
		t.Errorf("unexpected PR: %+v", pr)
	}
}

func TestGetDiffPaginates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/octo/repo/pulls/12/files", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			fmt.Fprint(w, `[{"filename":"new.go","status":"renamed","previous_filename":"old.go","additions":0,"deletions":0}]`)
			return
		}
		w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?page=2>; rel="next"`, r.Host, r.URL.Path))
		fmt.Fprint(w, `[{"filename":"a.go","status":"modified","additions":3,"deletions":1,"patch":"@@ -1 +1 @@"},
			{"filename":"big.bin","status":"added","additions":10,"deletions":0}]`)
	})
	d, err := newTestProvider(t, mux).GetDiff(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	if d.Stats != (forge.DiffStats{Files: 3, Additions: 13, Deletions: 1}) {
		t.Errorf("stats = %+v", d.Stats)
	}
	if !d.Files[1].Truncated || d.Files[1].Status != forge.FileAdded {
		t.Errorf("big.bin = %+v", d.Files[1])
	}
	if d.Files[2].Status != forge.FileRenamed || d.Files[2].OldPath != "old.go" {
		t.Errorf("rename = %+v", d.Files[2])
	}
}

func TestNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/octo/repo/pulls/12", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	_, err := newTestProvider(t, mux).GetPR(context.Background(), ref)
	if !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}
