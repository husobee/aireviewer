// SPDX-License-Identifier: MIT

package golden

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/husobee/aireviewer/internal/decisionlog"
	"github.com/husobee/aireviewer/internal/schema"
)

// DefaultHighRisk mirrors config risk.high_risk's default.
const DefaultHighRisk = 60

// Observe reads what a run produced from its run directory
// (<workspace>/.aireviewer/runs/<run id>): code-review claims, the alignment
// category and the PR risk score, all at the checked-out head.
func Observe(runDir string, highRisk int) (Observed, error) {
	o := Observed{HighRisk: highRisk}
	if o.HighRisk <= 0 {
		o.HighRisk = DefaultHighRisk
	}
	var co struct {
		HeadSHA string `json:"head_sha"`
	}
	if err := readJSON(filepath.Join(runDir, "checkout.json"), &co); err != nil || co.HeadSHA == "" {
		return o, fmt.Errorf("no checkout in %s: the run did not complete", runDir)
	}
	var reports []schema.Report
	files, _ := filepath.Glob(filepath.Join(runDir, "review", co.HeadSHA, "code-reviewer*.json"))
	sort.Strings(files)
	for _, f := range files {
		if strings.HasSuffix(f, ".dropped.json") {
			continue
		}
		var r schema.Report
		if readJSON(f, &r) == nil && r.Agent != "" {
			reports = append(reports, r)
		}
	}
	o.Claims = ClaimsFrom(reports)
	var al schema.Report
	if readJSON(filepath.Join(runDir, "augmentation", co.HeadSHA, "alignment-analyst.json"), &al) == nil {
		if v, ok := al.Facets["alignment_category"].(string); ok {
			o.Alignment = v
		}
	}
	var metrics map[string]map[string]any
	if readJSON(filepath.Join(runDir, "metrics.json"), &metrics) == nil {
		if v, ok := metrics["repo_metrics"]["pr_risk"].(float64); ok {
			n := int(v)
			o.Risk = &n
		}
	}
	return o, nil
}

func readJSON(p string, v any) error {
	b, err := os.ReadFile(p) //nolint:gosec // run directory files
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// BiasCandidate is a posted review where every AI-derived comment went out
// unchanged and nothing was disputed: a sample for the automation-bias audit
// (SPEC §16).
type BiasCandidate struct {
	Run      string   `json:"run"`
	Proposal string   `json:"proposal"`
	Comments []string `json:"comments"`
	Findings []string `json:"findings"`
}

// Bias scans every run under runsRoot for automation-bias candidates.
func Bias(runsRoot string) ([]BiasCandidate, error) {
	ents, err := os.ReadDir(runsRoot)
	if err != nil {
		return nil, err
	}
	var out []BiasCandidate
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(runsRoot, e.Name(), "decisions.jsonl")) //nolint:gosec // run directory
		if err != nil {
			continue
		}
		entries, _ := decisionlog.Parse(raw)
		out = append(out, biasIn(e.Name(), entries)...)
	}
	return out, nil
}

func biasIn(run string, entries []decisionlog.Entry) []BiasCandidate {
	finding := map[string]string{}
	edited := map[string]bool{}
	disputes := 0
	comments := map[string][]string{}
	var out []BiasCandidate
	for _, en := range entries {
		d := en.Data
		id, _ := d["id"].(string)
		switch en.Event {
		case "comment_drafted":
			f, _ := d["from_finding"].(string)
			finding[id] = f
		case "comment_edited":
			edited[id] = true
		case "finding_disputed":
			disputes++
		case "proposal_created":
			if k, _ := d["kind"].(string); k == "review" {
				if cs, ok := d["comments"].([]any); ok {
					for _, c := range cs {
						if s, ok := c.(string); ok {
							comments[id] = append(comments[id], s)
						}
					}
				}
			}
		case "posted":
			if k, _ := d["kind"].(string); k != "review" || disputes > 0 {
				continue
			}
			cs := comments[id]
			if len(cs) == 0 {
				continue
			}
			c := BiasCandidate{Run: run, Proposal: id, Comments: cs}
			all := true
			for _, cid := range cs {
				if finding[cid] == "" || edited[cid] {
					all = false
					break
				}
				c.Findings = append(c.Findings, finding[cid])
			}
			if all {
				out = append(out, c)
			}
		}
	}
	return out
}
