// SPDX-License-Identifier: MIT

package sandbox

import (
	"bufio"
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Outcome of one attempt for one test or command.
const (
	Pass    = "pass"
	Fail    = "fail"
	Timeout = "timeout"
	Skip    = "skip"
)

// Classification across attempts (SPEC §10: never claim a failure before
// re-running it).
const (
	ClassPass        = "pass"
	ClassFail        = "deterministic_fail"
	ClassFlaky       = "flaky"
	ClassTimeout     = "timeout"
	ClassSkip        = "skip"
	ClassAbsent      = "absent"
	ClassSetupFailed = "setup_failed"
)

// Classify turns per-attempt outcomes into a classification.
func Classify(attempts []string) string {
	var pass, fail, to, skip int
	for _, a := range attempts {
		switch a {
		case Pass:
			pass++
		case Fail:
			fail++
		case Timeout:
			to++
		case Skip:
			skip++
		}
	}
	switch {
	case pass+fail+to+skip == 0:
		return ClassAbsent
	case pass > 0 && fail+to > 0:
		return ClassFlaky
	case pass > 0:
		return ClassPass
	case fail > 0:
		return ClassFail
	case to > 0:
		return ClassTimeout
	default:
		return ClassSkip
	}
}

// Delta compares one test's (or command's) classification on the merge base
// and on the head.
func Delta(base, head string) string {
	switch {
	case head == ClassSetupFailed || base == ClassSetupFailed:
		return "not_comparable"
	case head == ClassAbsent && base != ClassAbsent:
		return "removed"
	case head == ClassFlaky:
		return "flaky"
	case head == ClassTimeout:
		if base == ClassTimeout {
			return "still_timing_out"
		}
		return "new_timeout"
	case head == ClassFail:
		switch base {
		case ClassFail:
			return "still_failing"
		case ClassFlaky:
			return "fails_now_flaky_before"
		default:
			return "new_failure"
		}
	case head == ClassPass:
		switch base {
		case ClassFail, ClassTimeout:
			return "fixed"
		case ClassAbsent:
			return "new_passing"
		default:
			return "pass"
		}
	default:
		return "skip"
	}
}

type goEvent struct {
	Action  string `json:"Action"`
	Package string `json:"Package"`
	Test    string `json:"Test"`
}

// ParseGoTestJSON extracts per-test outcomes from `go test -json` output.
// Package-level failures without a failing test (build errors, panics in
// TestMain) are reported as "<pkg> [package]". ok is false when the output
// contains no test events.
func ParseGoTestJSON(out []byte) (map[string]string, bool) {
	res := map[string]string{}
	pkgFail := map[string]bool{}
	pkgHasFail := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	seen := false
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var e goEvent
		if json.Unmarshal(line, &e) != nil || e.Action == "" {
			continue
		}
		switch e.Action {
		case "pass", "fail", "skip":
		default:
			continue
		}
		seen = true
		if e.Test == "" {
			if e.Action == "fail" {
				pkgFail[e.Package] = true
			}
			continue
		}
		name := e.Package + "." + e.Test
		res[name] = e.Action
		if e.Action == "fail" {
			pkgHasFail[e.Package] = true
		}
	}
	for p := range pkgFail {
		if !pkgHasFail[p] {
			res[p+" [package]"] = Fail
		}
	}
	return res, seen
}

// Finding is one normalised static-analysis finding.
type Finding struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Column   int    `json:"column,omitempty"`
	Linter   string `json:"linter,omitempty"`
	Severity string `json:"severity,omitempty"`
	Message  string `json:"message"`
	InDiff   bool   `json:"in_diff" jsonschema:"the line is shown in the diff (changed or context)"`
	Changed  bool   `json:"changed" jsonschema:"the line was added or modified by the change"`
	Command  int    `json:"command" jsonschema:"index of the static_analysis command that reported it"`
}

type golangciOut struct {
	Issues []struct {
		FromLinter string `json:"FromLinter"`
		Text       string `json:"Text"`
		Severity   string `json:"Severity"`
		Pos        struct {
			Filename string `json:"Filename"`
			Line     int    `json:"Line"`
			Column   int    `json:"Column"`
		} `json:"Pos"`
	} `json:"Issues"`
}

var lintLine = regexp.MustCompile(`^(?:\./)?([^\s:][^:]*):(\d+)(?::(\d+))?:\s*(.+)$`)

// ParseLint normalises analyzer output: golangci-lint JSON, or the common
// "file:line[:col]: message" format used by go vet, staticcheck, eslint
// (unix formatter), ruff, flake8, shellcheck (gcc) and others.
func ParseLint(out []byte) []Finding {
	var fs []Finding
	// golangci-lint JSON: one document (possibly pretty-printed) that may be
	// preceded by log lines; decode from where it starts.
	if i := bytes.Index(out, []byte(`{"Issues"`)); i >= 0 {
		var g golangciOut
		if json.NewDecoder(bytes.NewReader(out[i:])).Decode(&g) == nil {
			for _, is := range g.Issues {
				fs = append(fs, Finding{File: cleanPath(is.Pos.Filename), Line: is.Pos.Line, Column: is.Pos.Column,
					Linter: is.FromLinter, Severity: is.Severity, Message: is.Text})
			}
			return dedupe(fs)
		}
	}
	for _, line := range strings.Split(string(out), "\n") {
		m := lintLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		c, _ := strconv.Atoi(m[3])
		if n <= 0 || strings.Contains(m[1], " ") {
			continue
		}
		fs = append(fs, Finding{File: cleanPath(m[1]), Line: n, Column: c, Message: strings.TrimSpace(m[4])})
	}
	return dedupe(fs)
}

func cleanPath(p string) string {
	p = strings.TrimPrefix(p, "/work/")
	return strings.TrimPrefix(p, "./")
}

func dedupe(fs []Finding) []Finding {
	seen := map[string]bool{}
	out := fs[:0]
	for _, f := range fs {
		k := f.File + ":" + strconv.Itoa(f.Line) + ":" + f.Message
		if seen[k] || f.File == "" {
			continue
		}
		seen[k] = true
		out = append(out, f)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out
}
