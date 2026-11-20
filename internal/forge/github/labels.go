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

// LabelledFixCommits returns the commits of PRs merged in [since, until]
// that carry any of labels (search `label:a,b` is an OR).
func (p *Provider) LabelledFixCommits(ctx context.Context, repo forge.Repo, labels []string, since, until time.Time, limit int) (map[string]bool, int, error) {
	owner, name, err := splitProject(repo.Project)
	if err != nil {
		return nil, 0, err
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	quoted := make([]string, len(labels))
	for i, l := range labels {
		quoted[i] = `"` + strings.ReplaceAll(l, `"`, "") + `"`
	}
	q := fmt.Sprintf("repo:%s is:pr is:merged label:%s merged:%s..%s", repo.Project, strings.Join(quoted, ","),
		since.UTC().Format("2006-01-02"), until.UTC().Format("2006-01-02"))
	out := map[string]bool{}
	prs := 0
	opts := &gh.SearchOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for prs < limit {
		res, resp, err := p.client.Search.Issues(ctx, q, opts)
		if err != nil {
			return nil, prs, wrap(err)
		}
		for _, is := range res.Issues {
			if prs >= limit {
				break
			}
			prs++
			pr, _, err := p.client.PullRequests.Get(ctx, owner, name, is.GetNumber())
			if err != nil {
				return nil, prs, wrap(err)
			}
			if mc := pr.GetMergeCommitSHA(); mc != "" {
				out[mc] = true
			}
			commits, _, err := p.client.PullRequests.ListCommits(ctx, owner, name, is.GetNumber(), &gh.ListOptions{PerPage: 100})
			if err != nil {
				return nil, prs, wrap(err)
			}
			for _, c := range commits {
				out[c.GetSHA()] = true
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, prs, nil
}
