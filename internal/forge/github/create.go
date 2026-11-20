// SPDX-License-Identifier: MIT

package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v93/github"

	"github.com/husobee/aireviewer/internal/forge"
)

// DefaultBranch returns the repository's default branch.
func (p *Provider) DefaultBranch(ctx context.Context, repo forge.Repo) (string, error) {
	owner, name, err := splitProject(repo.Project)
	if err != nil {
		return "", err
	}
	r, _, err := p.client.Repositories.Get(ctx, owner, name)
	if err != nil {
		return "", wrap(err)
	}
	return r.GetDefaultBranch(), nil
}

// FindOpenPR returns the open PR whose head is branch in the same repository.
func (p *Provider) FindOpenPR(ctx context.Context, repo forge.Repo, branch string) (*forge.PR, error) {
	owner, name, err := splitProject(repo.Project)
	if err != nil {
		return nil, err
	}
	prs, _, err := p.client.PullRequests.List(ctx, owner, name, &gh.PullRequestListOptions{
		State: "open", Head: owner + ":" + branch, ListOptions: gh.ListOptions{PerPage: 5}})
	if err != nil {
		return nil, wrap(err)
	}
	for _, pr := range prs {
		if pr.GetHead().GetRef() == branch && strings.EqualFold(pr.GetHead().GetRepo().GetFullName(), repo.Project) {
			return toPR(forge.PRRef{Repo: repo, Number: pr.GetNumber()}, pr), nil
		}
	}
	return nil, nil
}

func toIssue(project string, is *gh.Issue, source string) forge.Issue {
	out := forge.Issue{Ref: forge.IssueRefString(project, is.GetNumber()), Number: is.GetNumber(), Title: is.GetTitle(),
		Body: is.GetBody(), State: is.GetState(), URL: is.GetHTMLURL(), Labels: []string{}, Source: source}
	for _, l := range is.Labels {
		out.Labels = append(out.Labels, l.GetName())
	}
	return out
}

// SearchIssues runs one query against the search API, restricted to issues
// (not PRs) in the repository: open ones, plus closed ones within the window.
func (p *Provider) SearchIssues(ctx context.Context, repo forge.Repo, q forge.IssueSearch) ([]forge.Issue, error) {
	if _, _, err := splitProject(repo.Project); err != nil {
		return nil, err
	}
	limit := q.Limit
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	query := "repo:" + repo.Project + " is:issue " + strings.TrimSpace(q.Query)
	if q.ClosedWithin <= 0 {
		query += " is:open"
	}
	res, _, err := p.client.Search.Issues(ctx, query, &gh.SearchOptions{ListOptions: gh.ListOptions{PerPage: limit}})
	if err != nil {
		return nil, wrap(err)
	}
	cutoff := time.Now().Add(-q.ClosedWithin)
	out := []forge.Issue{}
	for _, is := range res.Issues {
		if is.IsPullRequest() {
			continue
		}
		if is.GetState() == "closed" && is.GetClosedAt().Before(cutoff) {
			continue
		}
		out = append(out, toIssue(repo.Project, is, "search"))
	}
	return out, nil
}

// GetIssue fetches one issue of the repository. PRs are rejected.
func (p *Provider) GetIssue(ctx context.Context, repo forge.Repo, number int) (*forge.Issue, error) {
	owner, name, err := splitProject(repo.Project)
	if err != nil {
		return nil, err
	}
	is, _, err := p.client.Issues.Get(ctx, owner, name, number)
	if err != nil {
		return nil, wrap(err)
	}
	if is.IsPullRequest() {
		return nil, fmt.Errorf("#%d is a pull request, not an issue", number)
	}
	out := toIssue(repo.Project, is, "lookup")
	return &out, nil
}

// CreateIssue opens a new issue.
func (p *Provider) CreateIssue(ctx context.Context, repo forge.Repo, ni forge.NewIssue) (*forge.Issue, error) {
	owner, name, err := splitProject(repo.Project)
	if err != nil {
		return nil, err
	}
	is, _, err := p.client.Issues.Create(ctx, owner, name, gh.CreateIssueRequest{Title: ni.Title, Body: new(ni.Body), Labels: ni.Labels})
	if err != nil {
		return nil, wrap(err)
	}
	out := toIssue(repo.Project, is, "created")
	return &out, nil
}

// CreatePR opens a pull request from spec.Head into spec.Base. Labels are
// added afterwards (the create endpoint does not take them); a labelling
// failure does not undo the PR.
func (p *Provider) CreatePR(ctx context.Context, repo forge.Repo, spec forge.PRSpec) (*forge.PR, error) {
	owner, name, err := splitProject(repo.Project)
	if err != nil {
		return nil, err
	}
	pr, _, err := p.client.PullRequests.Create(ctx, owner, name, gh.CreatePullRequest{
		Title: new(spec.Title), Head: spec.Head, Base: spec.Base, Body: new(spec.Body), Draft: new(spec.Draft)})
	if err != nil {
		return nil, wrap(err)
	}
	out := toPR(forge.PRRef{Repo: repo, Number: pr.GetNumber()}, pr)
	if len(spec.Labels) > 0 {
		if _, _, err := p.client.Issues.AddLabelsToIssue(ctx, owner, name, pr.GetNumber(), spec.Labels); err != nil {
			return out, fmt.Errorf("pull request created (%s) but adding labels failed: %w", out.WebURL, err)
		}
		out.Labels = append(out.Labels, spec.Labels...)
	}
	return out, nil
}
