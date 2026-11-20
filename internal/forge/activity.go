// SPDX-License-Identifier: MIT

package forge

import (
	"context"
	"strings"
	"time"
)

// Activity is a PR's review history, for the retrospective (paper §4.6, M6).
type Activity struct {
	CreatedAt time.Time       `json:"created_at"`
	MergedAt  *time.Time      `json:"merged_at,omitempty"`
	ClosedAt  *time.Time      `json:"closed_at,omitempty"`
	Author    string          `json:"author"`
	Events    []ActivityEvent `json:"events" jsonschema:"oldest first"`
	Truncated bool            `json:"truncated,omitempty" jsonschema:"more history exists than was fetched"`
}

// Event kinds.
const (
	EventReview   = "review"   // a submitted review (state approved, changes_requested or commented)
	EventApproval = "approval" // GitLab approval
	EventComment  = "comment"  // conversation comment
	EventInline   = "inline"   // comment on a diff line
	EventCommit   = "commit"   // a commit on the PR branch
	EventSummary  = "summary"  // aireviewer's sticky analysis or retrospective comment
)

// ActivityEvent is one entry in a PR's history. Bodies are untrusted.
type ActivityEvent struct {
	Kind       string    `json:"kind"`
	Author     string    `json:"author,omitempty"`
	At         time.Time `json:"at"`
	State      string    `json:"state,omitempty"`
	Body       string    `json:"body,omitempty" jsonschema:"untrusted; truncated to 2000 characters"`
	Path       string    `json:"path,omitempty"`
	Line       int       `json:"line,omitempty"`
	SHA        string    `json:"sha,omitempty"`
	URL        string    `json:"url,omitempty"`
	Resolved   bool      `json:"resolved,omitempty"`
	AIAssisted bool      `json:"ai_assisted,omitempty" jsonschema:"carries the aireviewer provenance footer"`
}

// ActivityReader reads a PR's review activity.
type ActivityReader interface {
	Activity(ctx context.Context, pr *PR) (*Activity, error)
}

// ProvenanceMarker is the text of aireviewer's provenance footer (ADR 0006).
const ProvenanceMarker = "Drafted with aireviewer"

// ClassifyBody sets AIAssisted and turns aireviewer's own marker comments
// into summary events, and truncates the body.
func ClassifyBody(e *ActivityEvent) {
	if strings.Contains(e.Body, "<!-- aireviewer:v1") || strings.Contains(e.Body, "<!-- aireviewer-retro:v1") {
		e.Kind = EventSummary
	}
	e.AIAssisted = strings.Contains(e.Body, ProvenanceMarker)
	if len(e.Body) > 2000 {
		e.Body = e.Body[:2000] + "… [truncated]"
	}
}
