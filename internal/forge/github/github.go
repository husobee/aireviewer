// SPDX-License-Identifier: MIT

// Package github implements forge.Provider for GitHub and GitHub Enterprise.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	base   string
	token  string
	http   *http.Client
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
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	return &Provider{host: o.Host, base: base, token: o.Token, http: hc, client: c}, nil
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
	return toPR(ref, pr), nil
}

func toPR(ref forge.PRRef, pr *gh.PullRequest) *forge.PR {
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
	return out
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

// ---- M2: linked issues, author mapping, workload ----

func (p *Provider) graphqlURL() string {
	if p.base != "" {
		return strings.TrimSuffix(strings.TrimSuffix(p.base, "/"), "/v3") + "/graphql"
	}
	return "https://api.github.com/graphql"
}

const closingQuery = `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){
closingIssuesReferences(first:25){nodes{number title body state url repository{nameWithOwner} labels(first:20){nodes{name}}}}}}}`

type gqlResp struct {
	Data struct {
		Repository struct {
			PullRequest struct {
				ClosingIssuesReferences struct {
					Nodes []struct {
						Number     int    `json:"number"`
						Title      string `json:"title"`
						Body       string `json:"body"`
						State      string `json:"state"`
						URL        string `json:"url"`
						Repository struct {
							NameWithOwner string `json:"nameWithOwner"`
						} `json:"repository"`
						Labels struct {
							Nodes []struct {
								Name string `json:"name"`
							} `json:"nodes"`
						} `json:"labels"`
					} `json:"nodes"`
				} `json:"closingIssuesReferences"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// LinkedIssues returns the PR's closing issue references (GraphQL, needs a
// token) plus issues named with closing keywords in the PR body.
func (p *Provider) LinkedIssues(ctx context.Context, pr *forge.PR) ([]forge.Issue, error) {
	owner, repo, err := splitProject(pr.Ref.Project)
	if err != nil {
		return nil, err
	}
	out := []forge.Issue{}
	seen := map[string]bool{}
	if p.token != "" {
		body, _ := json.Marshal(map[string]any{"query": closingQuery,
			"variables": map[string]any{"owner": owner, "name": repo, "number": pr.Ref.Number}})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.graphqlURL(), bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+p.token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := p.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("github graphql: %w", err)
		}
		defer func() { _ = resp.Body.Close() }()
		var g gqlResp
		if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&g); err != nil {
			return nil, fmt.Errorf("github graphql: %w", err)
		}
		if len(g.Errors) > 0 {
			return nil, fmt.Errorf("github graphql: %s", g.Errors[0].Message)
		}
		for _, n := range g.Data.Repository.PullRequest.ClosingIssuesReferences.Nodes {
			ref := forge.IssueRefString(n.Repository.NameWithOwner, n.Number)
			is := forge.Issue{Ref: ref, Number: n.Number, Title: n.Title, Body: n.Body, State: strings.ToLower(n.State),
				URL: n.URL, Labels: []string{}, Source: "closing_reference"}
			for _, l := range n.Labels.Nodes {
				is.Labels = append(is.Labels, l.Name)
			}
			seen[strings.ToLower(ref)] = true
			out = append(out, is)
		}
	}
	for _, r := range forge.ParseClosingRefs(pr.Body) {
		proj := r.Project
		if proj == "" {
			proj = pr.Ref.Project
		}
		ref := forge.IssueRefString(proj, r.Number)
		if seen[strings.ToLower(ref)] {
			continue
		}
		o, n, err := splitProject(proj)
		if err != nil {
			continue
		}
		is, _, err := p.client.Issues.Get(ctx, o, n, r.Number)
		if err != nil || is.IsPullRequest() {
			continue // unknown, inaccessible, or a PR rather than an issue
		}
		seen[strings.ToLower(ref)] = true
		fi := forge.Issue{Ref: ref, Number: r.Number, Title: is.GetTitle(), Body: is.GetBody(), State: is.GetState(),
			URL: is.GetHTMLURL(), Labels: []string{}, Source: "body_keyword"}
		for _, l := range is.Labels {
			fi.Labels = append(fi.Labels, l.GetName())
		}
		out = append(out, fi)
	}
	return out, nil
}

// CommitAuthorLogins maps commit author emails to GitHub logins for commits
// touching path since the given time.
func (p *Provider) CommitAuthorLogins(ctx context.Context, repo forge.Repo, path string, since time.Time) (map[string]string, error) {
	owner, name, err := splitProject(repo.Project)
	if err != nil {
		return nil, err
	}
	commits, _, err := p.client.Repositories.ListCommits(ctx, owner, name, &gh.CommitsListOptions{
		Path: path, Since: since, ListOptions: gh.ListOptions{PerPage: 100}})
	if err != nil {
		return nil, wrap(err)
	}
	out := map[string]string{}
	for _, c := range commits {
		email := strings.ToLower(c.GetCommit().GetAuthor().GetEmail())
		if login := c.GetAuthor().GetLogin(); email != "" && login != "" {
			out[email] = login
		}
	}
	return out, nil
}

// OpenReviewRequests counts open PRs on which the user is a requested reviewer.
func (p *Provider) OpenReviewRequests(ctx context.Context, _ forge.Repo, user string) (int, error) {
	res, _, err := p.client.Search.Issues(ctx, "is:open is:pr review-requested:"+user, &gh.SearchOptions{ListOptions: gh.ListOptions{PerPage: 1}})
	if err != nil {
		return 0, wrap(err)
	}
	return res.GetTotal(), nil
}
