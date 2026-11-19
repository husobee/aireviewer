// SPDX-License-Identifier: MIT

package diffmap

import (
	"regexp"
	"strconv"
	"strings"
)

// Line is one line of a hunk with its line numbers on each side
// (0 when the line does not exist on that side).
type Line struct {
	Op   string `json:"op" jsonschema:"+ added, - deleted, space = context"`
	Old  int    `json:"old,omitempty"`
	New  int    `json:"new,omitempty"`
	Text string `json:"text"`
}

// Hunk is a parsed unified-diff hunk.
type Hunk struct {
	OldStart int    `json:"old_start"`
	NewStart int    `json:"new_start"`
	Header   string `json:"header"`
	Lines    []Line `json:"lines"`
}

// FileDiff is one file section of a git diff.
type FileDiff struct {
	OldPath string
	NewPath string
	Status  string // added | modified | deleted | renamed
	Binary  bool
	Hunks   []Hunk
}

// Path returns the path that identifies the file (new path unless deleted).
func (f *FileDiff) Path() string {
	if f.Status == StatusDeleted {
		return f.OldPath
	}
	return f.NewPath
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@(.*)$`)

func unquotePath(p string) string {
	if strings.HasPrefix(p, `"`) {
		if u, err := strconv.Unquote(p); err == nil {
			p = u
		}
	}
	return p
}

func stripPrefix(p string) string {
	p = unquotePath(strings.TrimSpace(p))
	if p == "/dev/null" {
		return ""
	}
	if len(p) > 2 && (p[:2] == "a/" || p[:2] == "b/") {
		return p[2:]
	}
	return p
}

// ParseGitDiff parses `git diff` output (any -U value, with or without -M).
func ParseGitDiff(out string) []*FileDiff {
	var files []*FileDiff
	var cur *FileDiff
	var hunk *Hunk
	oldNo, newNo := 0, 0

	flushHunk := func() {
		if cur != nil && hunk != nil {
			cur.Hunks = append(cur.Hunks, *hunk)
		}
		hunk = nil
	}
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if i == len(lines)-1 && l == "" {
			break
		}
		switch {
		case strings.HasPrefix(l, "diff --git "):
			flushHunk()
			cur = &FileDiff{Status: StatusModified}
			// Fallback paths from the header; refined by ---/+++/rename lines.
			rest := strings.TrimPrefix(l, "diff --git ")
			if j := strings.Index(rest, " b/"); j >= 0 && strings.HasPrefix(rest, "a/") {
				cur.OldPath, cur.NewPath = rest[2:j], rest[j+3:]
			}
			files = append(files, cur)
			continue
		case cur == nil:
			continue
		}
		if hunk == nil || !(strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") || strings.HasPrefix(l, " ") || strings.HasPrefix(l, `\`)) {
			switch {
			case strings.HasPrefix(l, "new file mode"):
				cur.Status = StatusAdded
				continue
			case strings.HasPrefix(l, "deleted file mode"):
				cur.Status = StatusDeleted
				continue
			case strings.HasPrefix(l, "rename from "):
				cur.OldPath, cur.Status = unquotePath(strings.TrimPrefix(l, "rename from ")), StatusRenamed
				continue
			case strings.HasPrefix(l, "rename to "):
				cur.NewPath, cur.Status = unquotePath(strings.TrimPrefix(l, "rename to ")), StatusRenamed
				continue
			case strings.HasPrefix(l, "Binary files "):
				cur.Binary = true
				continue
			case strings.HasPrefix(l, "--- ") && hunk == nil:
				if p := stripPrefix(l[4:]); p != "" {
					cur.OldPath = p
				}
				continue
			case strings.HasPrefix(l, "+++ ") && hunk == nil:
				if p := stripPrefix(l[4:]); p != "" {
					cur.NewPath = p
				}
				continue
			}
		}
		if m := hunkHeader.FindStringSubmatch(l); m != nil {
			flushHunk()
			oldNo, _ = strconv.Atoi(m[1])
			newNo, _ = strconv.Atoi(m[3])
			hunk = &Hunk{OldStart: oldNo, NewStart: newNo, Header: strings.TrimSpace(m[5])}
			continue
		}
		if hunk == nil || l == "" {
			continue
		}
		switch l[0] {
		case '+':
			hunk.Lines = append(hunk.Lines, Line{Op: "+", New: newNo, Text: l[1:]})
			newNo++
		case '-':
			hunk.Lines = append(hunk.Lines, Line{Op: "-", Old: oldNo, Text: l[1:]})
			oldNo++
		case ' ':
			hunk.Lines = append(hunk.Lines, Line{Op: " ", Old: oldNo, New: newNo, Text: l[1:]})
			oldNo++
			newNo++
		}
	}
	flushHunk()
	return files
}
