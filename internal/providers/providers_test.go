// SPDX-License-Identifier: MIT

package providers

import (
	"context"
	"testing"

	"github.com/husobee/aireviewer/internal/forge"
)

func TestTokensOnlyForDeclaredHosts(t *testing.T) {
	r := NewRegistry(forge.HostConfig{GitLabHosts: []string{"code.example.com"}})
	r.Tokens = func(context.Context, forge.Kind, string) string { return "secret" }
	ctx := context.Background()
	for _, tt := range []struct {
		kind forge.Kind
		host string
		want string
	}{
		{forge.GitHub, "github.com", "secret"},
		{forge.GitLab, "gitlab.com", "secret"},
		{forge.GitLab, "code.example.com", "secret"},
		{forge.GitHub, "evil.example", ""},
		{forge.GitLab, "gitlab.evil.example", ""},
		{forge.GitHub, "github.evil.example", ""},
		{forge.GitHub, "gitlab.com", ""},
		{forge.GitHub, "code.example.com", ""},
	} {
		if got := r.Token(ctx, tt.kind, tt.host); got != tt.want {
			t.Errorf("Token(%s, %q) = %q, want %q", tt.kind, tt.host, got, tt.want)
		}
	}
	if _, err := r.Get(ctx, forge.Repo{Forge: forge.GitLab, Host: "evil.example", Project: "a/b"}); err == nil {
		t.Error("Get built a provider for an undeclared host")
	}
	if _, err := r.Get(ctx, forge.Repo{Forge: forge.GitHub, Host: "gitlab.com", Project: "a/b"}); err == nil {
		t.Error("Get built a GitHub provider for a GitLab host")
	}
}

func TestJobTokenOnlyForCIServer(t *testing.T) {
	t.Setenv("CI_JOB_TOKEN", "job")
	t.Setenv("CI_SERVER_HOST", "code.example.com")
	if jobToken("code.example.com") != "job" || jobToken("CODE.example.com:443") != "job" {
		t.Error("job token withheld from the CI server")
	}
	if jobToken("gitlab.com") != "" || jobToken("evil.example") != "" {
		t.Error("job token sent to a host other than CI_SERVER_HOST")
	}
	t.Setenv("CI_SERVER_HOST", "")
	if jobToken("code.example.com") != "" {
		t.Error("job token sent without CI_SERVER_HOST")
	}
}
