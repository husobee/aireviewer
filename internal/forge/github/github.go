// SPDX-License-Identifier: MIT

// Package github implements forge.Provider for GitHub and GitHub Enterprise.
package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	gh "github.com/google/go-github/v93/github"

	"github.com/husobee/aireviewer/internal/forge"
)

// maxFiles mirrors the GitHub API limit for the PR files endpoint.
const maxFiles = 3000

// Provider talks to one GitHub host.
type Provider struct {
	host   string
	client *gh.Client
}

// Options configures a Provider.
type Options struct {
	Host  string // github.com or a GHES hostname
	Token string // optional; unauthenticated access works for public repos
	// BaseURL overrides the API endpoint (tests, unusual GHES setups).
	BaseURL    string
	HTTPClient *http.Client
}

// New builds a GitHub provider.
func New(o Options) (*Provider, error) {
	opts := []gh.ClientOptionsFunc{
		gh.WithUserAgent("aireviewer"),
		gh.WithTimeout(60 * time.Second),
	}
	if o.HTTPClient != nil {
		opts = append(opts, gh.WithHTTPClient(o.HTTPClient))
	}
	if o.Token != "" {
		opts = append(opts, gh.WithAuthToken(o.Token))
	}
	base := o.BaseURL
	if base == "" && o.Host != "" && o.Host != "github.com" {
		base = "https://" + o.Host + "/api/v3/"
	}
	if base != "" {
		opts = append(opts, gh.WithEnterpriseURLs(base, base))
	}
	c, err := gh.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("github client: %w", err)
	}
	return &Provider{host: o.Host, client: c}, nil
}

func (p *Provider) Kind() forge.Kind { return forge.GitHub }

func splitProject(project string) (owner, repo string, err error) {
	owner, repo, ok := strings.Cut(project, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", fmt.Errorf("github project must be owner/repo, got %q", project)
	}
	return owner, repo, nil
}

func wrap(err error) error {
	var er *gh.ErrorResponse
	if errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %v", forge.ErrNotFound, err)
	}
	return err
}

// GetPR fetches PR metadata.
func (p *Provider) GetPR(ctx context.Context, ref forge.PRRef) (*forge.PR, error) {
	owner, repo, err := splitProject(ref.Project)
	if err != nil {
		return nil, err
	}
	pr, _, err := p.client.PullRequests.Get(ctx, owner, repo, ref.Number)
	if err != nil {
		return nil, wrap(err)
	}
	state := forge.StateOpen
	switch {
	case pr.GetMerged():
		state = forge.StateMerged
	case pr.GetState() == "closed":
		state = forge.StateClosed
	}
	out := &forge.PR{
		Ref:        ref,
		Noun:       forge.GitHub.Noun(),
		Title:      pr.GetTitle(),
		Body:       pr.GetBody(),
		State:      state,
		Draft:      pr.GetDraft(),
		Author:     pr.GetUser().GetLogin(),
		BaseBranch: pr.GetBase().GetRef(),
		HeadBranch: pr.GetHead().GetRef(),
		BaseSHA:    pr.GetBase().GetSHA(),
		HeadSHA:    pr.GetHead().GetSHA(),
		WebURL:     pr.GetHTMLURL(),
		CreatedAt:  pr.GetCreatedAt().Time,
		Labels:     []string{},
		Reviewers:  []string{},
	}
	for _, l := range pr.Labels {
		out.Labels = append(out.Labels, l.GetName())
	}
	for _, u := range pr.RequestedReviewers {
		out.Reviewers = append(out.Reviewers, u.GetLogin())
	}
	return out, nil
}

// GetDiff fetches the per-file changes of a PR.
func (p *Provider) GetDiff(ctx context.Context, ref forge.PRRef) (*forge.Diff, error) {
	owner, repo, err := splitProject(ref.Project)
	if err != nil {
		return nil, err
	}
	d := &forge.Diff{Files: []forge.FileChange{}}
	opt := &gh.ListOptions{PerPage: 100}
	for {
		files, resp, err := p.client.PullRequests.ListFiles(ctx, owner, repo, ref.Number, opt)
		if err != nil {
			return nil, wrap(err)
		}
		for _, f := range files {
			d.Files = append(d.Files, convertFile(f))
		}
		if resp.NextPage == 0 || len(d.Files) >= maxFiles {
			break
		}
		opt.Page = resp.NextPage
	}
	d.ComputeStats()
	return d, nil
}

func convertFile(f *gh.CommitFile) forge.FileChange {
	fc := forge.FileChange{
		Path:      f.GetFilename(),
		Additions: f.GetAdditions(),
		Deletions: f.GetDeletions(),
		Patch:     f.GetPatch(),
	}
	switch f.GetStatus() {
	case "added":
		fc.Status = forge.FileAdded
	case "removed":
		fc.Status = forge.FileDeleted
	case "renamed", "copied":
		fc.Status = forge.FileRenamed
		fc.OldPath = f.GetPreviousFilename()
	default:
		fc.Status = forge.FileModified
	}
	fc.Truncated = fc.Patch == "" && (fc.Additions+fc.Deletions) > 0
	return fc
}
