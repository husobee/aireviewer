// SPDX-License-Identifier: MIT

package github

import (
	"context"
	"fmt"
	"strings"

	gh "github.com/google/go-github/v93/github"

	"github.com/husobee/aireviewer/internal/forge"
)

// M3: gated writes. Called only by the MCP server after a frozen proposal has
// been confirmed by a human.

// CurrentUser returns the authenticated login.
func (p *Provider) CurrentUser(ctx context.Context, _ forge.Repo) (string, error) {
	u, _, err := p.client.Users.Get(ctx, "")
	if err != nil {
		return "", wrap(err)
	}
	return u.GetLogin(), nil
}

var ghEvent = map[forge.Verdict]string{
	forge.VerdictApprove:        "APPROVE",
	forge.VerdictRequestChanges: "REQUEST_CHANGES",
	forge.VerdictComment:        "COMMENT",
}

// SubmitReview posts one batched review with inline comments.
func (p *Provider) SubmitReview(ctx context.Context, pr *forge.PR, r forge.Review) (forge.PostResult, error) {
	owner, repo, err := splitProject(pr.Ref.Project)
	if err != nil {
		return forge.PostResult{}, err
	}
	ev, ok := ghEvent[r.Verdict]
	if !ok {
		return forge.PostResult{}, fmt.Errorf("unknown verdict %q", r.Verdict)
	}
	req := &gh.PullRequestReviewRequest{CommitID: new(r.HeadSHA), Body: new(r.Body), Event: new(ev)}
	for _, c := range r.Comments {
		side, path := "RIGHT", c.Path
		if c.Side == "old" {
			side = "LEFT"
			if c.OldPath != "" {
				path = c.OldPath
			}
		}
		dc := &gh.DraftReviewComment{Path: new(path), Body: new(c.Body), Line: new(c.Line), Side: new(side)}
		if c.StartLine > 0 && c.StartLine < c.Line {
			dc.StartLine, dc.StartSide = new(c.StartLine), new(side)
		}
		req.Comments = append(req.Comments, dc)
	}
	rv, _, err := p.client.PullRequests.CreateReview(ctx, owner, repo, pr.Ref.Number, req)
	if err != nil {
		return forge.PostResult{}, wrap(err)
	}
	return forge.PostResult{URL: rv.GetHTMLURL()}, nil
}

// FindMarkerComment returns the newest top-level comment containing marker.
func (p *Provider) FindMarkerComment(ctx context.Context, pr *forge.PR, marker string) (*forge.MarkerComment, error) {
	owner, repo, err := splitProject(pr.Ref.Project)
	if err != nil {
		return nil, err
	}
	var found *forge.MarkerComment
	opt := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		cs, resp, err := p.client.Issues.ListComments(ctx, owner, repo, pr.Ref.Number, opt)
		if err != nil {
			return nil, wrap(err)
		}
		for _, c := range cs {
			if strings.Contains(c.GetBody(), marker) {
				found = &forge.MarkerComment{ID: c.GetID(), Author: c.GetUser().GetLogin(), Body: c.GetBody(), URL: c.GetHTMLURL()}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return found, nil
}

// UpsertMarkerComment edits the marked comment if it exists, else creates it.
func (p *Provider) UpsertMarkerComment(ctx context.Context, pr *forge.PR, marker, body string) (forge.PostResult, error) {
	owner, repo, err := splitProject(pr.Ref.Project)
	if err != nil {
		return forge.PostResult{}, err
	}
	existing, err := p.FindMarkerComment(ctx, pr, marker)
	if err != nil {
		return forge.PostResult{}, err
	}
	if existing != nil {
		c, _, err := p.client.Issues.UpdateComment(ctx, owner, repo, existing.ID, gh.IssueCommentRequest{Body: body})
		if err != nil {
			return forge.PostResult{}, wrap(err)
		}
		return forge.PostResult{URL: c.GetHTMLURL(), Notes: []string{"updated existing comment"}}, nil
	}
	c, _, err := p.client.Issues.CreateComment(ctx, owner, repo, pr.Ref.Number, gh.IssueCommentRequest{Body: body})
	if err != nil {
		return forge.PostResult{}, wrap(err)
	}
	return forge.PostResult{URL: c.GetHTMLURL(), Notes: []string{"created comment"}}, nil
}

// RequestReviewers requests users (login) and teams (@org/team) as reviewers.
func (p *Provider) RequestReviewers(ctx context.Context, pr *forge.PR, users []string) (forge.PostResult, error) {
	owner, repo, err := splitProject(pr.Ref.Project)
	if err != nil {
		return forge.PostResult{}, err
	}
	var rr gh.ReviewersRequest
	for _, u := range users {
		u = strings.TrimPrefix(u, "@")
		if _, team, ok := strings.Cut(u, "/"); ok {
			rr.TeamReviewers = append(rr.TeamReviewers, team)
		} else {
			rr.Reviewers = append(rr.Reviewers, u)
		}
	}
	got, _, err := p.client.PullRequests.RequestReviewers(ctx, owner, repo, pr.Ref.Number, rr)
	if err != nil {
		return forge.PostResult{}, wrap(err)
	}
	return forge.PostResult{URL: got.GetHTMLURL()}, nil
}
