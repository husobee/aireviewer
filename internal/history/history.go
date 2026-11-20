// SPDX-License-Identifier: MIT

// Package history computes deterministic change-history signals from git:
// per-file change frequency, bug-fix history, author spread and hotspot rank
// (for the Bug Proneness Analysis Agent, paper §4.3.2), a reproducible risk
// score, and per-author expertise on the changed files (for reviewer
// suggestion, §4.4). Everything is anchored to the merge-base commit's time,
// so the same PR always yields the same numbers.
package history

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Runner runs git (e.g. *gitrepo.Git.Run with dir "").
type Runner func(ctx context.Context, args ...string) (string, error)

// Commit is one non-merge commit in the analysed window.
type Commit struct {
	SHA     string
	Time    time.Time
	Name    string
	Email   string
	Subject string
	Files   []string
}

// MaxCommits caps the history scanned (large monorepos).
const MaxCommits = 20000

// Load reads commits reachable from base within window before base's
// commit time. It needs only commits and trees (works on blobless mirrors).
func Load(ctx context.Context, run Runner, base string, window time.Duration) (now time.Time, commits []Commit, total int, err error) {
	ts, err := run(ctx, "show", "-s", "--format=%ct", base)
	if err != nil {
		return
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(ts), 10, 64)
	if err != nil {
		return now, nil, 0, fmt.Errorf("base commit time: %w", err)
	}
	now = time.Unix(sec, 0).UTC()
	cnt, err := run(ctx, "rev-list", "--count", "--max-count=100000", base)
	if err != nil {
		return
	}
	total, _ = strconv.Atoi(strings.TrimSpace(cnt))
	out, err := run(ctx, "-c", "core.quotePath=false", "log", "--no-merges", "--no-renames",
		"--since="+now.Add(-window).Format(time.RFC3339), "--until="+now.Add(time.Second).Format(time.RFC3339), "--max-count="+strconv.Itoa(MaxCommits),
		"--format=%x1e%H%x1f%ct%x1f%aN%x1f%aE%x1f%s", "--name-only", base)
	if err != nil {
		return
	}
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimSpace(rec)
		if rec == "" {
			continue
		}
		lines := strings.Split(rec, "\n")
		f := strings.SplitN(lines[0], "\x1f", 5)
		if len(f) < 5 {
			continue
		}
		t, _ := strconv.ParseInt(f[1], 10, 64)
		c := Commit{SHA: f[0], Time: time.Unix(t, 0).UTC(), Name: f[2], Email: strings.ToLower(f[3]), Subject: f[4]}
		for _, p := range lines[1:] {
			if p = strings.TrimSpace(p); p != "" {
				c.Files = append(c.Files, p)
			}
		}
		commits = append(commits, c)
	}
	return now, commits, total, nil
}

// FileStats are history signals for one path.
type FileStats struct {
	Path              string  `json:"path"`
	Commits30         int     `json:"commits_30d"`
	Commits90         int     `json:"commits_90d"`
	Commits365        int     `json:"commits_365d"`
	Bugfix365         int     `json:"bugfix_365d" jsonschema:"bug-fix commits: matched by keyword or by a bug-labelled PR"`
	BugfixKeyword365  int     `json:"bugfix_keyword_365d" jsonschema:"of which matched by commit-subject keyword"`
	BugfixLabel365    int     `json:"bugfix_label_365d" jsonschema:"of which belong to a merged PR/MR with a bug label"`
	Authors365        int     `json:"authors_365d"`
	LastChangeDays    int     `json:"last_change_days" jsonschema:"days before the merge base; -1 if none in window"`
	HotspotPercentile float64 `json:"hotspot_percentile" jsonschema:"share of repository files changed less often in the window (0..1)"`
}

// BugSources counts the window's bug-fix commits by how they were found.
func BugSources(commits []Commit, bug *regexp.Regexp, labelled map[string]bool) (keywordOnly, labelOnly, both int) {
	for _, c := range commits {
		kw, lb := bug.MatchString(c.Subject), labelled[c.SHA]
		switch {
		case kw && lb:
			both++
		case kw:
			keywordOnly++
		case lb:
			labelOnly++
		}
	}
	return
}

