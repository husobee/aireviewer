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
//	GitLab: GITLAB_TOKEN (CI_JOB_TOKEN is handled by Registry, for CI_SERVER_HOST only)
//
// It does not check the host: callers go through Registry.Token, which does.
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

// Registry caches one provider per (kind, host). It is the only place forge
// credentials are attached, and it attaches them only to hosts the HostConfig
// declares for that forge (github.com, gitlab.com, AIREVIEWER_*_HOSTS).
type Registry struct {
	Hosts  forge.HostConfig
	Tokens TokenSource
	mu     sync.Mutex
	cache  map[string]forge.Provider
}

// NewRegistry returns a Registry using EnvTokens, bound to hosts.
func NewRegistry(hosts forge.HostConfig) *Registry {
	return &Registry{Hosts: hosts, Tokens: EnvTokens, cache: map[string]forge.Provider{}}
}

// Token returns the credential for host, or "" when host is not declared for
// kind. Use it (not Tokens) wherever a credential leaves the process.
func (r *Registry) Token(ctx context.Context, kind forge.Kind, host string) string {
	if !r.Hosts.Trusted(kind, host) || r.Tokens == nil {
		return ""
	}
	return r.Tokens(ctx, kind, host)
}

// jobToken returns CI_JOB_TOKEN only for the GitLab instance running the job
// (CI_SERVER_HOST): a job token is scoped to that instance and is never sent
// anywhere else.
func jobToken(host string) string {
	tok, ci := os.Getenv("CI_JOB_TOKEN"), strings.ToLower(os.Getenv("CI_SERVER_HOST"))
	if tok == "" || ci == "" {
		return ""
	}
	h := strings.ToLower(host)
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	if h != ci {
		return ""
	}
	return tok
}

// Get returns a provider for the repo's forge and host.
func (r *Registry) Get(ctx context.Context, repo forge.Repo) (forge.Provider, error) {
	if _, err := r.Hosts.KindForHost(repo.Host); err != nil {
		return nil, err
	}
	if !r.Hosts.Trusted(repo.Forge, repo.Host) {
		return nil, fmt.Errorf("host %q is not configured as %s", repo.Host, repo.Forge)
	}
	key := string(repo.Forge) + "|" + repo.Host
	r.mu.Lock()
	defer r.mu.Unlock()
	if p, ok := r.cache[key]; ok {
		return p, nil
	}
	tok := r.Token(ctx, repo.Forge, repo.Host)
	var (
		p   forge.Provider
		err error
	)
	switch repo.Forge {
	case forge.GitHub:
		p, err = github.New(github.Options{Host: repo.Host, Token: tok})
	case forge.GitLab:
		p, err = gitlab.New(gitlab.Options{Host: repo.Host, Token: tok, JobToken: jobToken(repo.Host)})
	default:
		err = fmt.Errorf("unsupported forge %q", repo.Forge)
	}
	if err != nil {
		return nil, err
	}
	r.cache[key] = p
	return p, nil
}
