// SPDX-License-Identifier: MIT

package gitlab

import (
	"context"
	"time"

	gl "gitlab.com/gitlab-org/api/client-go"

	"github.com/husobee/aireviewer/internal/forge"
)

// ListIssuesUpdatedSince pages through the project's issues (open and
// closed) updated after since, oldest update first, for the local issue
// index (M6d).
func (p *Provider) ListIssuesUpdatedSince(ctx context.Context, repo forge.Repo, since time.Time, page int) ([]forge.Issue, []time.Time, []*time.Time, bool, error) {
	opts := &gl.ListProjectIssuesOptions{OrderBy: ptr("updated_at"), Sort: ptr("asc"), ListOptions: gl.ListOptions{PerPage: 100, Page: int64(page)}}
	if !since.IsZero() {
		opts.UpdatedAfter = ptr(since)
	}
	issues, resp, err := p.client.Issues.ListProjectIssues(repo.Project, opts, gl.WithContext(ctx))
	if err != nil {
		return nil, nil, nil, false, wrap(err)
	}
	var out []forge.Issue
	var updated []time.Time
	var closed []*time.Time
	for _, is := range issues {
		out = append(out, convertIssue(is, repo.Project, "index"))
		u := time.Time{}
		if is.UpdatedAt != nil {
			u = *is.UpdatedAt
		}
		updated = append(updated, u)
		closed = append(closed, is.ClosedAt)
	}
	return out, updated, closed, resp != nil && resp.NextPage != 0, nil
}
