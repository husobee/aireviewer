// SPDX-License-Identifier: MIT

package config

import (
	"os"
	"testing"
)

func TestParse(t *testing.T) {
	c, err := Parse([]byte("risk:\n  min_history_commits: 50\nreviewers:\n  exclude: [mybot]\nlimits:\n  max_diff_lines: 900\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Risk.MinHistoryCommits != 50 || c.Limits.MaxDiffLines != 900 || c.Limits.MaxParallelAgents != 6 ||
		c.Reviewers.Weights.Expertise != 0.35 || c.Reviewers.Exclude[len(c.Reviewers.Exclude)-1] != "mybot" {
		t.Errorf("config = %+v", c)
	}
	if _, err := Parse([]byte("risk: [")); err == nil {
		t.Error("invalid YAML accepted")
	}
}

func TestExampleParses(t *testing.T) {
	b, err := os.ReadFile("../../examples/.aireviewer.yml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(b); err != nil {
		t.Fatalf("examples/.aireviewer.yml: %v", err)
	}
}
