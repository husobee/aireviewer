// SPDX-License-Identifier: MIT

// Package golden scores aireviewer runs against golden cases (SPEC §16, paper
// §5.3): finding precision and recall stratified by category (comment
// type), alignment accuracy and risk agreement, with cost per run. Scoring
// is deterministic; only producing the runs uses a model.
package golden

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/husobee/aireviewer/internal/schema"
)

// Expected is one ground-truth finding.
type Expected struct {
	ID          string `yaml:"id" json:"id"`
	File        string `yaml:"file" json:"file"`
	Lines       []int  `yaml:"lines" json:"lines" jsonschema:"[start, end] on the head"`
	Category    string `yaml:"category,omitempty" json:"category,omitempty"`
	Match       string `yaml:"match,omitempty" json:"match,omitempty" jsonschema:"optional regex the statement must match (case-insensitive)"`
	SeverityMin string `yaml:"severity_min,omitempty" json:"severity_min,omitempty"`
}

// Trap is a location where any finding is a known false positive.
type Trap struct {
	File  string `yaml:"file" json:"file"`
	Lines []int  `yaml:"lines,omitempty" json:"lines,omitempty"`
	Why   string `yaml:"why,omitempty" json:"why,omitempty"`
}

// Case is one golden PR.
type Case struct {
	Name      string     `yaml:"name" json:"name"`
	URL       string     `yaml:"url" json:"url"`
	Mode      string     `yaml:"mode,omitempty" json:"mode,omitempty" jsonschema:"review (default) or author"`
	Findings  []Expected `yaml:"findings" json:"findings"`
	Traps     []Trap     `yaml:"traps,omitempty" json:"traps,omitempty"`
	Alignment string     `yaml:"alignment,omitempty" json:"alignment,omitempty" jsonschema:"expected alignment_category"`
	Risk      string     `yaml:"risk,omitempty" json:"risk,omitempty" jsonschema:"high or low: whether the PR was later followed by fixes"`
	Notes     string     `yaml:"notes,omitempty" json:"notes,omitempty"`
}

// LoadCases reads every *.yaml/*.yml case in dir, sorted by name.
func LoadCases(dir string) ([]Case, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var cs []Case
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || (!strings.HasSuffix(n, ".yaml") && !strings.HasSuffix(n, ".yml")) {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, n)) //nolint:gosec // case files in the given dir
		if err != nil {
			return nil, err
		}
		var c Case
		if err := yaml.Unmarshal(b, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		if err := c.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", n, err)
		}
		cs = append(cs, c)
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].Name < cs[j].Name })
	return cs, nil
}

func (c Case) validate() error {
	if c.Name == "" || c.URL == "" {
		return errors.New("a case needs name and url")
	}
	for i, f := range c.Findings {
		if f.File == "" || len(f.Lines) != 2 || f.Lines[0] < 1 || f.Lines[1] < f.Lines[0] {
			return fmt.Errorf("finding %d needs file and lines [start, end]", i+1)
		}
		if f.Match != "" {
			if _, err := regexp.Compile("(?i)" + f.Match); err != nil {
				return fmt.Errorf("finding %d: bad match regex: %w", i+1, err)
			}
		}
	}
	if c.Risk != "" && c.Risk != "high" && c.Risk != "low" {
		return errors.New("risk must be high or low")
	}
	return nil
}

// Observed is what a run produced, read from its run directory.
type Observed struct {
	Claims    []Claim `json:"claims"`
	Alignment string  `json:"alignment,omitempty"`
	Risk      *int    `json:"risk,omitempty"`
	HighRisk  int     `json:"high_risk" jsonschema:"risk.high_risk threshold used"`
}

// Claim is a code-review finding with its code locations.
type Claim struct {
	Ref       string     `json:"ref" jsonschema:"<agent>:<id>"`
	Statement string     `json:"statement"`
	Category  string     `json:"category"`
	Severity  string     `json:"severity"`
	Locs      []Location `json:"locations"`
}

