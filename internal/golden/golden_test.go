// SPDX-License-Identifier: MIT

package golden

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/schema"
)

func claim(id, cat, sev, file string, a, b int, stmt string) schema.Claim {
	return schema.Claim{ID: id, Statement: stmt, Category: cat, Severity: sev,
		Evidence: []schema.Evidence{{Kind: "code", File: file, Lines: []int{a, b}}}}
}

func TestScore(t *testing.T) {
	c := Case{Name: "ttl", URL: "https://github.com/o/r/pull/3", Alignment: "exact", Risk: "low",
		Findings: []Expected{
			{ID: "race", File: "store/ttl.go", Lines: []int{33, 40}, Category: "defect", Match: "lock|race", SeverityMin: "major"},
			{ID: "leak", File: "store/ttl.go", Lines: []int{60, 66}, Category: "defect"},
			{ID: "magic", File: "store/ttl.go", Lines: []int{58, 58}, Category: "style"},
		},
		Traps: []Trap{{File: "store/store.go", Why: "unchanged, correct"}}}
	reps := []schema.Report{{Agent: "code-reviewer", Claims: []schema.Claim{
		claim("F1", "defect", "major", "store/ttl.go", 34, 35, "Get reads the maps without the lock: data race"),
		claim("F2", "defect", "minor", "store/ttl.go", 35, 36, "Concurrent map access without locking"), // duplicate of race
		claim("F3", "defect", "major", "store/ttl.go", 63, 64, "janitor goroutine never stops"),
		claim("F4", "performance", "minor", "store/store.go", 10, 12, "RWMutex is overkill"), // trap
		claim("F5", "defect", "minor", "store/other.go", 1, 1, "unrelated"),
	}}, {Agent: "alignment-analyst", Claims: []schema.Claim{claim("A1", "alignment", "info", "store/ttl.go", 1, 1, "x")}}}
	risk := 30
	r := Score(c, Observed{Claims: ClaimsFrom(reps), Alignment: "exact", Risk: &risk, HighRisk: 60})
	if r.TruePositives != 2 || r.Expected != 3 || r.Claims != 5 || r.MatchedClaims != 3 {
		t.Fatalf("result = %+v", r)
	}
	if *r.Precision != 0.6 || *r.Recall != 0.667 || r.F1 == nil {
		t.Errorf("p/r = %v %v", *r.Precision, *r.Recall)
	}
	if len(r.TrapHits) != 1 || r.TrapHits[0] != "code-reviewer:F4" || len(r.Missed) != 1 || r.Missed[0] != "magic" {
		t.Errorf("traps/missed = %v %v", r.TrapHits, r.Missed)
	}
	if !*r.AlignmentOK || !*r.RiskOK || r.Matches[0].SevAgrees == nil || !*r.Matches[0].SevAgrees {
		t.Errorf("checks = %+v", r)
	}
	if d := r.ByCategory["defect"]; d.Expected != 2 || d.Found != 2 || d.Claims != 4 || d.Matched != 3 {
		t.Errorf("defect = %+v", d)
	}
	if s := r.ByCategory["style"]; s.Expected != 1 || s.Found != 0 {
		t.Errorf("style = %+v", s)
	}

	s := Summarize([]CaseResult{r, {Name: "broken", Error: "boom"}})
	if s.Cases != 2 || s.Errors != 1 || *s.Precision != 0.6 || s.TrapHits != 1 || *s.AlignmentAcc != 1 {
		t.Errorf("summary = %+v", s)
	}
	base := s
	better := 0.9
	base.Recall = &better
	if reg := Regression(s, base, 0.05); len(reg) != 1 || !strings.Contains(reg[0], "recall") {
		t.Errorf("regression = %v", reg)
	}
	if reg := Regression(s, s, 0.05); len(reg) != 0 {
		t.Errorf("self-regression = %v", reg)
	}
}

// TestRepositoryCases keeps the shipped golden cases loadable.
func TestRepositoryCases(t *testing.T) {
	cs, err := LoadCases(filepath.Join("..", "..", "evaluation", "golden"))
	if err != nil || len(cs) < 3 {
		t.Fatalf("evaluation/golden: %d cases, %v", len(cs), err)
	}
	for _, c := range cs {
		if _, err := forge.HostConfigFromEnv().ParsePRURL(c.URL); err != nil {
			t.Errorf("%s: %v", c.Name, err)
		}
	}
}

func TestLoadCases(t *testing.T) {
	dir := t.TempDir()
	write := func(n, s string) { _ = os.WriteFile(filepath.Join(dir, n), []byte(s), 0o600) }
	write("b.yaml", "name: b\nurl: https://github.com/o/r/pull/2\nfindings:\n  - {file: a.go, lines: [1, 2]}\n")
	write("a.yml", "name: a\nurl: https://github.com/o/r/pull/1\nrisk: high\n")
	write("notes.txt", "ignored")
	cs, err := LoadCases(dir)
	if err != nil || len(cs) != 2 || cs[0].Name != "a" || cs[1].Findings[0].File != "a.go" {
		t.Fatalf("cases = %+v, %v", cs, err)
	}
	write("c.yaml", "name: c\nurl: u\nfindings:\n  - {file: a.go, lines: [5, 2]}\n")
	if _, err := LoadCases(dir); err == nil || !strings.Contains(err.Error(), "c.yaml") {
		t.Errorf("bad lines accepted: %v", err)
	}
}

