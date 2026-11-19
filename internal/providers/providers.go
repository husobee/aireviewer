// SPDX-License-Identifier: MIT

// Package providers constructs forge.Provider instances and resolves
// credentials. It is the only package that knows about every adapter.
package providers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/forge/github"
	"github.com/husobee/aireviewer/internal/forge/gitlab"
)

// TokenSource resolves a credential for a forge host. Returning "" means
// "unauthenticated", which works for public projects.
type TokenSource func(ctx context.Context, kind forge.Kind, host string) string

// EnvTokens resolves tokens from the environment, falling back to the gh CLI
// for GitHub hosts.
//
//	GitHub: GITHUB_TOKEN, GH_TOKEN, then `gh auth token --hostname <host>`
//	GitLab: GITLAB_TOKEN (CI_JOB_TOKEN is handled separately by the adapter)
func EnvTokens(ctx context.Context, kind forge.Kind, host string) string {
	switch kind {
	case forge.GitHub:
		for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
			if v := os.Getenv(k); v != "" {
				return v
			}
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", host).Output() //nolint:gosec // fixed binary, host is a parsed hostname passed as one argv element
		if err == nil {
			return strings.TrimSpace(string(out))
		}
	case forge.GitLab:
		return os.Getenv("GITLAB_TOKEN")
	}
	return ""
}

// Registry caches one provider per (kind, host).
type Registry struct {
	Tokens TokenSource
	mu     sync.Mutex
	cache  map[string]forge.Provider
}

// NewRegistry returns a Registry using EnvTokens.
func NewRegistry() *Registry {
	return &Registry{Tokens: EnvTokens, cache: map[string]forge.Provider{}}
}

// Get returns a provider for the repo's forge and host.
func (r *Registry) Get(ctx context.Context, repo forge.Repo) (forge.Provider, error) {
	key := string(repo.Forge) + "|" + repo.Host
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.cache[key]; ok {
		return p, nil
	}
	tok := r.Tokens(ctx, repo.Forge, repo.Host)
	var (
		p   forge.Provider
		err error
	)
	switch repo.Forge {
	case forge.GitHub:
		p, err = github.New(github.Options{Host: repo.Host, Token: tok})
	case forge.GitLab:
		p, err = gitlab.New(gitlab.Options{Host: repo.Host, Token: tok, JobToken: os.Getenv("CI_JOB_TOKEN")})
	default:
		err = fmt.Errorf("unsupported forge %q", repo.Forge)
	}
	if err != nil {
		return nil, err
	}
	r.cache[key] = p
	return p, nil
}
