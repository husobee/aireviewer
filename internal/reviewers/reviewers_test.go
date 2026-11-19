// SPDX-License-Identifier: MIT

package reviewers

import (
	"strings"
	"testing"

	"github.com/husobee/aireviewer/internal/config"
	"github.com/husobee/aireviewer/internal/history"
)

const co = `# owners
*            @org/everyone
/docs/       @docs-team
*.go         @gophers
/pkg/http/   @alice   # http owner
**/testdata/** @qa
[Section]
`

func TestOwners(t *testing.T) {
	rules := ParseCodeowners(co)
	cases := map[string]string{
		"README.md":                 "@org/everyone",
		"docs/guide.md":             "@docs-team",
		"cmd/main.go":               "@gophers",
		"pkg/http/client.go":        "@alice",
		"pkg/http/sub/x.txt":        "@alice",
		"a/testdata/b/fixture.json": "@qa",
		"src/docs/x.md":             "@org/everyone", // /docs/ is anchored
	}
	for path, want := range cases {
		got := Owners(rules, path)
		if len(got) != 1 || got[0] != want {
			t.Errorf("Owners(%q) = %v, want %s", path, got, want)
		}
	}
}

func TestGitLabSections(t *testing.T) {
	rules := ParseCodeowners("^[Documentation Pages]\n/*.md @aqualls\n[Backend][2] @be-lead @be2\napp/\n")
	if o := Owners(rules, "README.md"); len(o) != 1 || o[0] != "@aqualls" {
		t.Errorf("README owners = %v", o)
	}
	if o := Owners(rules, "app/x.go"); len(o) != 2 || o[0] != "@be-lead" {
		t.Errorf("section default owners = %v", o)
	}
	// An unmapped git identity with the same local part merges into the handle.
	got, _ := Suggest(Input{ChangedFiles: []string{"README.md"}, Rules: rules, Config: config.Default(),
		Authors: []history.Author{{Name: "Amy", Email: "aqualls@gitlab.com", Expertise: 2, Files: []string{"README.md"}}}})
	if len(got) != 1 || got[0].Identity != "@aqualls" || got[0].Factors["ownership"] != 1 || got[0].Factors["expertise"] != 1 ||
		!strings.Contains(strings.Join(got[0].Reasons, ";"), "heuristic") {
		t.Errorf("merged = %+v", got)
	}
}

func TestSuggest(t *testing.T) {
	cfg := config.Default()
	in := Input{
		ChangedFiles: []string{"pkg/http/client.go", "pkg/http/retry.go"},
		Authors: []history.Author{
			{Name: "Bob", Email: "bob@x", Expertise: 3, Files: []string{"pkg/http/client.go", "pkg/http/retry.go"}},
			{Name: "Ann", Email: "ann@x", Expertise: 1, Files: []string{"pkg/http/client.go"}},
			{Name: "Pat Author", Email: "pat@x", Expertise: 5, Files: []string{"pkg/http/client.go"}},
			{Name: "dependabot[bot]", Email: "bot@x", Expertise: 9, Files: []string{"pkg/http/client.go"}},
		},
		EmailLogin: map[string]string{"bob@x": "bob", "pat@x": "pat"},
		Rules:      ParseCodeowners(co),
		Workload:   map[string]int{"bob": 12, "alice": 0},
		PRAuthor:   "pat",
		Config:     cfg,
	}
	got, mode := Suggest(in)
	if mode != Balanced {
		t.Errorf("mode = %s", mode)
	}
	ids := []string{}
	for _, c := range got {
		ids = append(ids, c.Identity)
	}
	joined := strings.Join(ids, ",")
	if strings.Contains(joined, "pat") || strings.Contains(joined, "bot") {
		t.Errorf("author or bot not excluded: %v", ids)
	}
	if len(got) != 3 {
		t.Fatalf("candidates = %v", ids)
	}
	var bob, ann, alice Candidate
	for _, c := range got {
		switch c.Identity {
		case "@bob":
			bob = c
		case "git:ann@x":
			ann = c
		case "@alice":
			alice = c
		}
	}
	if bob.Factors["expertise"] != 1 || bob.Factors["workload"] != 0 || bob.OpenLoad != 12 || !bob.Mapped {
		t.Errorf("bob = %+v", bob)
	}
	if ann.Mapped || ann.OpenLoad != -1 || ann.Factors["workload"] != 0.5 {
		t.Errorf("ann = %+v", ann)
	}
	if alice.Factors["ownership"] != 1 || alice.Factors["workload"] != 1 {
		t.Errorf("alice = %+v", alice)
	}

	in.RiskScore = 90
	_, mode = Suggest(in)
	if mode != ExpertiseFirst {
		t.Errorf("high-risk mode = %s", mode)
	}
}
