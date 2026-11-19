// SPDX-License-Identifier: MIT

package diffmap

import (
	"strings"
	"testing"
)

var sample = []Line{
	{Op: " ", Old: 3, New: 3, Text: "func Retry(n int) int {"},
	{Op: "-", Old: 4, Text: "\treturn n"},
	{Op: "+", New: 4, Text: "\tfor i := 0; i < n; i++ {"},
	{Op: "+", New: 5, Text: "\t}"},
	{Op: "+", New: 6, Text: "\treturn n"},
	{Op: " ", Old: 5, New: 7, Text: "}"},
}

func TestRenderHighlighted(t *testing.T) {
	out := RenderHighlighted("unit:svc.go#Retry", sample, []Mark{
		{Label: "F1 minor/high — loop body is empty", Side: "new", Start: 4, End: 5},
		{Label: "F2 info/low — removed return", Side: "old", Start: 4, End: 4},
		{Label: "F3 major/medium — elsewhere", Side: "new", Start: 90, End: 91},
	})
	lines := strings.Split(out, "\n")
	if lines[0] != "```diff" || lines[1] != "@@ unit:svc.go#Retry @@" {
		t.Fatalf("header:\n%s", out)
	}
	if !strings.Contains(lines[2], "F3") || !strings.Contains(lines[2], "not in this view") {
		t.Errorf("missing out-of-view mark:\n%s", out)
	}
	want := []string{
		"     3     3 │ func Retry(n int) int {",
		"-    4       ▶     return n",
		"@@ ▲ F2 info/low — removed return @@",
		"+          4 ▶     for i := 0; i < n; i++ {",
		"+          5 ▶     }",
		"@@ ▲ F1 minor/high — loop body is empty @@",
		"+          6 │     return n",
		"     5     7 │ }",
	}
	for _, w := range want {
		if !strings.Contains(out, w+"\n") {
			t.Errorf("missing line %q in:\n%s", w, out)
		}
	}
	// every non-annotation line keeps its diff op as the first character
	for _, l := range lines[1 : len(lines)-2] {
		if !(strings.HasPrefix(l, "@@") || strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") || strings.HasPrefix(l, " ")) {
			t.Errorf("line lost its diff op: %q", l)
		}
	}
}

func TestExcerpt(t *testing.T) {
	got := Excerpt(sample, "new", 5, 5, 0)
	if len(got) != 1 || got[0].New != 5 {
		t.Errorf("new 5..5 = %+v", got)
	}
	got = Excerpt(sample, "new", 3, 4, 0)
	if len(got) != 3 || got[1].Op != "-" { // keeps the interleaved deletion
		t.Errorf("new 3..4 = %+v", got)
	}
	if Excerpt(sample, "new", 50, 60, 3) != nil {
		t.Error("out-of-range excerpt should be nil")
	}
	if got := Excerpt(sample, "old", 4, 4, 0); len(got) != 1 || got[0].Op != "-" {
		t.Errorf("old 4..4 = %+v", got)
	}
}

func TestFenceEscalates(t *testing.T) {
	out := RenderHighlighted("", []Line{{Op: "+", New: 1, Text: "s := \"```\""}}, nil)
	if !strings.HasPrefix(out, "````diff\n") || !strings.HasSuffix(out, "````\n") {
		t.Errorf("fence not escalated:\n%s", out)
	}
}

func TestFileLines(t *testing.T) {
	got := FileLines([]byte("a\nb\nc\n"), "old", 0, 9)
	if len(got) != 3 || got[2].Old != 3 || got[2].New != 0 || got[2].Text != "c" {
		t.Errorf("FileLines = %+v", got)
	}
}
