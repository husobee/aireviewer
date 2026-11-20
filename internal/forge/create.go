// SPDX-License-Identifier: MIT

package forge

import (
	"context"
	"fmt"
	"time"
)

// Interfaces used by /pr-create (SPEC §8). Read-only ones are used freely;
// IssueCreator and PRCreator are writes and are only called by proposal_submit
// after the human confirmed the frozen proposal (ADR 0003, ADR 0007).

// RepoInfo reports repository metadata.
type RepoInfo interface {
	DefaultBranch(ctx context.Context, repo Repo) (string, error)
}

// BranchPRFinder finds an open PR whose head is branch (same repository).
// It returns nil, nil when there is none.
type BranchPRFinder interface {
	FindOpenPR(ctx context.Context, repo Repo, branch string) (*PR, error)
}

// IssueSearch is one retrieval query (SPEC §8.2 v1).
type IssueSearch struct {
	Query string
	// ClosedWithin keeps closed issues closed at most this long ago; open
	// issues are always kept. Zero means open issues only.
	ClosedWithin time.Duration
	Limit        int
}

// IssueSearcher retrieves issues from the forge's search API.
type IssueSearcher interface {
	SearchIssues(ctx context.Context, repo Repo, q IssueSearch) ([]Issue, error)
	GetIssue(ctx context.Context, repo Repo, number int) (*Issue, error)
}

// NewIssue is an issue to create.
type NewIssue struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Labels []string `json:"labels,omitempty"`
}

// IssueCreator creates issues.
type IssueCreator interface {
	CreateIssue(ctx context.Context, repo Repo, is NewIssue) (*Issue, error)
}

// PRSpec describes a PR to open from Head into Base in the same repository.
type PRSpec struct {
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Base   string   `json:"base"`
	Head   string   `json:"head"`
	Draft  bool     `json:"draft"`
	Labels []string `json:"labels,omitempty"`
}

// PRCreator opens PRs.
type PRCreator interface {
	CreatePR(ctx context.Context, repo Repo, spec PRSpec) (*PR, error)
}

// ClosingLine is the forge's closing keyword for an issue in the same
// project (SPEC §8.4): GitHub "Fixes #N", GitLab "Closes #N".
func ClosingLine(kind Kind, number int) string {
	if kind == GitLab {
		return fmt.Sprintf("Closes #%d", number)
	}
	return fmt.Sprintf("Fixes #%d", number)
}
