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
	Comments struct {
		Toxicity         string `yaml:"toxicity" json:"toxicity" jsonschema:"off | warn | block"`
		Usefulness       string `yaml:"usefulness" json:"usefulness" jsonschema:"off | warn | block"`
		ProvenanceFooter *bool  `yaml:"provenance_footer" json:"provenance_footer"`
	} `yaml:"comments" json:"comments"`
	Review struct {
		RequireCritique bool `yaml:"require_critique" json:"require_critique"`
		ReviewerFirst   bool `yaml:"reviewer_first" json:"reviewer_first"`
		// LowConfidenceBreaker: low-confidence or disputed findings cannot
		// become posted comments without an explicit, logged override.
		LowConfidenceBreaker *bool `yaml:"low_confidence_breaker" json:"low_confidence_breaker"`
	} `yaml:"review" json:"review"`
	Gate struct {
		TrustedBots []string `yaml:"trusted_bots" json:"trusted_bots"`
	} `yaml:"gate" json:"gate"`
	// IssueLink configures issue retrieval for /pr-create (SPEC §8.2).
	IssueLink struct {
		Threshold         float64 `yaml:"threshold" json:"threshold" jsonschema:"minimum relevance (0-1) for a candidate to be linked with a closing keyword"`
		IncludeClosedDays int     `yaml:"include_closed_days" json:"include_closed_days" jsonschema:"also consider issues closed this many days ago or less"`
		AllowCreate       *bool   `yaml:"allow_create" json:"allow_create" jsonschema:"whether /pr-create may propose and create a new issue (default true)"`
		MaxCandidates     int     `yaml:"max_candidates" json:"max_candidates"`
	} `yaml:"issue_link" json:"issue_link"`
	// Create configures /pr-create commits.
	Create struct {
		Signoff bool `yaml:"signoff" json:"signoff" jsonschema:"add a DCO Signed-off-by trailer to the fix commit"`
	} `yaml:"create" json:"create"`
}

// Footer reports whether to add the provenance footer (default true).
func (c Config) Footer() bool {
	return c.Comments.ProvenanceFooter == nil || *c.Comments.ProvenanceFooter
}

// AllowIssueCreate reports whether /pr-create may create issues (default true).
func (c Config) AllowIssueCreate() bool {
	return c.IssueLink.AllowCreate == nil || *c.IssueLink.AllowCreate
}

// Breaker reports whether the low-confidence circuit breaker is on (default true).
func (c Config) Breaker() bool {
	return c.Review.LowConfidenceBreaker == nil || *c.Review.LowConfidenceBreaker
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
	c.Comments.Toxicity = "warn"
	c.Comments.Usefulness = "warn"
	c.IssueLink.Threshold = 0.7
	c.IssueLink.IncludeClosedDays = 30
	c.IssueLink.MaxCandidates = 20
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
	for _, m := range []struct {
		in  string
		out *string
	}{{in.Comments.Toxicity, &c.Comments.Toxicity}, {in.Comments.Usefulness, &c.Comments.Usefulness}} {
		switch m.in {
		case "":
		case "off", "warn", "block":
			*m.out = m.in
		default:
			return c, fmt.Errorf(".aireviewer.yml: comment check mode %q must be off, warn or block", m.in)
		}
	}
	c.Comments.ProvenanceFooter = in.Comments.ProvenanceFooter
	c.Review = in.Review
	c.Gate = in.Gate
	if t := in.IssueLink.Threshold; t != 0 {
		if t < 0 || t > 1 {
			return c, fmt.Errorf(".aireviewer.yml: issue_link.threshold %v must be between 0 and 1", t)
		}
		c.IssueLink.Threshold = t
	}
	if in.IssueLink.IncludeClosedDays > 0 {
		c.IssueLink.IncludeClosedDays = in.IssueLink.IncludeClosedDays
	}
	c.IssueLink.AllowCreate = in.IssueLink.AllowCreate
	if in.IssueLink.MaxCandidates > 0 {
		c.IssueLink.MaxCandidates = min(in.IssueLink.MaxCandidates, 50)
	}
	c.Create = in.Create
	return c, nil
}
