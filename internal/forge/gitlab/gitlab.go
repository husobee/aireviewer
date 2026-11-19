// SPDX-License-Identifier: MIT

// Package gitlab implements forge.Provider for gitlab.com and self-managed
// GitLab instances.
package gitlab

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	gl "gitlab.com/gitlab-org/api/client-go"

	"github.com/husobee/aireviewer/internal/forge"
)

// Provider talks to one GitLab host.
type Provider struct {
	host   string
	client *gl.Client
}

// Options configures a Provider.
type Options struct {
	Host string // gitlab.com or a self-managed hostname
	// Token is a personal/project access token. If empty and JobToken is
	// set, CI job-token auth is used. Public projects work unauthenticated.
	Token    string
	JobToken string
	// BaseURL overrides the API endpoint (tests). Defaults to https://<host>/api/v4.
	BaseURL    string
	HTTPClient *http.Client
}

// New builds a GitLab provider.
func New(o Options) (*Provider, error) {
	base := o.BaseURL
	if base == "" {
		base = "https://" + o.Host + "/api/v4"
	}
	hc := o.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	opts := []gl.ClientOptionFunc{gl.WithBaseURL(base), gl.WithHTTPClient(hc)}
	var (
		c   *gl.Client
		err error
	)
	if o.Token == "" && o.JobToken != "" {
		c, err = gl.NewJobClient(o.JobToken, opts...)
	} else {
		c, err = gl.NewClient(o.Token, opts...)
	}
	if err != nil {
		return nil, fmt.Errorf("gitlab client: %w", err)
	}
	return &Provider{host: o.Host, client: c}, nil
}

func (p *Provider) Kind() forge.Kind { return forge.GitLab }

func wrap(err error) error {
	var er *gl.ErrorResponse
	if errors.Is(err, gl.ErrNotFound) ||
		(errors.As(err, &er) && er.Response != nil && er.Response.StatusCode == http.StatusNotFound) {
		return fmt.Errorf("%w: %v", forge.ErrNotFound, err)
	}
	return err
}

// GetPR fetches merge request metadata.
func (p *Provider) GetPR(ctx context.Context, ref forge.PRRef) (*forge.PR, error) {
	mr, _, err := p.client.MergeRequests.GetMergeRequest(ref.Project, int64(ref.Number), nil, gl.WithContext(ctx))
	if err != nil {
		return nil, wrap(err)
	}
	state := forge.StateOpen
	switch mr.State {
	case "merged":
		state = forge.StateMerged
	case "closed", "locked":
		state = forge.StateClosed
	}
	out := &forge.PR{
		Ref:        ref,
		Noun:       forge.GitLab.Noun(),
		Title:      mr.Title,
		Body:       mr.Description,
		State:      state,
		Draft:      mr.Draft,
		BaseBranch: mr.TargetBranch,
		HeadBranch: mr.SourceBranch,
		BaseSHA:    mr.DiffRefs.BaseSha,
		HeadSHA:    mr.DiffRefs.HeadSha,
		StartSHA:   mr.DiffRefs.StartSha,
		WebURL:     mr.WebURL,
		Labels:     append([]string{}, mr.Labels...),
		Reviewers:  []string{},
	}
	if out.HeadSHA == "" {
		out.HeadSHA = mr.SHA
	}
	if mr.Author != nil {
		out.Author = mr.Author.Username
	}
	if mr.CreatedAt != nil {
		out.CreatedAt = *mr.CreatedAt
	}
	for _, u := range mr.Reviewers {
		if u != nil {
			out.Reviewers = append(out.Reviewers, u.Username)
		}
	}
	return out, nil
}

// GetDiff fetches the per-file changes of a merge request.
func (p *Provider) GetDiff(ctx context.Context, ref forge.PRRef) (*forge.Diff, error) {
	d := &forge.Diff{Files: []forge.FileChange{}}
	opt := &gl.ListMergeRequestDiffsOptions{ListOptions: gl.ListOptions{PerPage: 100}}
	for {
		diffs, resp, err := p.client.MergeRequests.ListMergeRequestDiffs(ref.Project, int64(ref.Number), opt, gl.WithContext(ctx))
		if err != nil {
			return nil, wrap(err)
		}
		for _, f := range diffs {
			d.Files = append(d.Files, convertDiff(f))
		}
		if resp.NextPage == 0 {
			break
		}
		opt.Page = resp.NextPage
	}
	d.ComputeStats()
	return d, nil
}

func convertDiff(f *gl.MergeRequestDiff) forge.FileChange {
	fc := forge.FileChange{Path: f.NewPath, Patch: f.Diff}
	switch {
	case f.NewFile:
		fc.Status = forge.FileAdded
	case f.DeletedFile:
		fc.Status = forge.FileDeleted
		fc.Path = f.OldPath
	case f.RenamedFile:
		fc.Status = forge.FileRenamed
		fc.OldPath = f.OldPath
	default:
		fc.Status = forge.FileModified
	}
	fc.Additions, fc.Deletions = forge.CountPatch(f.Diff)
	fc.Truncated = f.TooLarge || f.Collapsed
	return fc
}
