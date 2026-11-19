// SPDX-License-Identifier: MIT

package gitlab

import (
	"context"
	"fmt"
	"strings"

	gl "gitlab.com/gitlab-org/api/client-go"

	"github.com/husobee/aireviewer/internal/forge"
)

// M3: gated writes. Called only by the MCP server after a frozen proposal has
// been confirmed by a human.

// CurrentUser returns the authenticated username.
func (p *Provider) CurrentUser(ctx context.Context, _ forge.Repo) (string, error) {
	u, _, err := p.client.Users.CurrentUser(gl.WithContext(ctx))
	if err != nil {
		return "", wrap(err)
	}
	return u.Username, nil
}

func ptr[T any](v T) *T { return &v }

// SubmitReview posts the review as draft notes published in one batch, then
// applies the verdict. GitLab's API has no "request changes" call, so that
// verdict is expressed in the summary note and reported in the result.
func (p *Provider) SubmitReview(ctx context.Context, pr *forge.PR, r forge.Review) (forge.PostResult, error) {
	pid, iid := pr.Ref.Project, int64(pr.Ref.Number)
	res := forge.PostResult{URL: pr.WebURL}
	for i, c := range r.Comments {
		pos := &gl.PositionOptions{BaseSHA: ptr(r.BaseSHA), StartSHA: ptr(r.StartSHA), HeadSHA: ptr(r.HeadSHA),
			PositionType: ptr("text"), NewPath: ptr(c.Path), OldPath: ptr(c.Path)}
		if c.OldPath != "" {
			pos.OldPath = ptr(c.OldPath)
		}
		if c.Side == "old" {
			pos.OldLine = ptr(int64(c.Line))
		} else {
			pos.NewLine = ptr(int64(c.Line))
			if c.ContextOldLine > 0 {
				pos.OldLine = ptr(int64(c.ContextOldLine))
			}
		}
		if _, _, err := p.client.DraftNotes.CreateDraftNote(pid, iid, &gl.CreateDraftNoteOptions{
			Note: ptr(c.Body), CommitID: ptr(r.HeadSHA), Position: pos}, gl.WithContext(ctx)); err != nil {
			return res, fmt.Errorf("draft note %d (%s:%d): %w", i+1, c.Path, c.Line, wrap(err))
		}
	}
	body := r.Body
	if r.Verdict == forge.VerdictRequestChanges && !strings.Contains(body, "Changes requested") {
		body = "**Changes requested.**\n\n" + body
	}
	if strings.TrimSpace(body) != "" {
		if _, _, err := p.client.DraftNotes.CreateDraftNote(pid, iid, &gl.CreateDraftNoteOptions{Note: ptr(body)}, gl.WithContext(ctx)); err != nil {
			return res, fmt.Errorf("summary note: %w", wrap(err))
		}
	}
	if _, err := p.client.DraftNotes.PublishAllDraftNotes(pid, iid, gl.WithContext(ctx)); err != nil {
		return res, fmt.Errorf("publish draft notes: %w", wrap(err))
	}
	switch r.Verdict {
	case forge.VerdictApprove:
		if _, _, err := p.client.MergeRequestApprovals.ApproveMergeRequest(pid, iid,
			&gl.ApproveMergeRequestOptions{SHA: ptr(r.HeadSHA)}, gl.WithContext(ctx)); err != nil {
			return res, fmt.Errorf("comments published, but approval failed: %w", wrap(err))
		}
		res.Notes = append(res.Notes, "approved")
	case forge.VerdictRequestChanges:
		res.Notes = append(res.Notes, "GitLab API has no request-changes call: the summary note states that changes are requested, and the MR was not approved")
	}
	return res, nil
}

// FindMarkerComment returns the newest non-system MR note containing marker.
func (p *Provider) FindMarkerComment(ctx context.Context, pr *forge.PR, marker string) (*forge.MarkerComment, error) {
	var found *forge.MarkerComment
	opt := &gl.ListMergeRequestNotesOptions{ListOptions: gl.ListOptions{PerPage: 100}}
	for {
		notes, resp, err := p.client.Notes.ListMergeRequestNotes(pr.Ref.Project, int64(pr.Ref.Number), opt, gl.WithContext(ctx))
		if err != nil {
			return nil, wrap(err)
		}
		for _, n := range notes {
			if n.System || !strings.Contains(n.Body, marker) {
				continue
			}
			mc := &forge.MarkerComment{ID: n.ID, Author: n.Author.Username, Body: n.Body, URL: fmt.Sprintf("%s#note_%d", pr.WebURL, n.ID)}
			if found == nil || n.ID > found.ID {
				found = mc
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	return found, nil
}

// UpsertMarkerComment edits the marked note if it exists, else creates it.
func (p *Provider) UpsertMarkerComment(ctx context.Context, pr *forge.PR, marker, body string) (forge.PostResult, error) {
	existing, err := p.FindMarkerComment(ctx, pr, marker)
	if err != nil {
		return forge.PostResult{}, err
	}
	pid, iid := pr.Ref.Project, int64(pr.Ref.Number)
	if existing != nil {
		if _, _, err := p.client.Notes.UpdateMergeRequestNote(pid, iid, existing.ID,
			&gl.UpdateMergeRequestNoteOptions{Body: ptr(body)}, gl.WithContext(ctx)); err != nil {
			return forge.PostResult{}, wrap(err)
		}
		return forge.PostResult{URL: existing.URL, Notes: []string{"updated existing note"}}, nil
	}
	n, _, err := p.client.Notes.CreateMergeRequestNote(pid, iid, &gl.CreateMergeRequestNoteOptions{Body: ptr(body)}, gl.WithContext(ctx))
	if err != nil {
		return forge.PostResult{}, wrap(err)
	}
	return forge.PostResult{URL: fmt.Sprintf("%s#note_%d", pr.WebURL, n.ID), Notes: []string{"created note"}}, nil
}

// RequestReviewers adds users as reviewers, keeping existing ones (the
// GitLab API replaces the whole reviewer set).
func (p *Provider) RequestReviewers(ctx context.Context, pr *forge.PR, users []string) (forge.PostResult, error) {
	pid, iid := pr.Ref.Project, int64(pr.Ref.Number)
	mr, _, err := p.client.MergeRequests.GetMergeRequest(pid, iid, nil, gl.WithContext(ctx))
	if err != nil {
		return forge.PostResult{}, wrap(err)
	}
	ids := []int64{}
	seen := map[int64]bool{}
	for _, r := range mr.Reviewers {
		if r != nil && !seen[r.ID] {
			ids, seen[r.ID] = append(ids, r.ID), true
		}
	}
	res := forge.PostResult{URL: pr.WebURL}
	for _, u := range users {
		u = strings.TrimPrefix(u, "@")
		if strings.Contains(u, "/") {
			res.Notes = append(res.Notes, "skipped group "+u+": GitLab reviewers must be users")
			continue
		}
		found, _, err := p.client.Users.ListUsers(&gl.ListUsersOptions{Username: ptr(u)}, gl.WithContext(ctx))
		if err != nil {
			return res, wrap(err)
		}
		if len(found) == 0 {
			res.Notes = append(res.Notes, "unknown user "+u)
			continue
		}
		if !seen[found[0].ID] {
			ids, seen[found[0].ID] = append(ids, found[0].ID), true
		}
	}
	if _, _, err := p.client.MergeRequests.UpdateMergeRequest(pid, iid, &gl.UpdateMergeRequestOptions{ReviewerIDs: &ids}, gl.WithContext(ctx)); err != nil {
		return res, wrap(err)
	}
	return res, nil
}
