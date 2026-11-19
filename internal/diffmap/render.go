// SPDX-License-Identifier: MIT

package diffmap

import (
	"fmt"
	"strings"
)

// Mark highlights a line range on one side of the diff and attaches a label
// (typically a finding: "F1 major/high — loop body is empty").
type Mark struct {
	Label string
	Side  string // "new" (head) or "old" (merge base); default "new"
	Start int
	End   int
}

func (m Mark) hits(l Line) bool {
	if m.Side == "old" {
		return l.Op != "+" && l.Old >= m.Start && l.Old <= m.End
	}
	return l.Op != "-" && l.New >= m.Start && l.New <= m.End
}

// Excerpt returns the contiguous run of lines spanning every line whose
// number on side lies within [start-ctx, end+ctx]. Interleaved lines from the
// other side (deletions when side is "new", additions when "old") are kept so
// the change still reads as a diff. It returns nil when no line is in range.
func Excerpt(lines []Line, side string, start, end, ctx int) []Line {
	lo, hi := start-ctx, end+ctx
	first, last := -1, -1
	for i, l := range lines {
		n := l.New
		if side == "old" {
			if l.Op == "+" {
				continue
			}
			n = l.Old
		} else if l.Op == "-" {
			continue
		}
		if n >= lo && n <= hi {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return nil
	}
	return lines[first : last+1]
}

// FileLines turns raw file content into context lines numbered on one side.
func FileLines(content []byte, side string, start, end int) []Line {
	all := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	if start < 1 {
		start = 1
	}
	if end > len(all) {
		end = len(all)
	}
	var out []Line
	for n := start; n <= end; n++ {
		l := Line{Op: " ", Text: all[n-1]}
		if side == "old" {
			l.Old = n
		} else {
			l.New = n
		}
		out = append(out, l)
	}
	return out
}

func fence(body string) string {
	run, best := 0, 0
	for _, r := range body {
		if r == '`' {
			run++
			if run > best {
				best = run
			}
		} else {
			run = 0
		}
	}
	n := 3
	if best >= n {
		n = best + 1
	}
	return strings.Repeat("`", n)
}

func num(n int) string {
	if n == 0 {
		return "     "
	}
	return fmt.Sprintf("%5d", n)
}

// RenderHighlighted renders lines as a fenced ```diff block suitable for a
// Markdown terminal: the first column keeps the diff op so +/- lines are
// coloured, cited lines get a ▶ gutter, and each mark's label is emitted as
// an "@@ ▲ …" line (coloured like a hunk header) right after its last cited
// line. Marks with no line in the excerpt are listed at the top.
func RenderHighlighted(header string, lines []Line, marks []Mark) string {
	last := make([]int, len(marks))
	for i := range last {
		last[i] = -1
	}
	for li, l := range lines {
		for mi, m := range marks {
			if m.hits(l) {
				last[mi] = li
			}
		}
	}
	var b strings.Builder
	if header != "" {
		b.WriteString("@@ " + header + " @@\n")
	}
	for mi, m := range marks {
		if last[mi] < 0 {
			fmt.Fprintf(&b, "@@ ▲ %s  (cited lines %s %d-%d not in this view) @@\n", m.Label, sideName(m.Side), m.Start, m.End)
		}
	}
	for li, l := range lines {
		gutter := "│"
		for _, m := range marks {
			if m.hits(l) {
				gutter = "▶"
				break
			}
		}
		op := l.Op
		if op == "" {
			op = " "
		}
		text := strings.ReplaceAll(l.Text, "\t", "    ")
		fmt.Fprintf(&b, "%s%s %s %s %s\n", op, num(l.Old), num(l.New), gutter, text)
		for mi, m := range marks {
			if last[mi] == li {
				b.WriteString("@@ ▲ " + m.Label + " @@\n")
			}
		}
	}
	body := b.String()
	f := fence(body)
	return f + "diff\n" + body + f + "\n"
}

func sideName(side string) string {
	if side == "old" {
		return "old"
	}
	return "new"
}
