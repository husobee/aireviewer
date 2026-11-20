// SPDX-License-Identifier: MIT

package gitlab

import (
	"context"
	"sort"
	"strings"

	gl "gitlab.com/gitlab-org/api/client-go"

	"github.com/husobee/aireviewer/internal/forge"
)

// Activity returns the MR's notes (conversation, inline and approvals) and
// commits, oldest first. GitLab has no review objects: approvals come from
// system notes, and the request-changes verdict from aireviewer is a note.
func (p *Provider) Activity(ctx context.Context, pr *forge.PR) (*forge.Activity, error) {
	iid := int64(pr.Ref.Number)
	mr, _, err := p.client.MergeRequests.GetMergeRequest(pr.Ref.Project, iid, nil, gl.WithContext(ctx))
	if err != nil {
		return nil, wrap(err)
	}
	a := &forge.Activity{MergedAt: mr.MergedAt, ClosedAt: mr.ClosedAt}
	if mr.CreatedAt != nil {
		a.CreatedAt = *mr.CreatedAt
	}
	if mr.Author != nil {
		a.Author = mr.Author.Username
	}
	opts := &gl.ListMergeRequestNotesOptions{ListOptions: gl.ListOptions{PerPage: 100}}
	for page := int64(1); page <= 10; page++ {
		opts.Page = page
		notes, resp, err := p.client.Notes.ListMergeRequestNotes(pr.Ref.Project, iid, opts, gl.WithContext(ctx))
		if err != nil {
			return nil, wrap(err)
		}
		for _, n := range notes {
			e := forge.ActivityEvent{Author: n.Author.Username, Body: n.Body, Resolved: n.Resolved}
			if n.CreatedAt != nil {
				e.At = *n.CreatedAt
			}
			switch {
			case n.System && strings.HasPrefix(strings.TrimSpace(n.Body), "approved this merge request"):
				e.Kind, e.State = forge.EventApproval, "approved"
			case n.System:
				continue
			case n.Position != nil && n.Position.NewPath != "":
				e.Kind, e.Path, e.Line = forge.EventInline, n.Position.NewPath, int(n.Position.NewLine)
			default:
				e.Kind = forge.EventComment
			}
			forge.ClassifyBody(&e)
			a.Events = append(a.Events, e)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		a.Truncated = a.Truncated || page == 10
	}
	commits, _, err := p.client.MergeRequests.GetMergeRequestCommits(pr.Ref.Project, iid,
		&gl.GetMergeRequestCommitsOptions{ListOptions: gl.ListOptions{PerPage: 100}}, gl.WithContext(ctx))
	if err != nil {
		return nil, wrap(err)
	}
	for _, c := range commits {
		e := forge.ActivityEvent{Kind: forge.EventCommit, SHA: c.ID, Author: c.AuthorName, Body: c.Title}
		if c.CommittedDate != nil {
			e.At = *c.CommittedDate
		} else if c.CreatedAt != nil {
			e.At = *c.CreatedAt
		}
		a.Events = append(a.Events, e)
	}
	sort.SliceStable(a.Events, func(i, j int) bool { return a.Events[i].At.Before(a.Events[j].At) })
	return a, nil
}
