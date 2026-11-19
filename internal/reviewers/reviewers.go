// SPDX-License-Identifier: MIT

// Package reviewers implements the deterministic Reviewer Suggestion Tool
// (paper §4.4.1): candidates come from CODEOWNERS and from git history on the
// changed files, and are scored on expertise, experience, ownership, current
// workload and knowledge sharing with configurable weights. No LLM is used,
// as the paper prescribes for cost and reproducibility.
package reviewers

import (
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/husobee/aireviewer/internal/config"
	"github.com/husobee/aireviewer/internal/history"
)

// CodeownersPaths are checked in order (GitHub and GitLab locations).
var CodeownersPaths = []string{".github/CODEOWNERS", "CODEOWNERS", "docs/CODEOWNERS", ".gitlab/CODEOWNERS"}

// Rule is one CODEOWNERS line.
type Rule struct {
	Pattern string
	Owners  []string
	re      *regexp.Regexp
}

// ParseCodeowners parses CODEOWNERS content (comments, blank lines and GitLab
// [Section] headers are skipped).
var sectionRE = regexp.MustCompile(`^\^?\[[^\]]*\](?:\[\d+\])?\s*(.*)$`)

func ParseCodeowners(content string) []Rule {
	var rules []Rule
	var sectionOwners []string // GitLab: "[Section] @owner" default owners
	for _, line := range strings.Split(content, "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if m := sectionRE.FindStringSubmatch(line); m != nil {
			sectionOwners = strings.Fields(m[1])
			continue
		}
		f := strings.Fields(line)
		owners := f[1:]
		if len(owners) == 0 {
			owners = sectionOwners
		}
		if len(owners) == 0 {
			continue
		}
		re, err := globRE(f[0])
		if err != nil {
			continue
		}
		rules = append(rules, Rule{Pattern: f[0], Owners: owners, re: re})
	}
	return rules
}

func globRE(p string) (*regexp.Regexp, error) {
	anchored := strings.HasPrefix(p, "/") || strings.Contains(strings.TrimSuffix(p, "/"), "/")
	dirOnly := strings.HasSuffix(p, "/")
	p = strings.Trim(p, "/")
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		switch {
		case strings.HasPrefix(p[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(p[i:], "/**"):
			b.WriteString("(?:/.*)?")
			i += 2
		case strings.HasPrefix(p[i:], "**"):
			b.WriteString(".*")
			i++
		case p[i] == '*':
			b.WriteString("[^/]*")
		case p[i] == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(p[i])))
		}
	}
	prefix := "^"
	if !anchored {
		prefix = "^(?:.*/)?"
	}
	suffix := "(?:/.*)?$"
	if dirOnly {
		suffix = "/.*$"
	}
	return regexp.Compile(prefix + b.String() + suffix)
}

// Owners returns the owners of path (last matching rule wins).
func Owners(rules []Rule, path string) []string {
	var owners []string
	for _, r := range rules {
		if r.re.MatchString(path) {
			owners = r.Owners
		}
	}
	return owners
}

// Candidate is one ranked suggestion.
type Candidate struct {
	Identity string             `json:"identity" jsonschema:"forge username (@user or @org/team), or git:<email> when no username is known"`
	Name     string             `json:"name,omitempty"`
	Score    float64            `json:"score"`
	Factors  map[string]float64 `json:"factors" jsonschema:"each 0..1: expertise, experience, ownership, workload, knowledge_share"`
	Reasons  []string           `json:"reasons"`
	Mapped   bool               `json:"mapped" jsonschema:"true when Identity is a forge username that can be requested directly"`
	OpenLoad int                `json:"open_review_requests" jsonschema:"-1 when unknown"`
}

// Input collects everything the scorer needs.
type Input struct {
	ChangedFiles []string
	Authors      []history.Author
	EmailLogin   map[string]string // git email → forge login
	Rules        []Rule
	Workload     map[string]int // login → open review requests (missing = unknown)
	PRAuthor     string         // forge login
	Assigned     []string       // already-assigned reviewers (excluded)
	RiskScore    int
	Config       config.Config
}

// Mode is the weighting mode actually applied.
type Mode string

const (
	ExpertiseFirst Mode = "expertise_first" // high-risk change: knowledge-sharing weight dropped
	Balanced       Mode = "balanced"
)

