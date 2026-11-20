// SPDX-License-Identifier: MIT

package github

import (
	"context"
	"sort"
	"strings"

	gh "github.com/google/go-github/v93/github"

	"github.com/husobee/aireviewer/internal/forge"
)

const activityPages = 10 // 100 per page

// Activity returns the PR's reviews, conversation and inline comments, and
// commits, oldest first.
func (p *Provider) Activity(ctx context.Context, pr *forge.PR) (*forge.Activity, error) {
	owner, name, err := splitProject(pr.Ref.Project)
	if err != nil {
		return nil, err
	}
	n := pr.Ref.Number
	raw, _, err := p.client.PullRequests.Get(ctx, owner, name, n)
	if err != nil {
		return nil, wrap(err)
	}
	a := &forge.Activity{CreatedAt: raw.GetCreatedAt().Time, Author: raw.GetUser().GetLogin()}
	if raw.MergedAt != nil {
		t := raw.GetMergedAt().Time
		a.MergedAt = &t
	}
	if raw.ClosedAt != nil {
		t := raw.GetClosedAt().Time
		a.ClosedAt = &t
	}
	add := func(e forge.ActivityEvent) {
		forge.ClassifyBody(&e)
		a.Events = append(a.Events, e)
	}
	lo := gh.ListOptions{PerPage: 100}
	for page := 1; page <= activityPages; page++ {
		lo.Page = page
		reviews, resp, err := p.client.PullRequests.ListReviews(ctx, owner, name, n, &lo)
		if err != nil {
			return nil, wrap(err)
		}
		for _, r := range reviews {
			add(forge.ActivityEvent{Kind: forge.EventReview, Author: r.GetUser().GetLogin(), At: r.GetSubmittedAt().Time,
				State: strings.ToLower(r.GetState()), Body: r.GetBody(), URL: r.GetHTMLURL()})
		}
		if resp.NextPage == 0 {
			break
		}
		a.Truncated = a.Truncated || page == activityPages
	}
	for page := 1; page <= activityPages; page++ {
		cs, resp, err := p.client.PullRequests.ListComments(ctx, owner, name, n, &gh.PullRequestListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100, Page: page}})
		if err != nil {
			return nil, wrap(err)
		}
		for _, c := range cs {
			add(forge.ActivityEvent{Kind: forge.EventInline, Author: c.GetUser().GetLogin(), At: c.GetCreatedAt().Time,
				Body: c.GetBody(), Path: c.GetPath(), Line: c.GetLine(), URL: c.GetHTMLURL()})
		}
		if resp.NextPage == 0 {
			break
		}
		a.Truncated = a.Truncated || page == activityPages
	}
	for page := 1; page <= activityPages; page++ {
		cs, resp, err := p.client.Issues.ListComments(ctx, owner, name, n, &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100, Page: page}})
		if err != nil {
			return nil, wrap(err)
		}
		for _, c := range cs {
			add(forge.ActivityEvent{Kind: forge.EventComment, Author: c.GetUser().GetLogin(), At: c.GetCreatedAt().Time,
				Body: c.GetBody(), URL: c.GetHTMLURL()})
		}
		if resp.NextPage == 0 {
			break
		}
		a.Truncated = a.Truncated || page == activityPages
	}
	for page := 1; page <= 3; page++ {
		lo.Page = page
		commits, resp, err := p.client.PullRequests.ListCommits(ctx, owner, name, n, &lo)
		if err != nil {
			return nil, wrap(err)
		}
		for _, c := range commits {
			a.Events = append(a.Events, forge.ActivityEvent{Kind: forge.EventCommit, Author: c.GetAuthor().GetLogin(),
				At: c.GetCommit().GetCommitter().GetDate().Time, SHA: c.GetSHA(), Body: firstLine(c.GetCommit().GetMessage())})
		}
		if resp.NextPage == 0 {
			break
		}
	}
	sort.SliceStable(a.Events, func(i, j int) bool { return a.Events[i].At.Before(a.Events[j].At) })
	return a, nil
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}
