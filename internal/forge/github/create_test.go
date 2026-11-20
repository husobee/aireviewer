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
	"time"

	"github.com/husobee/aireviewer/internal/forge"
)

func TestCreateSideReads(t *testing.T) {
	var searchQ, listHead string
	old := time.Now().Add(-400 * 24 * time.Hour).UTC().Format(time.RFC3339)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/repos/octo/repo", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"default_branch":"trunk"}`)
	})
	mux.HandleFunc("GET /api/v3/repos/octo/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		listHead = r.URL.Query().Get("head")
		fmt.Fprint(w, `[{"number":9,"html_url":"https://github.com/octo/repo/pull/9","head":{"ref":"feat","sha":"h","repo":{"full_name":"octo/repo"}},"base":{"ref":"trunk"}}]`)
	})
	mux.HandleFunc("GET /api/v3/search/issues", func(w http.ResponseWriter, r *http.Request) {
		searchQ = r.URL.Query().Get("q")
		fmt.Fprintf(w, `{"total_count":3,"items":[
			{"number":3,"title":"Retry storm","state":"open","html_url":"u3"},
			{"number":4,"title":"Old retry bug","state":"closed","closed_at":%q,"html_url":"u4"},
			{"number":5,"title":"A PR","state":"open","pull_request":{"url":"x"}}]}`, old)
	})
	mux.HandleFunc("GET /api/v3/repos/octo/repo/issues/5", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"number":5,"title":"A PR","pull_request":{"url":"x"}}`)
	})
	p := newTestProvider(t, mux)
	ctx := context.Background()

	if b, err := p.DefaultBranch(ctx, ref.Repo); err != nil || b != "trunk" {
		t.Errorf("default branch = %q, %v", b, err)
	}
	pr, err := p.FindOpenPR(ctx, ref.Repo, "feat")
	if err != nil || pr == nil || pr.Ref.Number != 9 || listHead != "octo:feat" {
		t.Errorf("find = %+v, %v (head=%q)", pr, err, listHead)
	}
	is, err := p.SearchIssues(ctx, ref.Repo, forge.IssueSearch{Query: "retry", ClosedWithin: 90 * 24 * time.Hour})
	if err != nil || len(is) != 1 || is[0].Number != 3 || is[0].Ref != "octo/repo#3" {
		t.Errorf("search = %+v, %v", is, err)
	}
	if searchQ != "repo:octo/repo is:issue retry" {
		t.Errorf("query = %q", searchQ)
	}
	if _, err := p.SearchIssues(ctx, ref.Repo, forge.IssueSearch{Query: "retry"}); err != nil || !strings.HasSuffix(searchQ, " is:open") {
		t.Errorf("open-only query = %q, %v", searchQ, err)
	}
	if _, err := p.GetIssue(ctx, ref.Repo, 5); err == nil || !strings.Contains(err.Error(), "pull request") {
		t.Errorf("PR accepted as issue: %v", err)
	}
}

func TestCreateIssueAndPR(t *testing.T) {
	var issueReq, prReq map[string]any
	var labels []string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v3/repos/octo/repo/issues", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &issueReq)
		fmt.Fprint(w, `{"number":42,"title":"Track metrics","state":"open","html_url":"https://github.com/octo/repo/issues/42"}`)
	})
	mux.HandleFunc("POST /api/v3/repos/octo/repo/pulls", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &prReq)
		fmt.Fprint(w, `{"number":13,"title":"Add backoff","draft":true,"html_url":"https://github.com/octo/repo/pull/13",
			"user":{"login":"ada"},"base":{"ref":"main","sha":"a"},"head":{"ref":"feat","sha":"b"}}`)
	})
	mux.HandleFunc("POST /api/v3/repos/octo/repo/issues/13/labels", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &labels)
		fmt.Fprint(w, `[{"name":"enhancement"}]`)
	})
	p := newTestProvider(t, mux)
	ctx := context.Background()

	is, err := p.CreateIssue(ctx, ref.Repo, forge.NewIssue{Title: "Track metrics", Body: "follow-up", Labels: []string{"metrics"}})
	if err != nil || is.Number != 42 || issueReq["title"] != "Track metrics" || issueReq["body"] != "follow-up" {
		t.Errorf("issue = %+v, %v, req=%v", is, err, issueReq)
	}
	pr, err := p.CreatePR(ctx, ref.Repo, forge.PRSpec{Title: "Add backoff", Body: "Fixes #42", Base: "main", Head: "feat", Draft: true, Labels: []string{"enhancement"}})
	if err != nil || pr.Ref.Number != 13 || pr.WebURL == "" || !pr.Draft {
		t.Fatalf("pr = %+v, %v", pr, err)
	}
	if prReq["head"] != "feat" || prReq["base"] != "main" || prReq["draft"] != true || prReq["body"] != "Fixes #42" {
		t.Errorf("pr request = %v", prReq)
	}
	if len(labels) != 1 || labels[0] != "enhancement" {
		t.Errorf("labels = %v", labels)
	}
	if forge.ClosingLine(forge.GitHub, 42) != "Fixes #42" || forge.ClosingLine(forge.GitLab, 42) != "Closes #42" {
		t.Error("closing keywords")
	}
}
