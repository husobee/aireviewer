// SPDX-License-Identifier: MIT

package retro

import (
	"testing"
	"time"

	"github.com/husobee/aireviewer/internal/decisionlog"
	"github.com/husobee/aireviewer/internal/forge"
)

func TestFromLog(t *testing.T) {
	ev := func(event string, data map[string]any) decisionlog.Entry {
		return decisionlog.Entry{Event: event, Data: data}
	}
	entries := []decisionlog.Entry{
		ev("comment_drafted", map[string]any{"id": "C1", "from_finding": "code-reviewer:F1"}),
		ev("comment_drafted", map[string]any{"id": "C2", "from_finding": "code-reviewer:F2"}),
		ev("comment_drafted", map[string]any{"id": "C3", "from_finding": ""}),
		ev("comment_drafted", map[string]any{"id": "C4", "from_finding": "code-reviewer:F4"}),
		ev("comment_removed", map[string]any{"id": "C4"}),
		ev("comment_checked", map[string]any{"id": "C1", "check": "toxicity", "verdict": "pass"}),
		ev("comment_checked", map[string]any{"id": "C2", "check": "toxicity", "verdict": "warn"}),
		ev("comment_checked", map[string]any{"id": "C1", "check": "usefulness", "verdict": "pass"}),
		ev("finding_disputed", map[string]any{"finding": "code-reviewer:F3", "reason": "intended behaviour"}),
		ev("finding_disputed", map[string]any{"finding": "code-reviewer:F3", "reason": "again"}),
		ev("proposal_created", map[string]any{"id": "P1", "kind": "review", "comments": []any{"C1", "C3"}, "overrides": map[string]any{"C1": "ok"}}),
		ev("human_declined", map[string]any{"id": "P1"}),
		ev("proposal_created", map[string]any{"id": "P2", "kind": "review", "comments": []any{"C1", "C3"}, "overrides": map[string]any{}}),
		ev("posted", map[string]any{"id": "P2", "kind": "review"}),
		ev("posted", map[string]any{"id": "P3", "kind": "sticky"}),
	}
	l := FromLog(entries, 5)
	if !l.HasData || l.FindingsTotal != 5 || l.FindingsDrafted != 2 || l.FindingsPosted != 1 || l.FindingsDisputed != 1 ||
		l.CommentsPosted != 2 || l.Overrides != 1 || l.Declines != 1 || l.Posts["review"] != 1 || l.Posts["sticky"] != 1 {
		t.Errorf("local = %+v", l)
	}
	if l.Checks["toxicity"]["warn"] != 1 || len(l.DisputeReasons) != 1 {
		t.Errorf("checks/disputes = %+v %v", l.Checks, l.DisputeReasons)
	}
	if FromLog(nil, 0).HasData {
		t.Error("empty log reported data")
	}
}

func TestCompute(t *testing.T) {
	c := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	merged := c.Add(50 * time.Hour)
	a := &forge.Activity{CreatedAt: c, MergedAt: &merged, Author: "alice", Events: []forge.ActivityEvent{
		{Kind: forge.EventCommit, Author: "alice", At: c.Add(-time.Hour)},
		{Kind: forge.EventComment, Author: "dependabot", At: c.Add(time.Hour), Body: "bump"},
		{Kind: forge.EventSummary, Author: "bob", At: c.Add(2 * time.Hour)},
		{Kind: forge.EventComment, Author: "alice", At: c.Add(3 * time.Hour), Body: "ping"},
		{Kind: forge.EventReview, Author: "bob", At: c.Add(6 * time.Hour), State: "changes_requested", Body: "see inline", AIAssisted: true},
		{Kind: forge.EventInline, Author: "bob", At: c.Add(6 * time.Hour), Body: "nil check?", AIAssisted: true},
		{Kind: forge.EventInline, Author: "carol", At: c.Add(7 * time.Hour), Body: "agree"},
		{Kind: forge.EventCommit, Author: "alice", At: c.Add(20 * time.Hour)},
		{Kind: forge.EventApproval, Author: "carol", At: c.Add(40 * time.Hour), State: "approved"},
	}}
	m := Compute(a, Local{FindingsTotal: 4, FindingsPosted: 1, FindingsDisputed: 2,
		Checks: map[string]map[string]int{"toxicity": {"pass": 3, "warn": 1}}}, []string{"dependabot"})
	if m.State != "merged" || *m.TimeToFirstReviewHours != 6 || *m.DecisionLatencyHours != 50 || m.ReviewRounds != 2 ||
		m.ChangesRequested != 1 || m.CommitsAfterFirstReview != 1 {
		t.Errorf("timing = %+v", m)
	}
	if len(m.Reviewers) != 2 || m.Reviewers[0] != "bob" || len(m.ApprovedBy) != 1 || m.ApprovedBy[0] != "carol" {
		t.Errorf("reviewers = %v approved = %v", m.Reviewers, m.ApprovedBy)
	}
	if m.Comments.AIAssisted != 2 || m.Comments.Human != 2 || m.Comments.Inline != 2 || m.Comments.Summaries != 1 {
		t.Errorf("comments = %+v", m.Comments)
	}
	if *m.AIAcceptanceRate != 0.25 || *m.AIDismissalRate != 0.5 || *m.ToxicityFlagRate != 0.25 || m.UsefulnessFlagRate != nil {
		t.Errorf("rates = %v %v %v %v", *m.AIAcceptanceRate, *m.AIDismissalRate, *m.ToxicityFlagRate, m.UsefulnessFlagRate)
	}
	open := Compute(&forge.Activity{CreatedAt: c, Author: "a"}, Local{}, nil)
	if open.State != "open" || open.DecisionLatencyHours != nil || open.TimeToFirstReviewHours != nil || open.AIAcceptanceRate != nil {
		t.Errorf("open PR = %+v", open)
	}
}
