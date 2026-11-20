// SPDX-License-Identifier: MIT

package forge

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// BugLabeler finds the commits of merged PRs that carry any of the given
// labels, merged in [since, until] (M6, risk.bug_labels). The set contains
// each PR's own commits and its merge or squash commit, so it matches
// whichever of them appear in non-merge history. prs is how many labelled
// PRs were found; limit caps them.
type BugLabeler interface {
	LabelledFixCommits(ctx context.Context, repo Repo, labels []string, since, until time.Time, limit int) (commits map[string]bool, prs int, err error)
}

// Issue is a normalised issue linked to a PR.
type Issue struct {
	Ref    string   `json:"ref" jsonschema:"forge-native reference, e.g. o/r#12 or g/p#12"`
	Number int      `json:"number"`
	Title  string   `json:"title" jsonschema:"untrusted"`
	Body   string   `json:"body" jsonschema:"untrusted; may contain acceptance criteria"`
	State  string   `json:"state"`
	URL    string   `json:"url"`
	Labels []string `json:"labels"`
	// Source records how the link was found: "closing_reference" (the
	// forge's own PR↔issue link) or "body_keyword" (Fixes #N in the PR text).
	Source string `json:"source"`
}

// IssueRef is a parsed issue reference from PR text.
type IssueRef struct {
	Project string // empty = same project as the PR
	Number  int
}

// closingRef matches GitHub/GitLab closing keywords followed by an issue
// reference: "Fixes #12", "closes org/repo#3", "Resolves: group/sub/proj#9",
// or a full issue URL.
var closingRef = regexp.MustCompile(`(?i)\b(?:close[sd]?|closing|fix(?:e[sd])?|fixing|resolve[sd]?|resolving|implement(?:s|ed|ing)?)\b[:\s]+` +
	`(?:([\w.-]+(?:/[\w.-]+)+)?#(\d+)|https?://[^\s/]+/([\w./-]+?)(?:/-)?/issues/(\d+))`)

// ParseClosingRefs extracts closing-keyword issue references from PR text,
// de-duplicated in order of appearance.
func ParseClosingRefs(body string) []IssueRef {
	seen := map[IssueRef]bool{}
	var out []IssueRef
	for _, m := range closingRef.FindAllStringSubmatch(body, -1) {
		var r IssueRef
		if m[2] != "" {
			r.Project, r.Number = m[1], atoi(m[2])
		} else {
			r.Project, r.Number = m[3], atoi(m[4])
		}
		if r.Number > 0 && !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// IssueRefString renders a reference the way the forge writes it.
func IssueRefString(project string, number int) string {
	return project + "#" + strconv.Itoa(number)
}

// IssueLinker is implemented by providers that can list a PR's linked issues.
type IssueLinker interface {
	LinkedIssues(ctx context.Context, pr *PR) ([]Issue, error)
}

// AuthorMapper maps git commit author emails to forge usernames for the
// commits that touched a path (used by reviewer suggestion).
type AuthorMapper interface {
	CommitAuthorLogins(ctx context.Context, repo Repo, path string, since time.Time) (map[string]string, error)
}

// WorkloadReporter reports how many open review requests a user has.
type WorkloadReporter interface {
	OpenReviewRequests(ctx context.Context, repo Repo, user string) (int, error)
}

// SameProject reports whether a parsed ref points at the PR's own project.
func (r IssueRef) SameProject(project string) bool {
	return r.Project == "" || strings.EqualFold(r.Project, project)
}
