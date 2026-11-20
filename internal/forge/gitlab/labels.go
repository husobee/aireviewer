// SPDX-License-Identifier: MIT

package gitlab

import (
	"context"
	"time"

	gl "gitlab.com/gitlab-org/api/client-go"

	"github.com/husobee/aireviewer/internal/forge"
)

// LabelledFixCommits returns the commits of MRs merged in [since, until]
// that carry any of labels. GitLab's labels filter is an AND, so each label
// is queried separately and the results are combined.
func (p *Provider) LabelledFixCommits(ctx context.Context, repo forge.Repo, labels []string, since, until time.Time, limit int) (map[string]bool, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	out := map[string]bool{}
	seen := map[int64]bool{}
	for _, label := range labels {
		opts := &gl.ListProjectMergeRequestsOptions{State: ptr("merged"), Labels: ptr(gl.LabelOptions{label}),
			UpdatedAfter: ptr(since), ListOptions: gl.ListOptions{PerPage: 100}}
		for len(seen) < limit {
			mrs, resp, err := p.client.MergeRequests.ListProjectMergeRequests(repo.Project, opts, gl.WithContext(ctx))
			if err != nil {
				return nil, len(seen), wrap(err)
			}
			for _, mr := range mrs {
				if seen[mr.IID] || len(seen) >= limit {
					continue
				}
				if mr.MergedAt == nil || mr.MergedAt.Before(since) || mr.MergedAt.After(until) {
					continue
				}
				seen[mr.IID] = true
				for _, sha := range []string{mr.MergeCommitSHA, mr.SquashCommitSHA} {
					if sha != "" {
						out[sha] = true
					}
				}
				commits, _, err := p.client.MergeRequests.GetMergeRequestCommits(repo.Project, mr.IID, &gl.GetMergeRequestCommitsOptions{ListOptions: gl.ListOptions{PerPage: 100}}, gl.WithContext(ctx))
				if err != nil {
					return nil, len(seen), wrap(err)
				}
				for _, c := range commits {
					out[c.ID] = true
				}
			}
			if resp == nil || resp.NextPage == 0 {
				break
			}
			opts.Page = resp.NextPage
		}
	}
	return out, len(seen), nil
}
