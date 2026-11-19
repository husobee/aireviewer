// SPDX-License-Identifier: MIT

package schema

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/husobee/aireviewer/internal/diffmap"
)

func env() Env {
	return Env{
		HeadSHA: "abcdef1234567",
		Map:     &diffmap.Map{Units: []diffmap.Unit{{ID: "unit:a.go#F"}}},
		LineCount: func(side, path string) (int, error) {
			if path == "a.go" {
				if side == "old" {
					return 10, nil
				}
				return 20, nil
			}
			return 0, fmt.Errorf("missing")
		},
		ArtifactExists: func(p string) bool { return p == "runtime/run-1.log" || p == "context.json" },
		Metric: func(source, name string) (any, bool) {
			if source == "diffmap" && name == "units" {
				return 3, true
			}
			return nil, false
		},
		IssueText: func(ref string) (string, bool) {
			if ref == "o/r#3" {
				return "Retries hammer upstream\nAcceptance criteria:\n- use exponential   backoff\n- cap at 5 attempts", true
			}
			return "", false
		},
		UpstreamClaims: map[string]bool{"code-reviewer:F1": true},
	}
}

const tmpl = `{"agent":"code-reviewer","version":1,"head_sha":"abcdef1","status":"ok","summary":"s","claims":[%s]}`

func claim(id, evidence string, extra string) string {
	return fmt.Sprintf(`{"id":%q,"statement":"x","category":"defect","severity":"major","confidence":"high","evidence":[%s]%s}`, id, evidence, extra)
}

func TestValidateKeepsAndDrops(t *testing.T) {
	raw := fmt.Sprintf(tmpl,
		claim("ok", `{"kind":"code","file":"a.go","lines":[1,20]}`, `,"anchors":["unit:a.go#F","unit:nope"],"sources":["code-reviewer:F1","ghost"]`)+","+
			claim("old-side", `{"kind":"code","file":"a.go","lines":[5,10],"side":"old"}`, "")+","+
			claim("past-eof", `{"kind":"code","file":"a.go","lines":[15,21]}`, "")+","+
			claim("old-past-eof", `{"kind":"code","file":"a.go","lines":[5,11],"side":"old"}`, "")+","+
			claim("ghost-file", `{"kind":"code","file":"ghost.go","lines":[1,1]}`, "")+","+
			claim("bad-metric", `{"kind":"metric","name":"churn","value":3,"source":"made_up"}`, "")+","+
			claim("good-metric", `{"kind":"metric","name":"units","value":3,"source":"diffmap"}`, "")+","+
			claim("wrong-value", `{"kind":"metric","name":"units","value":4,"source":"diffmap"}`, "")+","+
			claim("unrecorded", `{"kind":"metric","name":"risk","value":90,"source":"diffmap"}`, "")+","+
			claim("non-runtime-trace", `{"kind":"trace","artifact":"context.json"}`, "")+","+
			claim("trace", `{"kind":"trace","artifact":"runtime/run-1.log"}`, "")+","+
			claim("issue-ok", `{"kind":"issue","ref":"o/r#3","quote":"Use exponential backoff"}`, "")+","+
			claim("issue-fabricated", `{"kind":"issue","ref":"o/r#3","quote":"must support jitter"}`, "")+","+
			claim("issue-unlinked", `{"kind":"issue","ref":"o/r#99","quote":"cap at 5 attempts"}`, "")+","+
			claim("bad-trace", `{"kind":"trace","artifact":"runtime/nope.log"}`, ""))
	res, err := Validate([]byte(raw), env())
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, c := range res.Report.Claims {
		kept = append(kept, c.ID)
	}
	if fmt.Sprint(kept) != "[ok old-side good-metric trace issue-ok]" {
		t.Errorf("kept = %v; dropped = %+v", kept, res.Dropped)
	}
	if len(res.Dropped) != 10 {
		t.Errorf("dropped = %+v", res.Dropped)
	}
	ok := res.Report.Claims[0]
	if fmt.Sprint(ok.Anchors) != "[unit:a.go#F]" || fmt.Sprint(ok.Sources) != "[code-reviewer:F1]" {
		t.Errorf("anchors/sources not filtered: %+v", ok)
	}
	if len(res.Notes) != 2 {
		t.Errorf("notes = %v", res.Notes)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]string{
		"not json":       `{`,
		"schema":         `{"agent":"x"}`,
		"bad enum":       fmt.Sprintf(tmpl, `{"id":"a","statement":"x","category":"vibes","severity":"major","confidence":"high","evidence":[{"kind":"code","file":"a.go","lines":[1,1]}]}`),
		"no evidence":    fmt.Sprintf(tmpl, `{"id":"a","statement":"x","category":"defect","severity":"major","confidence":"high","evidence":[]}`),
		"duplicate ids":  fmt.Sprintf(tmpl, claim("a", `{"kind":"code","file":"a.go","lines":[1,1]}`, "")+","+claim("a", `{"kind":"code","file":"a.go","lines":[1,1]}`, "")),
		"stale head":     `{"agent":"x","version":1,"head_sha":"9999999","status":"ok","summary":"s","claims":[]}`,
		"extra property": `{"agent":"x","version":1,"head_sha":"abcdef1","status":"ok","summary":"s","claims":[],"verdict":"approve"}`,
		"short quote":    fmt.Sprintf(tmpl, claim("q", `{"kind":"issue","ref":"o/r#3","quote":"cap"}`, "")),
	}
	for name, raw := range cases {
		if _, err := Validate([]byte(raw), env()); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
}

// The skill-facing copy of the schema must stay identical to the embedded one.
func TestSchemaCopyInSync(t *testing.T) {
	b, err := os.ReadFile("../../skills/_shared/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, ReportSchemaJSON()) {
		t.Error("skills/_shared/schema.json differs from internal/schema/report.v1.json; copy one to the other")
	}
}
