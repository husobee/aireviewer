// SPDX-License-Identifier: MIT

package gitlab

import (
	"context"

	gl "gitlab.com/gitlab-org/api/client-go"

	"github.com/husobee/aireviewer/internal/forge"
)

// ListOpenPRsByBase returns open MRs in the project that target base.
func (p *Provider) ListOpenPRsByBase(ctx context.Context, repo forge.Repo, base string) ([]*forge.PR, error) {
	mrs, _, err := p.client.MergeRequests.ListProjectMergeRequests(repo.Project, &gl.ListProjectMergeRequestsOptions{
		State: ptr("opened"), TargetBranch: ptr(base), ListOptions: gl.ListOptions{PerPage: 50}}, gl.WithContext(ctx))
	if err != nil {
		return nil, wrap(err)
	}
	var out []*forge.PR
	for _, mr := range mrs {
		if mr.SourceProjectID != 0 && mr.SourceProjectID != mr.ProjectID {
			continue // forks are not part of a same-project stack
		}
		pr := &forge.PR{Ref: forge.PRRef{Repo: repo, Number: int(mr.IID)}, Noun: forge.GitLab.Noun(), Title: mr.Title, State: forge.StateOpen,
			BaseBranch: mr.TargetBranch, HeadBranch: mr.SourceBranch, WebURL: mr.WebURL, Labels: []string{}, Reviewers: []string{}}
		if mr.Author != nil {
			pr.Author = mr.Author.Username
		}
		out = append(out, pr)
	}
	return out, nil
}
