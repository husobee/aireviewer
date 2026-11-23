// SPDX-License-Identifier: MIT

package github

import (
	"context"
	"strings"

	gh "github.com/google/go-github/v93/github"

	"github.com/husobee/aireviewer/internal/forge"
)

// ListOpenPRsByBase returns open PRs in the repository that target base.
func (p *Provider) ListOpenPRsByBase(ctx context.Context, repo forge.Repo, base string) ([]*forge.PR, error) {
	owner, name, err := splitProject(repo.Project)
	if err != nil {
		return nil, err
	}
	prs, _, err := p.client.PullRequests.List(ctx, owner, name, &gh.PullRequestListOptions{
		State: "open", Base: base, ListOptions: gh.ListOptions{PerPage: 50}})
	if err != nil {
		return nil, wrap(err)
	}
	var out []*forge.PR
	for _, pr := range prs {
		if strings.EqualFold(pr.GetHead().GetRepo().GetFullName(), repo.Project) || pr.GetHead().GetRepo() == nil {
			out = append(out, toPR(forge.PRRef{Repo: repo, Number: pr.GetNumber()}, pr))
		}
	}
	return out, nil
}