// BugfixRE builds the bug-fix subject matcher from keywords.
func BugfixRE(keywords []string) *regexp.Regexp {
	q := make([]string, 0, len(keywords))
	for _, k := range keywords {
		q = append(q, regexp.QuoteMeta(k))
	}
	return regexp.MustCompile(`(?i)\b(` + strings.Join(q, "|") + `)\b`)
}

// Stats computes FileStats for paths, plus the hotspot percentile against
// every file touched in the window. A commit is a bug fix when its subject
// matches bug or its SHA is in labelled (commits of bug-labelled PRs; may
// be nil).
func Stats(now time.Time, commits []Commit, paths []string, bug *regexp.Regexp, labelled map[string]bool) map[string]*FileStats {
	perFile := map[string]int{}
	for _, c := range commits {
		for _, f := range c.Files {
			perFile[f]++
		}
	}
	freqs := make([]int, 0, len(perFile))
	for _, n := range perFile {
		freqs = append(freqs, n)
	}
	sort.Ints(freqs)

	out := map[string]*FileStats{}
	authors := map[string]map[string]bool{}
	for _, p := range paths {
		out[p] = &FileStats{Path: p, LastChangeDays: -1}
		authors[p] = map[string]bool{}
	}
	for _, c := range commits {
		age := now.Sub(c.Time)
		byKW, byLabel := bug.MatchString(c.Subject), labelled[c.SHA]
		for _, f := range c.Files {
			s, ok := out[f]
			if !ok {
				continue
			}
			s.Commits365++
			if age <= 90*24*time.Hour {
				s.Commits90++
			}
			if age <= 30*24*time.Hour {
				s.Commits30++
			}
			if byKW || byLabel {
				s.Bugfix365++
			}
			if byKW {
				s.BugfixKeyword365++
			}
			if byLabel {
				s.BugfixLabel365++
			}
			authors[f][c.Email] = true
			if d := int(age.Hours() / 24); s.LastChangeDays < 0 || d < s.LastChangeDays {
				s.LastChangeDays = d
			}
		}
	}
	for p, s := range out {
		s.Authors365 = len(authors[p])
		if len(freqs) > 0 {
			below := sort.SearchInts(freqs, s.Commits365) // files changed strictly less often
			s.HotspotPercentile = math.Round(float64(below)/float64(len(freqs))*100) / 100
		}
	}
	return out
}

// FileRisk is the deterministic risk score of one changed file.
type FileRisk struct {
	Path       string             `json:"path"`
	Score      int                `json:"score" jsonschema:"0..100"`
	Components map[string]float64 `json:"components" jsonschema:"each 0..1: frequency, bug_ratio, ownership_spread, change_size"`
	Confidence string             `json:"confidence"`
}

// Risk is the PR-level result.
type Risk struct {
	Score            int        `json:"score" jsonschema:"0..100; 0.6*max file score + 0.4*churn-weighted mean"`
	Confidence       string     `json:"confidence"`
	ConfidenceReason string     `json:"confidence_reason"`
	Files            []FileRisk `json:"files"`
	Formula          string     `json:"formula"`
}

// Formula documents the score so reviewers can audit it.
const Formula = "file = 100*(0.35*hotspot_percentile + 0.30*min(bugfix_365d/commits_365d,1) + 0.15*min(authors_365d/5,1) + 0.20*min(changed_lines/200,1)); " +
	"pr = round(0.6*max(file) + 0.4*churn-weighted mean(file))"