// Suggest ranks candidates.
func Suggest(in Input) ([]Candidate, Mode) {
	w := in.Config.Reviewers.Weights
	mode := Balanced
	if in.RiskScore >= in.Config.Risk.HighRisk {
		mode = ExpertiseFirst
		w.KnowledgeShare = 0
	}
	excluded := func(id, name string) bool {
		l := strings.ToLower(id + " " + name)
		if strings.Contains(l, "[bot]") || strings.HasSuffix(strings.TrimPrefix(strings.ToLower(id), "@"), "bot") {
			return true
		}
		if in.PRAuthor != "" && strings.EqualFold(strings.TrimPrefix(id, "@"), in.PRAuthor) {
			return true
		}
		for _, a := range in.Assigned {
			if strings.EqualFold(strings.TrimPrefix(id, "@"), strings.TrimPrefix(a, "@")) {
				return true
			}
		}
		for _, x := range in.Config.Reviewers.Exclude {
			if x != "" && strings.Contains(l, strings.ToLower(x)) {
				return true
			}
		}
		return false
	}
	n := float64(len(in.ChangedFiles))
	if n == 0 {
		n = 1
	}
	type acc struct {
		c      Candidate
		exp    float64
		files  map[string]bool
		owned  int
		emails []string
	}
	by := map[string]*acc{}
	get := func(id, name string) *acc {
		a := by[id]
		if a == nil {
			a = &acc{c: Candidate{Identity: id, Name: name, OpenLoad: -1, Factors: map[string]float64{}}, files: map[string]bool{}}
			by[id] = a
		}
		return a
	}
	maxExp := 0.0
	// Known handles (mapped logins and CODEOWNERS users) let us merge an
	// unmapped git identity whose email local part equals a handle. This is a
	// heuristic and is labelled as such in the reasons.
	handles := map[string]string{}
	for _, l := range in.EmailLogin {
		handles[strings.ToLower(l)] = l
	}
	for _, r := range in.Rules {
		for _, o := range r.Owners {
			if strings.HasPrefix(o, "@") && !strings.Contains(o, "/") {
				handles[strings.ToLower(strings.TrimPrefix(o, "@"))] = strings.TrimPrefix(o, "@")
			}
		}
	}
	heuristic := map[string]bool{}
	for _, au := range in.Authors {
		id, mapped := "git:"+au.Email, false
		if login, ok := in.EmailLogin[au.Email]; ok && login != "" {
			id, mapped = "@"+login, true
		} else if local, _, ok := strings.Cut(au.Email, "@"); ok {
			if h, ok := handles[strings.ToLower(local)]; ok {
				id, mapped = "@"+h, true
				heuristic["@"+h] = true
			}
		}
		a := get(id, au.Name)
		a.c.Mapped = a.c.Mapped || mapped
		a.exp += au.Expertise
		a.emails = append(a.emails, au.Email)
		for _, f := range au.Files {
			a.files[f] = true
		}
	}
	for _, f := range in.ChangedFiles {
		for _, o := range Owners(in.Rules, f) {
			if !strings.HasPrefix(o, "@") {
				continue // email owners can't be requested by username
			}
			a := get(o, "")
			a.c.Mapped = true
			a.owned++
		}
	}
	// Normalise expertise over eligible candidates only, so excluded
	// identities (the PR author, bots) don't deflate everyone else.
	for id, a := range by {
		if !excluded(id, a.c.Name) {
			maxExp = math.Max(maxExp, a.exp)
		}
	}
	var out []Candidate
	for id, a := range by {
		if excluded(id, a.c.Name) {
			continue
		}
		f := a.c.Factors
		if maxExp > 0 {
			f["expertise"] = round2(a.exp / maxExp)
		} else {
			f["expertise"] = 0
		}
		f["experience"] = round2(float64(len(a.files)) / n)
		f["ownership"] = round2(float64(a.owned) / n)
		f["workload"] = 0.5
		if load, ok := in.Workload[strings.TrimPrefix(id, "@")]; ok {
			a.c.OpenLoad = load
			f["workload"] = round2(1 - math.Min(float64(load)/10, 1))
		}
		f["knowledge_share"] = 0
		if f["experience"] > 0 {
			f["knowledge_share"] = round2(1 - f["expertise"])
		}
		a.c.Score = round2(w.Expertise*f["expertise"] + w.Experience*f["experience"] + w.Ownership*f["ownership"] +
			w.Workload*f["workload"] + w.KnowledgeShare*f["knowledge_share"])
		if a.owned > 0 {
			a.c.Reasons = append(a.c.Reasons, "CODEOWNERS owner of changed files")
		}
		if len(a.files) > 0 {
			a.c.Reasons = append(a.c.Reasons, "changed these files recently")
		}
		if !a.c.Mapped {
			a.c.Reasons = append(a.c.Reasons, "no forge username known for this git identity; map it before requesting")
		}
		if heuristic[id] {
			a.c.Reasons = append(a.c.Reasons, "git identity matched to this username by email local part (heuristic; confirm before requesting)")
		}
		out = append(out, a.c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Identity < out[j].Identity
	})
	if max := in.Config.Reviewers.Max; max > 0 && len(out) > max {
		out = out[:max]
	}
	return out, mode
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }
