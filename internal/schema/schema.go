// SPDX-License-Identifier: MIT

// Package schema validates agent reports (SPEC.md §6): first against the
// JSON Schema, then semantically — every piece of evidence must point at
// something that really exists (a line range in a real file, a metric from a
// real tool, an artifact in the run context). Claims that fail are dropped
// and the reasons returned so the agent can correct and resubmit.
package schema

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/husobee/aireviewer/internal/diffmap"
)

//go:embed report.v1.json
var reportSchema []byte

// ReportSchemaJSON returns the raw v1 report schema.
func ReportSchemaJSON() []byte { return reportSchema }

var (
	resolveOnce sync.Once
	resolved    *jsonschema.Resolved
	resolveErr  error
)

func compiled() (*jsonschema.Resolved, error) {
	resolveOnce.Do(func() {
		var s jsonschema.Schema
		if resolveErr = json.Unmarshal(reportSchema, &s); resolveErr != nil {
			return
		}
		resolved, resolveErr = s.Resolve(nil)
	})
	return resolved, resolveErr
}

// Evidence is one piece of evidence (code, metric or trace).
type Evidence struct {
	Kind     string `json:"kind"`
	File     string `json:"file,omitempty"`
	Lines    []int  `json:"lines,omitempty"`
	Side     string `json:"side,omitempty"`
	Name     string `json:"name,omitempty"`
	Value    any    `json:"value,omitempty"`
	Source   string `json:"source,omitempty"`
	Artifact string `json:"artifact,omitempty"`
}

// Claim is one finding.
type Claim struct {
	ID               string     `json:"id"`
	Statement        string     `json:"statement"`
	Category         string     `json:"category"`
	Severity         string     `json:"severity"`
	Confidence       string     `json:"confidence"`
	ConfidenceReason string     `json:"confidence_reason,omitempty"`
	Evidence         []Evidence `json:"evidence"`
	Anchors          []string   `json:"anchors,omitempty"`
	Proposal         string     `json:"proposal,omitempty"`
	Sources          []string   `json:"sources,omitempty"`
}

// Contradiction notes tension between claims.
type Contradiction struct {
	With string `json:"with"`
	Note string `json:"note"`
}

// Report is the common agent report envelope.
type Report struct {
	Agent          string          `json:"agent"`
	Version        int             `json:"version"`
	HeadSHA        string          `json:"head_sha"`
	Status         string          `json:"status"`
	Summary        string          `json:"summary"`
	Claims         []Claim         `json:"claims"`
	Contradictions []Contradiction `json:"contradictions,omitempty"`
}

// Dropped records a claim removed during validation.
type Dropped struct {
	ClaimID string   `json:"claim_id"`
	Reasons []string `json:"reasons"`
}

// Result is the outcome of validating a report.
type Result struct {
	Report  *Report   `json:"report"`
	Dropped []Dropped `json:"dropped"`
	Notes   []string  `json:"notes"`
}

// Env provides the facts evidence is checked against.
type Env struct {
	HeadSHA string
	Map     *diffmap.Map
	// LineCount returns the number of lines of path on a side ("new" = head,
	// "old" = merge base), or an error if the file does not exist there.
	LineCount func(side, path string) (int, error)
	// ArtifactExists reports whether a run-context relative path exists.
	ArtifactExists func(path string) bool
	// Metric returns the value a tool recorded for (source, name), if any.
	// Metric evidence is accepted only when its value equals the recorded one.
	Metric func(source, name string) (any, bool)
	// UpstreamClaims is the set of claim ids that synthesising agents may
	// cite in "sources".
	UpstreamClaims map[string]bool
}

// ErrInvalid wraps schema-level failures (nothing is accepted).
var ErrInvalid = errors.New("report rejected")

// Validate checks raw report JSON against the schema and env.
func Validate(raw []byte, env Env) (*Result, error) {
	rs, err := compiled()
	if err != nil {
		return nil, fmt.Errorf("load schema: %w", err)
	}
	var inst any
	if err := json.Unmarshal(raw, &inst); err != nil {
		return nil, fmt.Errorf("%w: not valid JSON: %v", ErrInvalid, err)
	}
	if err := rs.Validate(inst); err != nil {
		return nil, fmt.Errorf("%w: schema: %v", ErrInvalid, err)
	}
	var r Report
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if env.HeadSHA != "" && !shaMatch(r.HeadSHA, env.HeadSHA) {
		return nil, fmt.Errorf("%w: head_sha %q does not match the current PR head %q (report is stale)", ErrInvalid, r.HeadSHA, env.HeadSHA)
	}
	seen := map[string]bool{}
	for _, c := range r.Claims {
		if seen[c.ID] {
			return nil, fmt.Errorf("%w: duplicate claim id %q", ErrInvalid, c.ID)
		}
		seen[c.ID] = true
	}

	res := &Result{Dropped: []Dropped{}, Notes: []string{}}
	kept := make([]Claim, 0, len(r.Claims))
	for _, c := range r.Claims {
		var reasons []string
		for i, e := range c.Evidence {
			if why := checkEvidence(e, env); why != "" {
				reasons = append(reasons, fmt.Sprintf("evidence[%d]: %s", i, why))
			}
		}
		if len(reasons) > 0 {
			res.Dropped = append(res.Dropped, Dropped{ClaimID: c.ID, Reasons: reasons})
			continue
		}
		if env.Map != nil {
			anchors := c.Anchors[:0:0]
			for _, a := range c.Anchors {
				if _, ok := env.Map.Unit(a); ok {
					anchors = append(anchors, a)
				} else {
					res.Notes = append(res.Notes, fmt.Sprintf("claim %s: removed unknown anchor %q", c.ID, a))
				}
			}
			c.Anchors = anchors
		}
		if len(c.Sources) > 0 {
			srcs := c.Sources[:0:0]
			for _, s := range c.Sources {
				if env.UpstreamClaims[s] {
					srcs = append(srcs, s)
				} else {
					res.Notes = append(res.Notes, fmt.Sprintf("claim %s: removed unknown source claim %q", c.ID, s))
				}
			}
			c.Sources = srcs
		}
		kept = append(kept, c)
	}
	r.Claims = kept
	res.Report = &r
	return res, nil
}

// sameJSON compares two values by their JSON encoding after normalising
// numbers (so int 3 and float64 3 are equal).
func sameJSON(a, b any) bool {
	norm := func(v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprint(v)
		}
		var x any
		if json.Unmarshal(raw, &x) != nil {
			return string(raw)
		}
		out, _ := json.Marshal(x)
		return string(out)
	}
	return norm(a) == norm(b)
}

func shaMatch(a, b string) bool {
	if len(a) < 7 || len(b) < 7 {
		return false
	}
	return strings.HasPrefix(a, b) || strings.HasPrefix(b, a)
}

func checkEvidence(e Evidence, env Env) string {
	switch e.Kind {
	case "code":
		side := e.Side
		if side == "" {
			side = "new"
		}
		if len(e.Lines) != 2 || e.Lines[0] < 1 || e.Lines[1] < e.Lines[0] {
			return fmt.Sprintf("invalid line range %v (need [start,end], 1-based, start<=end)", e.Lines)
		}
		if env.LineCount == nil {
			return ""
		}
		n, err := env.LineCount(side, e.File)
		if err != nil {
			return fmt.Sprintf("file %q does not exist on the %s side", e.File, side)
		}
		if e.Lines[1] > n {
			return fmt.Sprintf("lines %v exceed %q length (%d lines on the %s side)", e.Lines, e.File, n, side)
		}
	case "metric":
		if env.Metric == nil {
			return fmt.Sprintf("metric %s:%s cannot be verified (no metrics recorded)", e.Source, e.Name)
		}
		want, ok := env.Metric(e.Source, e.Name)
		if !ok {
			return fmt.Sprintf("metric %q was not recorded by tool %q", e.Name, e.Source)
		}
		if !sameJSON(want, e.Value) {
			return fmt.Sprintf("metric %s:%s value %v does not match the recorded value %v", e.Source, e.Name, e.Value, want)
		}
	case "trace":
		if !strings.HasPrefix(e.Artifact, "runtime/") {
			return fmt.Sprintf("trace artifact %q must be a runtime/ execution artifact", e.Artifact)
		}
		if env.ArtifactExists == nil || !env.ArtifactExists(e.Artifact) {
			return fmt.Sprintf("trace artifact %q not found in the run context", e.Artifact)
		}
	default:
		return fmt.Sprintf("unknown evidence kind %q", e.Kind)
	}
	return ""
}