// Score computes file and PR risk. changed maps path → changed lines in the PR.
func Score(stats map[string]*FileStats, changed map[string]int, repoCommits365, repoTotal, minHistory int) Risk {
	r := Risk{Formula: Formula, Files: []FileRisk{}}
	r.Confidence, r.ConfidenceReason = "high", ""
	if repoTotal < minHistory || repoCommits365 < minHistory {
		r.Confidence = "low"
		r.ConfidenceReason = fmt.Sprintf("sparse history: %d commits in the last year, %d total (minimum %d)", repoCommits365, repoTotal, minHistory)
	}
	maxScore, wsum, wtot := 0.0, 0.0, 0.0
	paths := make([]string, 0, len(changed))
	for p := range changed {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		s := stats[p]
		if s == nil {
			s = &FileStats{Path: p}
		}
		bug := 0.0
		if s.Commits365 > 0 {
			bug = math.Min(float64(s.Bugfix365)/float64(s.Commits365), 1)
		}
		comp := map[string]float64{
			"frequency":        s.HotspotPercentile,
			"bug_ratio":        round2(bug),
			"ownership_spread": round2(math.Min(float64(s.Authors365)/5, 1)),
			"change_size":      round2(math.Min(float64(changed[p])/200, 1)),
		}
		score := 100 * (0.35*comp["frequency"] + 0.30*comp["bug_ratio"] + 0.15*comp["ownership_spread"] + 0.20*comp["change_size"])
		conf := "high"
		switch {
		case r.Confidence == "low":
			conf = "low"
		case s.Commits365 < 3:
			conf = "medium"
		}
		r.Files = append(r.Files, FileRisk{Path: p, Score: int(math.Round(score)), Components: comp, Confidence: conf})
		maxScore = math.Max(maxScore, score)
		w := float64(changed[p]) + 1
		wsum += score * w
		wtot += w
	}
	if wtot > 0 {
		r.Score = int(math.Round(0.6*maxScore + 0.4*wsum/wtot))
	}
	if r.Confidence == "high" && len(r.Files) > 0 {
		low := 0
		for _, f := range r.Files {
			if f.Confidence != "high" {
				low++
			}
		}
		if low*2 >= len(r.Files) {
			r.Confidence, r.ConfidenceReason = "medium", "most changed files have little history (fewer than 3 commits in a year)"
		}
	}
	sort.SliceStable(r.Files, func(i, j int) bool { return r.Files[i].Score > r.Files[j].Score })
	return r
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// Author is a candidate reviewer identity from git history.
type Author struct {
	Name        string   `json:"name"`
	Email       string   `json:"email"`
	Expertise   float64  `json:"expertise" jsonschema:"recency-weighted commits on the changed files (half-life 90 days)"`
	Commits     int      `json:"commits"`
	Files       []string `json:"files"`
	LastDaysAgo int      `json:"last_days_ago"`
}

// Expertise ranks authors of commits that touched any of paths.
func Expertise(now time.Time, commits []Commit, paths []string) []Author {
	want := map[string]bool{}
	for _, p := range paths {
		want[p] = true
	}
	by := map[string]*Author{}
	files := map[string]map[string]bool{}
	for _, c := range commits {
		touched := false
		for _, f := range c.Files {
			if want[f] {
				touched = true
				if files[c.Email] == nil {
					files[c.Email] = map[string]bool{}
				}
				files[c.Email][f] = true
			}
		}
		if !touched || c.Email == "" {
			continue
		}
		a := by[c.Email]
		if a == nil {
			a = &Author{Name: c.Name, Email: c.Email, LastDaysAgo: -1}
			by[c.Email] = a
		}
		days := now.Sub(c.Time).Hours() / 24
		a.Expertise += math.Pow(0.5, days/90)
		a.Commits++
		if d := int(days); a.LastDaysAgo < 0 || d < a.LastDaysAgo {
			a.LastDaysAgo = d
		}
	}
	out := make([]Author, 0, len(by))
	for e, a := range by {
		a.Expertise = round2(a.Expertise)
		for f := range files[e] {
			a.Files = append(a.Files, f)
		}
		sort.Strings(a.Files)
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Expertise != out[j].Expertise {
			return out[i].Expertise > out[j].Expertise
		}
		return out[i].Email < out[j].Email
	})
	return out
}