// Location is a head-side code range cited by a claim.
type Location struct {
	File  string `json:"file"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

// ClaimsFrom extracts code-review claims and their head-side locations.
func ClaimsFrom(reports []schema.Report) []Claim {
	var out []Claim
	for _, r := range reports {
		if !strings.HasPrefix(r.Agent, "code-reviewer") {
			continue
		}
		for _, c := range r.Claims {
			cl := Claim{Ref: r.Agent + ":" + c.ID, Statement: c.Statement, Category: c.Category, Severity: c.Severity}
			for _, e := range c.Evidence {
				if e.Kind == "code" && len(e.Lines) == 2 && (e.Side == "" || e.Side == "new") {
					cl.Locs = append(cl.Locs, Location{File: e.File, Start: e.Lines[0], End: e.Lines[1]})
				}
			}
			out = append(out, cl)
		}
	}
	return out
}

// Tolerance is how many lines a cited range may miss the expected range by.
const Tolerance = 3

func overlaps(l Location, file string, lines []int) bool {
	if l.File != file {
		return false
	}
	if len(lines) != 2 {
		return true
	}
	return l.Start <= lines[1]+Tolerance && l.End >= lines[0]-Tolerance
}

var sevRank = map[string]int{"info": 0, "minor": 1, "major": 2, "blocker": 3}

func (e Expected) matches(c Claim) bool {
	if e.Category != "" && !strings.EqualFold(e.Category, c.Category) {
		return false
	}
	if e.Match != "" && !regexp.MustCompile("(?i)"+e.Match).MatchString(c.Statement) {
		return false
	}
	for _, l := range c.Locs {
		if overlaps(l, e.File, e.Lines) {
			return true
		}
	}
	return false
}

// Match pairs one expected finding with the claim that found it.
type Match struct {
	Expected  string `json:"expected"`
	Claim     string `json:"claim,omitempty"`
	Severity  string `json:"severity,omitempty"`
	SevAgrees *bool  `json:"severity_ok,omitempty" jsonschema:"claim severity is at least severity_min"`
}

// CaseResult is the score of one case.
type CaseResult struct {
	Name          string         `json:"name"`
	Expected      int            `json:"expected"`
	Claims        int            `json:"claims"`
	TruePositives int            `json:"true_positives" jsonschema:"expected findings that were found"`
	MatchedClaims int            `json:"matched_claims" jsonschema:"claims that match an expected finding"`
	TrapHits      []string       `json:"trap_hits,omitempty" jsonschema:"claims at known false-positive locations"`
	Missed        []string       `json:"missed,omitempty"`
	Unmatched     []string       `json:"unmatched,omitempty" jsonschema:"claims matching no expected finding (false positives, or findings the golden set lacks)"`
	Matches       []Match        `json:"matches"`
	Precision     *float64       `json:"precision,omitempty" jsonschema:"lower bound: unlisted true findings count as false positives"`
	Recall        *float64       `json:"recall,omitempty"`
	F1            *float64       `json:"f1,omitempty"`
	AlignmentOK   *bool          `json:"alignment_ok,omitempty"`
	RiskOK        *bool          `json:"risk_ok,omitempty"`
	ByCategory    map[string]Cat `json:"by_category"`
	CostUSD       float64        `json:"cost_usd,omitempty"`
	InputTokens   int            `json:"input_tokens,omitempty"`
	OutputTokens  int            `json:"output_tokens,omitempty"`
	DurationMS    int64          `json:"duration_ms,omitempty"`
	Error         string         `json:"error,omitempty"`
}

// Cat is a per-category tally (paper §5.3.1: stratify by comment type).
type Cat struct {
	Expected int `json:"expected"`
	Found    int `json:"found" jsonschema:"expected findings of this category that were found"`
	Claims   int `json:"claims" jsonschema:"claims of this category"`
	Matched  int `json:"matched" jsonschema:"claims of this category matching an expected finding"`
}

func ratio(n, d int) *float64 {
	if d == 0 {
		return nil
	}
	r := math.Round(float64(n)/float64(d)*1000) / 1000
	return &r
}

func boolp(b bool) *bool { return &b }

// Score compares a run's output with a case.
func Score(c Case, o Observed) CaseResult {
	r := CaseResult{Name: c.Name, Expected: len(c.Findings), Claims: len(o.Claims), ByCategory: map[string]Cat{}, Matches: []Match{}}
	used := map[int]bool{}
	matchedClaim := map[int]bool{}
	for _, e := range c.Findings {
		cat := r.ByCategory[catKey(e.Category)]
		cat.Expected++
		m := Match{Expected: e.ID}
		if m.Expected == "" {
			m.Expected = fmt.Sprintf("%s:%d-%d", e.File, e.Lines[0], e.Lines[1])
		}
		for i, cl := range o.Claims {
			if !used[i] && e.matches(cl) {
				used[i], matchedClaim[i] = true, true
				m.Claim, m.Severity = cl.Ref, cl.Severity
				if e.SeverityMin != "" {
					m.SevAgrees = boolp(sevRank[cl.Severity] >= sevRank[e.SeverityMin])
				}
				r.TruePositives++
				cat.Found++
				break
			}
		}
		if m.Claim == "" {
			r.Missed = append(r.Missed, m.Expected)
		}
		r.ByCategory[catKey(e.Category)] = cat
		r.Matches = append(r.Matches, m)
	}
	// A claim also counts as matched when it hits an expected finding that
	// another claim already took (duplicates are not false positives).
	for i, cl := range o.Claims {
		if !matchedClaim[i] {
			for _, e := range c.Findings {
				if e.matches(cl) {
					matchedClaim[i] = true
					break
				}
			}
		}
		cat := r.ByCategory[catKey(cl.Category)]
		cat.Claims++
		if matchedClaim[i] {
			r.MatchedClaims++
			cat.Matched++
		} else {
			r.Unmatched = append(r.Unmatched, cl.Ref)
			for _, t := range c.Traps {
				for _, l := range cl.Locs {
					if overlaps(l, t.File, t.Lines) {
						r.TrapHits = append(r.TrapHits, cl.Ref)
						goto next
					}
				}
			}
		}
	next:
		r.ByCategory[catKey(cl.Category)] = cat
	}
	r.Precision = ratio(r.MatchedClaims, r.Claims)
	r.Recall = ratio(r.TruePositives, r.Expected)
	if r.Precision != nil && r.Recall != nil && *r.Precision+*r.Recall > 0 {
		f := math.Round(2**r.Precision**r.Recall/(*r.Precision+*r.Recall)*1000) / 1000
		r.F1 = &f
	}
	if c.Alignment != "" {
		r.AlignmentOK = boolp(strings.EqualFold(c.Alignment, o.Alignment))
	}
	if c.Risk != "" && o.Risk != nil {
		r.RiskOK = boolp((*o.Risk >= o.HighRisk) == (c.Risk == "high"))
	}
	return r
}

func catKey(c string) string {
	if c == "" {
		return "any"
	}
	return strings.ToLower(c)
}

// Summary aggregates case results (micro-averaged over findings).
type Summary struct {
	Cases        int            `json:"cases"`
	Errors       int            `json:"errors"`
	Precision    *float64       `json:"precision,omitempty"`
	Recall       *float64       `json:"recall,omitempty"`
	F1           *float64       `json:"f1,omitempty"`
	TrapHits     int            `json:"trap_hits"`
	AlignmentAcc *float64       `json:"alignment_accuracy,omitempty"`
	RiskAcc      *float64       `json:"risk_accuracy,omitempty"`
	ByCategory   map[string]Cat `json:"by_category"`
	CostUSD      float64        `json:"cost_usd"`
	InputTokens  int            `json:"input_tokens"`
	OutputTokens int            `json:"output_tokens"`
	DurationMS   int64          `json:"duration_ms"`
}

// Summarize micro-averages results.
func Summarize(rs []CaseResult) Summary {
	s := Summary{ByCategory: map[string]Cat{}}
	var tp, exp, mc, cl, alOK, al, rkOK, rk int
	for _, r := range rs {
		s.Cases++
		if r.Error != "" {
			s.Errors++
			continue
		}
		tp, exp, mc, cl = tp+r.TruePositives, exp+r.Expected, mc+r.MatchedClaims, cl+r.Claims
		s.TrapHits += len(r.TrapHits)
		if r.AlignmentOK != nil {
			al++
			if *r.AlignmentOK {
				alOK++
			}
		}
		if r.RiskOK != nil {
			rk++
			if *r.RiskOK {
				rkOK++
			}
		}
		for k, v := range r.ByCategory {
			c := s.ByCategory[k]
			c.Expected, c.Found, c.Claims, c.Matched = c.Expected+v.Expected, c.Found+v.Found, c.Claims+v.Claims, c.Matched+v.Matched
			s.ByCategory[k] = c
		}
		s.CostUSD += r.CostUSD
		s.InputTokens += r.InputTokens
		s.OutputTokens += r.OutputTokens
		s.DurationMS += r.DurationMS
	}
	s.Precision, s.Recall = ratio(mc, cl), ratio(tp, exp)
	if s.Precision != nil && s.Recall != nil && *s.Precision+*s.Recall > 0 {
		f := math.Round(2**s.Precision**s.Recall/(*s.Precision+*s.Recall)*1000) / 1000
		s.F1 = &f
	}
	s.AlignmentAcc, s.RiskAcc = ratio(alOK, al), ratio(rkOK, rk)
	s.CostUSD = math.Round(s.CostUSD*10000) / 10000
	return s
}

// Regression compares a summary with a baseline; a drop of more than
// tolerance (absolute) in precision, recall or F1 is reported.
func Regression(cur, base Summary, tolerance float64) []string {
	var out []string
	check := func(name string, c, b *float64) {
		if c != nil && b != nil && *b-*c > tolerance {
			out = append(out, fmt.Sprintf("%s dropped from %.3f to %.3f", name, *b, *c))
		}
	}
	check("precision", cur.Precision, base.Precision)
	check("recall", cur.Recall, base.Recall)
	check("f1", cur.F1, base.F1)
	if cur.TrapHits > base.TrapHits {
		out = append(out, fmt.Sprintf("false-positive trap hits rose from %d to %d", base.TrapHits, cur.TrapHits))
	}
	return out
}