// writeRun lays out a run directory as the MCP server does.
func writeRun(t *testing.T, runDir string) {
	t.Helper()
	put := func(rel string, v any) {
		p := filepath.Join(runDir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o750)
		b, _ := json.Marshal(v)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	put("checkout.json", map[string]string{"head_sha": "abc"})
	put("review/abc/code-reviewer.json", schema.Report{Agent: "code-reviewer", Claims: []schema.Claim{
		claim("F1", "defect", "major", "svc/svc.go", 4, 5, "retry loop has no backoff")}})
	put("review/abc/code-reviewer.dropped.json", []any{})
	put("augmentation/abc/alignment-analyst.json", schema.Report{Agent: "alignment-analyst", Facets: map[string]any{"alignment_category": "tangling"}})
	put("metrics.json", map[string]map[string]any{"repo_metrics": {"pr_risk": 72}})
}

func TestObserveAndRun(t *testing.T) {
	work := t.TempDir()
	c := Case{Name: "retry", URL: "https://github.com/o/r/pull/3", Alignment: "tangling", Risk: "high",
		Findings: []Expected{{ID: "backoff", File: "svc/svc.go", Lines: []int{4, 5}, Match: "backoff"}}}
	o := Options{PluginDir: "/plugin", WorkRoot: work, Hosts: forge.HostConfigFromEnv(), MaxCostUSD: 1,
		Exec: func(_ context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
			if name != "claude" || !strings.Contains(strings.Join(env, " "), "AIREVIEWER_EVAL=1") || args[len(args)-1] != "/aireviewer:pr-review "+c.URL+" --eval" {
				t.Errorf("exec %s %v %v", name, env, args)
			}
			writeRun(t, filepath.Join(dir, ".aireviewer", "runs", "github-github.com-o-r-pr-3"))
			return []byte(`{"type":"result","total_cost_usd":0.75,"duration_ms":90000,"num_turns":12,"is_error":false,
				"usage":{"input_tokens":1000,"output_tokens":800,"cache_read_input_tokens":5000,"cache_creation_input_tokens":200}}`), nil
		}}
	rs := Run(context.Background(), []Case{c, c, {Name: "bad name!", URL: c.URL}, c}, o)
	if len(rs) != 4 || rs[0].Error != "" || rs[0].TruePositives != 1 || !*rs[0].AlignmentOK || !*rs[0].RiskOK {
		t.Fatalf("results = %+v", rs)
	}
	if rs[0].CostUSD != 0.75 || rs[0].InputTokens != 6200 || rs[0].OutputTokens != 800 || rs[0].DurationMS != 90000 {
		t.Errorf("usage = %+v", rs[0])
	}
	if rs[1].Error != "" {
		t.Errorf("second case should run (0.75 < 1): %s", rs[1].Error)
	}
	if !strings.Contains(rs[2].Error, "case name") {
		t.Errorf("unsafe name: %+v", rs[2])
	}
	if !strings.Contains(rs[3].Error, "cost budget") {
		t.Errorf("budget not enforced after $1.50: %+v", rs[3])
	}

	// Re-scoring needs no model.
	again := ScoreExisting([]Case{c}, o)
	if len(again) != 1 || again[0].TruePositives != 1 || again[0].CostUSD != 0 {
		t.Errorf("rescore = %+v", again)
	}
}

func TestBias(t *testing.T) {
	root := t.TempDir()
	write := func(run, lines string) {
		_ = os.MkdirAll(filepath.Join(root, run), 0o750)
		_ = os.WriteFile(filepath.Join(root, run, "decisions.jsonl"), []byte(lines), 0o600)
	}
	// Run a: every comment from a finding, unedited, nothing disputed.
	write("a", `{"seq":1,"event":"comment_drafted","data":{"id":"C1","from_finding":"code-reviewer:F1"}}
{"seq":2,"event":"proposal_created","data":{"id":"P1","kind":"review","comments":["C1"]}}
{"seq":3,"event":"posted","data":{"id":"P1","kind":"review"}}
`)
	// Run b: the comment was edited before posting.
	write("b", `{"seq":1,"event":"comment_drafted","data":{"id":"C1","from_finding":"code-reviewer:F1"}}
{"seq":2,"event":"comment_edited","data":{"id":"C1"}}
{"seq":3,"event":"proposal_created","data":{"id":"P1","kind":"review","comments":["C1"]}}
{"seq":4,"event":"posted","data":{"id":"P1","kind":"review"}}
`)
	// Run c: a human-written comment alongside.
	write("c", `{"seq":1,"event":"comment_drafted","data":{"id":"C1","from_finding":"code-reviewer:F1"}}
{"seq":2,"event":"comment_drafted","data":{"id":"C2","from_finding":""}}
{"seq":3,"event":"proposal_created","data":{"id":"P1","kind":"review","comments":["C1","C2"]}}
{"seq":4,"event":"posted","data":{"id":"P1","kind":"review"}}
`)
	cs, err := Bias(root)
	if err != nil || len(cs) != 1 || cs[0].Run != "a" || cs[0].Findings[0] != "code-reviewer:F1" {
		t.Fatalf("bias = %+v, %v", cs, err)
	}
}

func TestParseClaudeJSON(t *testing.T) {
	u, err := ParseClaudeJSON([]byte("warning: something\n{\"total_cost_usd\":1.5,\"is_error\":true,\"result\":\"oops\",\"usage\":{\"input_tokens\":3,\"output_tokens\":4}}\n"))
	if err != nil || u.CostUSD != 1.5 || !u.IsError || u.InputTokens != 3 || u.OutputTokens != 4 {
		t.Errorf("usage = %+v, %v", u, err)
	}
	if _, err := ParseClaudeJSON([]byte("not json")); err == nil {
		t.Error("garbage parsed")
	}
}
