// SPDX-License-Identifier: MIT

package gitlab

import (
	"context"
	"fmt"
	"strings"
	"time"

	gl "gitlab.com/gitlab-org/api/client-go"

	"github.com/husobee/aireviewer/internal/forge"
)

// DefaultBranch returns the project's default branch.
func (p *Provider) DefaultBranch(ctx context.Context, repo forge.Repo) (string, error) {
	pr, _, err := p.client.Projects.GetProject(repo.Project, &gl.GetProjectOptions{}, gl.WithContext(ctx))
	if err != nil {
		return "", wrap(err)
	}
	return pr.DefaultBranch, nil
}

// FindOpenPR returns the open MR whose source branch is branch in this project.
func (p *Provider) FindOpenPR(ctx context.Context, repo forge.Repo, branch string) (*forge.PR, error) {
	mrs, _, err := p.client.MergeRequests.ListProjectMergeRequests(repo.Project, &gl.ListProjectMergeRequestsOptions{
		State: ptr("opened"), SourceBranch: ptr(branch), ListOptions: gl.ListOptions{PerPage: 5}}, gl.WithContext(ctx))
	if err != nil {
		return nil, wrap(err)
	}
	for _, mr := range mrs {
		if mr.SourceBranch == branch && (mr.SourceProjectID == 0 || mr.SourceProjectID == mr.ProjectID) {
			return p.GetPR(ctx, forge.PRRef{Repo: repo, Number: int(mr.IID)})
		}
	}
	return nil, nil
}

// SearchIssues runs one query against the project's issue search: open
// issues, plus closed ones within the window.
func (p *Provider) SearchIssues(ctx context.Context, repo forge.Repo, q forge.IssueSearch) ([]forge.Issue, error) {
	limit := q.Limit
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	opts := &gl.ListProjectIssuesOptions{Search: ptr(strings.TrimSpace(q.Query)), ListOptions: gl.ListOptions{PerPage: int64(limit)}}
	if q.ClosedWithin <= 0 {
		opts.State = ptr("opened")
	}
	issues, _, err := p.client.Issues.ListProjectIssues(repo.Project, opts, gl.WithContext(ctx))
	if err != nil {
		return nil, wrap(err)
	}
	cutoff := time.Now().Add(-q.ClosedWithin)
	out := []forge.Issue{}
	for _, is := range issues {
		if is.State == "closed" && is.ClosedAt != nil && is.ClosedAt.Before(cutoff) {
			continue
		}
		out = append(out, convertIssue(is, repo.Project, "search"))
	}
	return out, nil
}

// GetIssue fetches one issue of the project.
func (p *Provider) GetIssue(ctx context.Context, repo forge.Repo, number int) (*forge.Issue, error) {
	is, _, err := p.client.Issues.GetIssue(repo.Project, int64(number), gl.WithContext(ctx))
	if err != nil {
		return nil, wrap(err)
	}
	out := convertIssue(is, repo.Project, "lookup")
	return &out, nil
}

// CreateIssue opens a new issue.
func (p *Provider) CreateIssue(ctx context.Context, repo forge.Repo, ni forge.NewIssue) (*forge.Issue, error) {
	opts := &gl.CreateIssueOptions{Title: ptr(ni.Title), Description: ptr(ni.Body)}
	if len(ni.Labels) > 0 {
		opts.Labels = ptr(gl.LabelOptions(ni.Labels))
	}
	is, _, err := p.client.Issues.CreateIssue(repo.Project, opts, gl.WithContext(ctx))
	if err != nil {
		return nil, wrap(err)
	}
	out := convertIssue(is, repo.Project, "created")
	return &out, nil
}

// draftTitle marks an MR as draft the way every GitLab version understands.
func draftTitle(title string, draft bool) string {
	if !draft {
		return title
	}
	l := strings.ToLower(title)
	for _, p := range []string{"draft:", "[draft]", "(draft)"} {
		if strings.HasPrefix(l, p) {
			return title
		}
	}
	return "Draft: " + title
}

// CreatePR opens a merge request from spec.Head into spec.Base.
func (p *Provider) CreatePR(ctx context.Context, repo forge.Repo, spec forge.PRSpec) (*forge.PR, error) {
	opts := &gl.CreateMergeRequestOptions{Title: ptr(draftTitle(spec.Title, spec.Draft)), Description: ptr(spec.Body),
		SourceBranch: ptr(spec.Head), TargetBranch: ptr(spec.Base)}
	if len(spec.Labels) > 0 {
		opts.Labels = ptr(gl.LabelOptions(spec.Labels))
	}
	mr, _, err := p.client.MergeRequests.CreateMergeRequest(repo.Project, opts, gl.WithContext(ctx))
	if err != nil {
		return nil, wrap(err)
	}
	pr, err := p.GetPR(ctx, forge.PRRef{Repo: repo, Number: int(mr.IID)})
	if err != nil {
		return &forge.PR{Ref: forge.PRRef{Repo: repo, Number: int(mr.IID)}, Noun: forge.GitLab.Noun(), Title: mr.Title, WebURL: mr.WebURL},
			fmt.Errorf("merge request created (%s) but re-reading it failed: %w", mr.WebURL, err)
	}
	return pr, nil
}
