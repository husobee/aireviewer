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
		// Voice is the team's default review voice (ADR 0009), combined
		// with each reviewer's personal profile.
		Voice string `yaml:"voice" json:"voice"`
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
	// Runtime configures sandboxed execution (SPEC §10 runtime-analyst, M5).
	// Like the rest of the file it is read from the merge base, so a PR
	// cannot choose the image, the commands or the network setting.
	Runtime struct {
		Enabled      bool              `yaml:"enabled" json:"enabled"`
		Image        string            `yaml:"image" json:"image"`
		Setup        []string          `yaml:"setup" json:"setup"`
		Commands     []string          `yaml:"commands" json:"commands"`
		Reruns       int               `yaml:"reruns" json:"reruns"`
		Network      bool              `yaml:"network" json:"network"`
		SetupNetwork bool              `yaml:"setup_network" json:"setup_network"`
		TimeoutS     int               `yaml:"timeout_s" json:"timeout_s"`
		Memory       string            `yaml:"memory" json:"memory"`
		CPUs         string            `yaml:"cpus" json:"cpus"`
		Env          map[string]string `yaml:"env" json:"env"`
	} `yaml:"runtime" json:"runtime"`
	// StaticAnalysis commands run in the sandbox before the code review;
	// their findings become evidence (SPEC §8 step 4).
	StaticAnalysis []string `yaml:"static_analysis" json:"static_analysis"`
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
	c.Runtime.Reruns = 3
	c.Runtime.TimeoutS = 900
	c.Runtime.Memory = "4g"
	c.Runtime.CPUs = "2"
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
	if len(in.Comments.Voice) > 1000 {
		return c, fmt.Errorf(".aireviewer.yml: comments.voice is %d characters; keep it under 1000", len(in.Comments.Voice))
	}
	c.Comments.Voice = in.Comments.Voice
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
	r := in.Runtime
	c.Runtime.Enabled, c.Runtime.Image, c.Runtime.Setup, c.Runtime.Commands = r.Enabled, r.Image, r.Setup, r.Commands
	c.Runtime.Network, c.Runtime.SetupNetwork, c.Runtime.Env = r.Network, r.SetupNetwork, r.Env
	if r.Reruns != 0 {
		if r.Reruns < 0 || r.Reruns > 10 {
			return c, fmt.Errorf(".aireviewer.yml: runtime.reruns %d must be between 0 and 10", r.Reruns)
		}
		c.Runtime.Reruns = r.Reruns
	}
	if r.TimeoutS != 0 {
		if r.TimeoutS < 1 || r.TimeoutS > 7200 {
			return c, fmt.Errorf(".aireviewer.yml: runtime.timeout_s %d must be between 1 and 7200", r.TimeoutS)
		}
		c.Runtime.TimeoutS = r.TimeoutS
	}
	if r.Memory != "" {
		c.Runtime.Memory = r.Memory
	}
	if r.CPUs != "" {
		c.Runtime.CPUs = r.CPUs
	}
	c.StaticAnalysis = in.StaticAnalysis
	return c, nil
}
