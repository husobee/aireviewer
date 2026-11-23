// SPDX-License-Identifier: MIT

package retro

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/husobee/aireviewer/internal/memory"
)

// Suggestion is a proposed change to how aireviewer is configured or used,
// derived from patterns across past retrospectives (paper l.1491-1497). It
// is advice for a human: nothing applies it automatically.
type Suggestion struct {
	ID        string   `json:"id"`
	Finding   string   `json:"finding" jsonschema:"the pattern, with counts"`
	Change    string   `json:"change" jsonschema:"what to change; a .aireviewer.yml snippet where applicable"`
	Rationale string   `json:"rationale"`
	Evidence  []string `json:"evidence" jsonschema:"PRs the pattern comes from"`
}

func num(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

type series struct {
	vals []float64
	refs []string
}

func (s *series) add(v float64, ref string) {
	s.vals = append(s.vals, v)
	s.refs = append(s.refs, ref)
}

func (s series) mean() float64 {
	t := 0.0
	for _, v := range s.vals {
		t += v
	}
	return t / float64(len(s.vals))
}

func (s series) median() float64 {
	v := append([]float64{}, s.vals...)
	sort.Float64s(v)
	if len(v)%2 == 1 {
		return v[len(v)/2]
	}
	return (v[len(v)/2-1] + v[len(v)/2]) / 2
}

// Suggest looks for patterns over at least minPRs retrospectives.
func Suggest(entries []memory.Entry, minPRs int) []Suggestion {
	if minPRs < 2 {
		minPRs = 3
	}
	var useful, toxic, accept, ttfr series
	disputes := map[string]int{}
	disputePRs := map[string][]string{}
	reasons := map[string][]string{}
	for _, e := range entries {
		m := e.Metrics
		if v, ok := num(m["usefulness_flag_rate"]); ok {
			useful.add(v, e.Ref)
		}
		if v, ok := num(m["toxicity_flag_rate"]); ok {
			toxic.add(v, e.Ref)
		}
		if v, ok := num(m["ai_acceptance_rate"]); ok {
			accept.add(v, e.Ref)
		}
		if v, ok := num(m["time_to_first_review_h"]); ok {
			ttfr.add(v, e.Ref)
		}
		if dc, ok := m["disputed_by_category"].(map[string]any); ok {
			for c, v := range dc {
				if n, ok := num(v); ok && n > 0 {
					disputes[c] += int(n)
					disputePRs[c] = append(disputePRs[c], e.Ref)
				}
			}
		}
		if rs, ok := m["dispute_reasons"].([]any); ok {
			for _, r := range rs {
				if s, ok := r.(string); ok {
					reasons[e.Ref] = append(reasons[e.Ref], s)
				}
			}
		}
	}
	var out []Suggestion
	pct := func(f float64) string { return fmt.Sprintf("%d%%", int(math.Round(f*100))) }

	cats := make([]string, 0, len(disputes))
	for c := range disputes {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	for _, c := range cats {
		prs := disputePRs[c]
		if disputes[c] < 3 || len(prs) < 2 {
			continue
		}
		var rs []string
		for _, p := range prs {
			rs = append(rs, reasons[p]...)
		}
		if len(rs) > 3 {
			rs = rs[:3]
		}
		change := "Add a repository policy that states the accepted practice, so the code reviewer stops raising it:\n" +
			"```yaml\npolicies:\n  rules:\n    - \"<the convention reviewers keep confirming for " + c + " findings>\"\n```"
		if c == "style" {
			change = "Style findings keep being disputed: document the style rule in `policies`, or rely on a linter in `static_analysis` instead of the reviewer."
		}
		out = append(out, Suggestion{ID: "disputes-" + c,
			Finding:   fmt.Sprintf("%d %s findings were disputed across %d PRs (reasons: %s)", disputes[c], c, len(prs), strings.Join(rs, "; ")),
			Change:    change,
			Rationale: "Repeatedly dismissed findings cost reviewer attention and erode trust in the rest (paper §4.5.5).",
			Evidence:  prs})
	}
	if len(useful.vals) >= minPRs && useful.mean() >= 0.3 {
		out = append(out, Suggestion{ID: "usefulness",
			Finding:   fmt.Sprintf("On average %s of drafted comments were flagged as not useful (over %d PRs)", pct(useful.mean()), len(useful.vals)),
			Change:    "Ask for actionable comments up front (\"make it actionable\" by default), or hold weak comments:\n```yaml\ncomments:\n  usefulness: block\n```",
			Rationale: "Vague comments delay merges without improving the change (paper §4.5.5).",
			Evidence:  useful.refs})
	}
	if len(toxic.vals) >= minPRs && toxic.mean() >= 0.15 {
		out = append(out, Suggestion{ID: "toxicity",
			Finding:   fmt.Sprintf("On average %s of drafted comments were flagged for tone (over %d PRs)", pct(toxic.mean()), len(toxic.vals)),
			Change:    "```yaml\ncomments:\n  toxicity: block\n```\nand consider a team voice (`comments.voice`) that sets the expected tone.",
			Rationale: "Tone problems were caught repeatedly; blocking makes the check mandatory rather than advisory (paper §4.5.4).",
			Evidence:  toxic.refs})
	}
	if len(accept.vals) >= minPRs && accept.mean() < 0.25 {
		out = append(out, Suggestion{ID: "acceptance",
			Finding:   fmt.Sprintf("Only %s of AI findings were posted on average (over %d PRs)", pct(accept.mean()), len(accept.vals)),
			Change:    "Keep the circuit breaker on, and narrow review to the riskiest units (`review_plan` with `only`), or add policies for the conventions behind dismissed findings.",
			Rationale: "A low acceptance rate means reviewers spend time triaging noise; precision matters more than coverage (paper §5.3).",
			Evidence:  accept.refs})
	}
	if len(ttfr.vals) >= minPRs && ttfr.median() > 24 {
		out = append(out, Suggestion{ID: "first-review",
			Finding:   fmt.Sprintf("The median time to first review is %.0f hours (over %d PRs)", ttfr.median(), len(ttfr.vals)),
			Change:    "Request reviewers when PRs are opened (`/pr-create` suggests them) and enable the CI summary so reviewers start with context:\n```yaml\nci:\n  post_augmentation: true\n```",
			Rationale: "Long waits for a first review are the main driver of review latency (paper §4.4).",
			Evidence:  ttfr.refs})
	}
	return out
}
