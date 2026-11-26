// SPDX-License-Identifier: MIT

package gitlab

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/husobee/aireviewer/internal/forge"
)

// A GitLab API redirect to another host must not carry the Private-Token
// header (Go forwards non-Authorization headers across hosts).
func TestTokenNotSentAcrossRedirect(t *testing.T) {
	leaked := false
	evil := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Private-Token") != "" || r.Header.Get("Job-Token") != "" {
			leaked = true
		}
	}))
	defer evil.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+r.URL.Path, http.StatusFound)
	}))
	defer api.Close()
	p, err := New(Options{Host: "gitlab.com", Token: "secret", BaseURL: api.URL + "/api/v4"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetPR(context.Background(), forge.PRRef{Repo: forge.Repo{Forge: forge.GitLab, Host: "gitlab.com", Project: "a/b"}, Number: 1}); err == nil {
		t.Fatal("expected the cross-host redirect to fail")
	}
	if leaked {
		t.Fatal("Private-Token reached the redirect target")
	}
}
