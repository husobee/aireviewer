// SPDX-License-Identifier: MIT

// Package retro computes deterministic review metrics for the PR
// retrospective (paper §4.6.2, SPEC §11.2): timing, iterations, comment
// origin, and what happened to AI findings, from the forge's activity and
// the local decision log. Nothing here is an LLM judgement.
package retro

import (
	"math"
	"sort"
	"strings"

	"github.com/husobee/aireviewer/internal/decisionlog"
	"github.com/husobee/aireviewer/internal/forge"
)

// Local summarises the reviewer's local aireviewer trail (decision log).
type Local struct {
	HasData          bool     `json:"has_data" jsonschema:"false when this PR was not reviewed with aireviewer on this machine"`
	FindingsTotal    int      `json:"findings_total" jsonschema:"code-review findings across reviewed heads"`
	FindingsDrafted  int      `json:"findings_drafted" jsonschema:"findings turned into a draft comment"`
	FindingsPosted   int      `json:"findings_posted" jsonschema:"findings whose comment was posted in a review"`
	FindingsDisputed int      `json:"findings_disputed" jsonschema:"findings the reviewer disputed (dismissed)"`
	DisputeReasons   []string `json:"dispute_reasons,omitempty"`
	DisputedFindings []string `json:"disputed_findings,omitempty"`
	// DisputedByCategory is filled by the caller from the stored reports.
	DisputedByCategory map[string]int            `json:"disputed_by_category,omitempty"`
	CommentsPosted     int                       `json:"comments_posted"`
	Checks             map[string]map[string]int `json:"checks" jsonschema:"check -> verdict -> count (toxicity, usefulness)"`
	Overrides          int                       `json:"overrides" jsonschema:"gate holds the reviewer overrode with a reason"`
	Declines           int                       `json:"declines"`
	Posts              map[string]int            `json:"posts" jsonschema:"posted proposals by kind"`
}

