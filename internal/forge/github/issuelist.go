// SPDX-License-Identifier: MIT

package github

import (
	"context"
	"time"

	gh "github.com/google/go-github/v93/github"

	"github.com/husobee/aireviewer/internal/forge"
)

// ListIssuesUpdatedSince pages through the repository's issues (open and
// closed, not PRs) updated at or after since, oldest update first, for the
// local issue index (M6d).
func (p *Provider) ListIssuesUpdatedSince(ctx context.Context, repo forge.Repo, since time.Time, page int) ([]forge.Issue, []time.Time, []*time.Time, bool, error) {
	owner, name, err := splitProject(repo.Project)
	if err != nil {
		return nil, nil, nil, false, err
	}
	issues, resp, err := p.client.Issues.ListByRepo(ctx, owner, name, &gh.IssueListByRepoOptions{
		State: "all", Sort: "updated", Direction: "asc", Since: since, ListOptions: gh.ListOptions{PerPage: 100, Page: page}})
	if err != nil {
		return nil, nil, nil, false, wrap(err)
	}
	var out []forge.Issue
	var updated []time.Time
	var closed []*time.Time
	for _, is := range issues {
		if is.IsPullRequest() {
			continue
		}
		out = append(out, toIssue(repo.Project, is, "index"))
		updated = append(updated, is.GetUpdatedAt().Time)
		if is.ClosedAt != nil {
			t := is.GetClosedAt().Time
			closed = append(closed, &t)
		} else {
			closed = append(closed, nil)
		}
	}
	return out, updated, closed, resp != nil && resp.NextPage != 0, nil
}
