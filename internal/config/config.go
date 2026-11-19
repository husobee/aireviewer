// SPDX-License-Identifier: MIT

// Package config parses .aireviewer.yml. The file is always read from the
// PR's merge base (never the head), so a PR cannot change how it is reviewed.
package config

import (
	"fmt"

	"go.yaml.in/yaml/v3"
)

// Weights are the reviewer-suggestion factor weights (SPEC §11.1).
type Weights struct {
	Expertise      float64 `yaml:"expertise" json:"expertise"`
	Experience     float64 `yaml:"experience" json:"experience"`
	Ownership      float64 `yaml:"ownership" json:"ownership"`
	Workload       float64 `yaml:"workload" json:"workload"`
	KnowledgeShare float64 `yaml:"knowledge_share" json:"knowledge_share"`
}

// Config is the subset of .aireviewer.yml the server uses.
type Config struct {
	Risk struct {
		BugfixKeywords    []string `yaml:"bugfix_keywords" json:"bugfix_keywords"`
		MinHistoryCommits int      `yaml:"min_history_commits" json:"min_history_commits"`
		HighRisk          int      `yaml:"high_risk" json:"high_risk"`
	} `yaml:"risk" json:"risk"`
	Reviewers struct {
		Weights Weights  `yaml:"weights" json:"weights"`
		Exclude []string `yaml:"exclude" json:"exclude"`
		Max     int      `yaml:"max" json:"max"`
	} `yaml:"reviewers" json:"reviewers"`
	Limits struct {
		MaxDiffLines      int `yaml:"max_diff_lines" json:"max_diff_lines"`
		MaxParallelAgents int `yaml:"max_parallel_agents" json:"max_parallel_agents"`
	} `yaml:"limits" json:"limits"`
}

// Default returns the built-in configuration.
func Default() Config {
	var c Config
	c.Risk.BugfixKeywords = []string{"fix", "fixes", "fixed", "bug", "hotfix", "regression", "revert"}
	c.Risk.MinHistoryCommits = 20
	c.Risk.HighRisk = 60
	c.Reviewers.Weights = Weights{Expertise: 0.35, Experience: 0.2, Ownership: 0.2, Workload: 0.15, KnowledgeShare: 0.1}
	c.Reviewers.Exclude = []string{"dependabot", "renovate", "github-actions", "gitlab-bot"}
	c.Reviewers.Max = 5
	c.Limits.MaxDiffLines = 4000
	c.Limits.MaxParallelAgents = 6
	return c
}

// Parse overlays YAML on the defaults. Missing or zero fields keep defaults.
func Parse(data []byte) (Config, error) {
	c := Default()
	if len(data) == 0 {
		return c, nil
	}
	var in Config
	if err := yaml.Unmarshal(data, &in); err != nil {
		return c, fmt.Errorf(".aireviewer.yml: %w", err)
	}
	if len(in.Risk.BugfixKeywords) > 0 {
		c.Risk.BugfixKeywords = in.Risk.BugfixKeywords
	}
	if in.Risk.MinHistoryCommits > 0 {
		c.Risk.MinHistoryCommits = in.Risk.MinHistoryCommits
	}
	if in.Risk.HighRisk > 0 {
		c.Risk.HighRisk = in.Risk.HighRisk
	}
	if w := in.Reviewers.Weights; w != (Weights{}) {
		c.Reviewers.Weights = w
	}
	if len(in.Reviewers.Exclude) > 0 {
		c.Reviewers.Exclude = append(c.Reviewers.Exclude, in.Reviewers.Exclude...)
	}
	if in.Reviewers.Max > 0 {
		c.Reviewers.Max = in.Reviewers.Max
	}
	if in.Limits.MaxDiffLines > 0 {
		c.Limits.MaxDiffLines = in.Limits.MaxDiffLines
	}
	if in.Limits.MaxParallelAgents > 0 {
		c.Limits.MaxParallelAgents = in.Limits.MaxParallelAgents
	}
	return c, nil
}