// FromLog derives Local from decision-log entries. findingsTotal comes from
// the stored code-review reports.
func FromLog(entries []decisionlog.Entry, findingsTotal int) Local {
	l := Local{FindingsTotal: findingsTotal, Checks: map[string]map[string]int{}, Posts: map[string]int{}}
	drafted := map[string]string{} // comment id -> finding
	removed := map[string]bool{}
	reviewComments := map[string][]string{}
	disputed := map[string]bool{}
	postedFinding := map[string]bool{}
	for _, e := range entries {
		l.HasData = true
		d := e.Data
		switch e.Event {
		case "comment_drafted":
			drafted[str(d["id"])] = str(d["from_finding"])
		case "comment_removed":
			removed[str(d["id"])] = true
		case "comment_checked":
			c, v := str(d["check"]), str(d["verdict"])
			if l.Checks[c] == nil {
				l.Checks[c] = map[string]int{}
			}
			l.Checks[c][v]++
		case "finding_disputed":
			if f := str(d["finding"]); !disputed[f] {
				disputed[f] = true
				if r := str(d["reason"]); r != "" {
					l.DisputeReasons = append(l.DisputeReasons, r)
				}
			}
		case "proposal_created":
			if str(d["kind"]) == "review" {
				reviewComments[str(d["id"])] = strs(d["comments"])
				if o, ok := d["overrides"].(map[string]any); ok {
					l.Overrides += len(o)
				}
			}
		case "human_declined":
			l.Declines++
		case "posted":
			kind := str(d["kind"])
			l.Posts[kind]++
			if kind == "review" {
				for _, c := range reviewComments[str(d["id"])] {
					l.CommentsPosted++
					if f := drafted[c]; f != "" {
						postedFinding[f] = true
					}
				}
			}
		}
	}
	findings := map[string]bool{}
	for id, f := range drafted {
		if f != "" && !removed[id] {
			findings[f] = true
		}
	}
	l.FindingsDrafted = len(findings)
	l.FindingsPosted = len(postedFinding)
	l.FindingsDisputed = len(disputed)
	for f := range disputed {
		l.DisputedFindings = append(l.DisputedFindings, f)
	}
	sort.Strings(l.DisputedFindings)
	return l
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func strs(v any) []string {
	var out []string
	if a, ok := v.([]any); ok {
		for _, x := range a {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// Comments counts comments by origin and kind. Human comments drafted with
// aireviewer carry the provenance footer and count as ai_assisted.
type Comments struct {
	Human        int `json:"human"`
	AIAssisted   int `json:"ai_assisted"`
	Inline       int `json:"inline"`
	Conversation int `json:"conversation"`
	Summaries    int `json:"summaries" jsonschema:"aireviewer analysis/retrospective comments"`
}

// Metrics is the deterministic retrospective.
type Metrics struct {
	State                   string   `json:"state" jsonschema:"merged, closed or open"`
	Author                  string   `json:"author"`
	TimeToFirstReviewHours  *float64 `json:"time_to_first_review_h,omitempty" jsonschema:"creation to the first review activity by someone other than the author or a bot"`
	DecisionLatencyHours    *float64 `json:"decision_latency_h,omitempty" jsonschema:"creation to merge or close"`
	ReviewRounds            int      `json:"review_rounds" jsonschema:"submitted reviews and approvals by reviewers"`
	ChangesRequested        int      `json:"changes_requested"`
	CommitsAfterFirstReview int      `json:"commits_after_first_review" jsonschema:"iterations: commits pushed after review started"`
	Reviewers               []string `json:"reviewers"`
	ApprovedBy              []string `json:"approved_by"`
	Comments                Comments `json:"comments"`
	Local                   Local    `json:"local"`
	// Rates are nil when their denominator is zero.
	AIAcceptanceRate   *float64 `json:"ai_acceptance_rate,omitempty" jsonschema:"findings posted / findings total"`
	AIDismissalRate    *float64 `json:"ai_dismissal_rate,omitempty" jsonschema:"findings disputed / findings total"`
	ToxicityFlagRate   *float64 `json:"toxicity_flag_rate,omitempty" jsonschema:"warn+fail / toxicity checks"`
	UsefulnessFlagRate *float64 `json:"usefulness_flag_rate,omitempty" jsonschema:"warn+fail / usefulness checks"`
	HistoryTruncated   bool     `json:"history_truncated,omitempty"`
}

func rate(n, d int) *float64 {
	if d == 0 {
		return nil
	}
	r := math.Round(float64(n)/float64(d)*1000) / 1000
	return &r
}

func hours(f float64) *float64 {
	h := math.Round(f*10) / 10
	return &h
}

// Compute builds Metrics. bots are logins excluded from reviewer activity.
func Compute(a *forge.Activity, local Local, bots []string) Metrics {
	m := Metrics{State: "open", Author: a.Author, Local: local, Reviewers: []string{}, ApprovedBy: []string{}, HistoryTruncated: a.Truncated}
	isBot := func(u string) bool {
		lu := strings.ToLower(u)
		if strings.HasSuffix(lu, "[bot]") || strings.HasSuffix(lu, "-bot") {
			return true
		}
		for _, b := range bots {
			if strings.EqualFold(b, u) {
				return true
			}
		}
		return false
	}
	var end = a.ClosedAt
	if a.MergedAt != nil {
		m.State, end = "merged", a.MergedAt
	} else if a.ClosedAt != nil {
		m.State = "closed"
	}
	if end != nil {
		m.DecisionLatencyHours = hours(end.Sub(a.CreatedAt).Hours())
	}
	reviewers, approvers := map[string]bool{}, map[string]bool{}
	var firstReview *forge.ActivityEvent
	for i := range a.Events {
		e := &a.Events[i]
		switch e.Kind {
		case forge.EventSummary:
			m.Comments.Summaries++
			continue
		case forge.EventCommit:
			if firstReview != nil && e.At.After(firstReview.At) {
				m.CommitsAfterFirstReview++
			}
			continue
		}
		if e.Kind == forge.EventComment || e.Kind == forge.EventInline || (e.Kind == forge.EventReview && strings.TrimSpace(e.Body) != "") {
			if e.AIAssisted {
				m.Comments.AIAssisted++
			} else if !isBot(e.Author) {
				m.Comments.Human++
			}
			if e.Kind == forge.EventInline {
				m.Comments.Inline++
			} else {
				m.Comments.Conversation++
			}
		}
		if strings.EqualFold(e.Author, a.Author) || isBot(e.Author) {
			continue
		}
		if firstReview == nil {
			firstReview = e
		}
		reviewers[e.Author] = true
		if e.Kind == forge.EventReview || e.Kind == forge.EventApproval {
			m.ReviewRounds++
			if e.State == "approved" {
				approvers[e.Author] = true
			}
			if e.State == "changes_requested" {
				m.ChangesRequested++
			}
		}
	}
	if firstReview != nil {
		m.TimeToFirstReviewHours = hours(firstReview.At.Sub(a.CreatedAt).Hours())
	}
	for r := range reviewers {
		m.Reviewers = append(m.Reviewers, r)
	}
	for r := range approvers {
		m.ApprovedBy = append(m.ApprovedBy, r)
	}
	sort.Strings(m.Reviewers)
	sort.Strings(m.ApprovedBy)
	m.AIAcceptanceRate = rate(local.FindingsPosted, local.FindingsTotal)
	m.AIDismissalRate = rate(local.FindingsDisputed, local.FindingsTotal)
	flags := func(c string) (int, int) {
		n, t := 0, 0
		for v, k := range local.Checks[c] {
			t += k
			if v == "warn" || v == "fail" {
				n += k
			}
		}
		return n, t
	}
	if n, t := flags("toxicity"); t > 0 {
		m.ToxicityFlagRate = rate(n, t)
	}
	if n, t := flags("usefulness"); t > 0 {
		m.UsefulnessFlagRate = rate(n, t)
	}
	return m
}
