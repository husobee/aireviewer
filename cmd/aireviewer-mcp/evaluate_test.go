// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/golden"
)

func TestEvaluateScoreAndBias(t *testing.T) {
	dir := t.TempDir()
	cases := filepath.Join(dir, "golden")
	work := filepath.Join(dir, "work")
	_ = os.MkdirAll(cases, 0o750)
	_ = os.WriteFile(filepath.Join(cases, "retry.yaml"), []byte(`name: retry
url: https://github.com/o/r/pull/3
findings:
  - {id: backoff, file: svc/svc.go, lines: [4, 5], category: defect}
  - {id: unused, file: svc/svc.go, lines: [9, 9]}
`), 0o600)
	run := filepath.Join(work, "retry", ".aireviewer", "runs", "github-github.com-o-r-pr-3")
	put := func(rel, s string) {
		p := filepath.Join(run, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o750)
		_ = os.WriteFile(p, []byte(s), 0o600)
	}
	put("checkout.json", `{"head_sha":"abc"}`)
	put("review/abc/code-reviewer.json", `{"agent":"code-reviewer","claims":[{"id":"F1","statement":"no backoff","category":"defect","severity":"major",
		"evidence":[{"kind":"code","file":"svc/svc.go","lines":[4,5]}]}]}`)
	put("decisions.jsonl", `{"seq":1,"event":"comment_drafted","data":{"id":"C1","from_finding":"code-reviewer:F1"}}
{"seq":2,"event":"proposal_created","data":{"id":"P1","kind":"review","comments":["C1"]}}
{"seq":3,"event":"posted","data":{"id":"P1","kind":"review"}}
`)
	hosts := forge.HostConfigFromEnv()
	out := filepath.Join(dir, "results", "r1.json")
	var buf bytes.Buffer
	if err := evaluate(context.Background(), hosts, dir, []string{"score", "-cases", cases, "-work", work, "-out", out}, &buf); err != nil {
		t.Fatal(err)
	}
	if s := buf.String(); !strings.Contains(s, "| retry | 1/2 | 100% | 50% |") || !strings.Contains(s, "missed: unused") {
		t.Errorf("report:\n%s", s)
	}
	var rep golden.Report
	b, _ := os.ReadFile(out)
	if json.Unmarshal(b, &rep) != nil || rep.Summary.Cases != 1 || *rep.Summary.Recall != 0.5 {
		t.Fatalf("stored report = %s", b)
	}

	// A better baseline makes the command fail.
	better := 1.0
	rep.Summary.Recall = &better
	b, _ = json.Marshal(rep)
	base := filepath.Join(dir, "baseline.json")
	_ = os.WriteFile(base, b, 0o600)
	buf.Reset()
	err := evaluate(context.Background(), hosts, dir, []string{"score", "-cases", cases, "-work", work, "-baseline", base}, &buf)
	if !errors.Is(err, errRegression) || !strings.Contains(buf.String(), "recall dropped from 1.000 to 0.500") {
		t.Errorf("regression: %v\n%s", err, buf.String())
	}

	buf.Reset()
	if err := evaluate(context.Background(), hosts, dir, []string{"bias", "-runs", filepath.Dir(run)}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "1 review(s)") || !strings.Contains(buf.String(), "code-reviewer:F1") {
		t.Errorf("bias:\n%s", buf.String())
	}
	if err := evaluate(context.Background(), hosts, dir, []string{"nope"}, &buf); err == nil {
		t.Error("unknown subcommand accepted")
	}
}
