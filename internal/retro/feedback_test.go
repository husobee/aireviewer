// SPDX-License-Identifier: MIT

package retro

import (
	"strings"
	"testing"
	"time"

	"github.com/husobee/aireviewer/internal/memory"
)

func TestSuggest(t *testing.T) {
	// Round-trip through the memory store, as the tool does.
	s := memory.Store{Dir: t.TempDir()}
	for i, m := range []map[string]any{
		{"usefulness_flag_rate": 0.5, "ai_acceptance_rate": 0.1, "time_to_first_review_h": 30.0,
			"disputed_by_category": map[string]int{"style": 2}, "dispute_reasons": []string{"we use tabs"}},
		{"usefulness_flag_rate": 0.4, "ai_acceptance_rate": 0.2, "time_to_first_review_h": 40.0,
			"disputed_by_category": map[string]int{"style": 1, "defect": 1}},
		{"usefulness_flag_rate": 0.3, "ai_acceptance_rate": 0.3, "time_to_first_review_h": 2.0, "toxicity_flag_rate": 0.0},
	} {
		if _, err := s.Save(memory.Entry{Host: "github.com", Project: "o/r", Number: i + 1, Ref: "o/r#" + string(rune('1'+i)), Metrics: m, SavedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	es, _ := s.List("github.com", "o/r")
	got := map[string]Suggestion{}
	for _, sg := range Suggest(es, 3) {
		got[sg.ID] = sg
	}
	if sg, ok := got["disputes-style"]; !ok || !strings.Contains(sg.Finding, "3 style findings") || !strings.Contains(sg.Finding, "we use tabs") || len(sg.Evidence) != 2 {
		t.Errorf("style disputes = %+v", sg)
	}
	if _, ok := got["disputes-defect"]; ok {
		t.Error("one dispute is not a pattern")
	}
	if sg, ok := got["usefulness"]; !ok || !strings.Contains(sg.Change, "usefulness: block") {
		t.Errorf("usefulness = %+v", sg)
	}
	if _, ok := got["acceptance"]; !ok {
		t.Error("low acceptance not suggested")
	}
	if sg, ok := got["first-review"]; !ok || !strings.Contains(sg.Finding, "30 hours") {
		t.Errorf("first review = %+v", sg)
	}
	if _, ok := got["toxicity"]; ok {
		t.Error("toxicity suggested from one clean PR")
	}
	if len(Suggest(es[:1], 3)) != 0 {
		t.Error("patterns from a single retrospective")
	}
}
