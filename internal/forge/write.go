// SPDX-License-Identifier: MIT

package forge

import (
	"context"
	"fmt"
	"strings"
)

// Forge write operations. Every implementation is called only by the MCP
// server after a frozen proposal has been confirmed by a human (SPEC §4,
// ADR 0003). Nothing in this file is exposed to subagents.

// Verdict is the reviewer's decision.
type Verdict string

const (
	VerdictApprove        Verdict = "approve"
	VerdictRequestChanges Verdict = "request_changes"
	VerdictComment        Verdict = "comment"
)

// ReviewComment is one inline comment anchored to the diff.
type ReviewComment struct {
	Path      string `json:"path"`
	OldPath   string `json:"old_path,omitempty"`
	Side      string `json:"side" jsonschema:"new (head) or old (merge base)"`
	Line      int    `json:"line" jsonschema:"last line of the commented range"`
	StartLine int    `json:"start_line,omitempty" jsonschema:"first line of a multi-line range"`
	// ContextOldLine is the merge-base line number when Line (new side) is an
	// unchanged context line; GitLab positions need both numbers then.
	ContextOldLine int    `json:"context_old_line,omitempty"`
	Body           string `json:"body"`
}

// Review is a batched review submission.
type Review struct {
	Verdict  Verdict         `json:"verdict"`
	Body     string          `json:"body"`
	Comments []ReviewComment `json:"comments"`
	HeadSHA  string          `json:"head_sha"`
	BaseSHA  string          `json:"base_sha"`
	StartSHA string          `json:"start_sha,omitempty"`
}

// PostResult describes what a write produced.
type PostResult struct {
	URL   string   `json:"url,omitempty"`
	Notes []string `json:"notes,omitempty"`
}

// MarkerComment is an existing top-level PR comment carrying a marker.
type MarkerComment struct {
	ID     int64  `json:"id"`
	Author string `json:"author"`
	Body   string `json:"body"`
	URL    string `json:"url"`
}

// Identity reports the authenticated user.
type Identity interface {
	CurrentUser(ctx context.Context, repo Repo) (string, error)
}

// ReviewPoster submits batched reviews.
type ReviewPoster interface {
	SubmitReview(ctx context.Context, pr *PR, r Review) (PostResult, error)
}

// MarkerCommenter finds and upserts a single marked comment (the sticky
// augmentation summary).
type MarkerCommenter interface {
	FindMarkerComment(ctx context.Context, pr *PR, marker string) (*MarkerComment, error)
	UpsertMarkerComment(ctx context.Context, pr *PR, marker, body string) (PostResult, error)
}

// ReviewerRequester requests reviewers on a PR.
type ReviewerRequester interface {
	RequestReviewers(ctx context.Context, pr *PR, users []string) (PostResult, error)
}

// SuggestionBlock renders a forge-native suggested change that replaces the
// commented lines [start, end] (new side) with replacement.
//
//	GitHub: ```suggestion        (the comment spans start..end)
//	GitLab: ```suggestion:-N+0   (the comment sits on end; N lines above it)
func SuggestionBlock(kind Kind, replacement string, start, end int) string {
	fence := "```"
	for strings.Contains(replacement, fence) {
		fence += "`"
	}
	header := "suggestion"
	if kind == GitLab {
		if start <= 0 || start > end {
			start = end
		}
		header = fmt.Sprintf("suggestion:-%d+0", end-start)
	}
	replacement = strings.TrimSuffix(replacement, "\n")
	return fence + header + "\n" + replacement + "\n" + fence
}
