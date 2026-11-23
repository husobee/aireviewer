// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/golden"
)

// errRegression makes main exit non-zero when results got worse.
var errRegression = errors.New("evaluation regressed against the baseline")

// evaluate implements `aireviewer-mcp evaluate run|score|bias` (SPEC §16).
func evaluate(ctx context.Context, hosts forge.HostConfig, root string, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: aireviewer-mcp evaluate run|score|bias [flags]")
	}
	fs := flag.NewFlagSet("evaluate "+args[0], flag.ContinueOnError)
	cases := fs.String("cases", filepath.Join(root, "evaluation", "golden"), "directory of golden case YAML files")
	work := fs.String("work", filepath.Join(os.TempDir(), "aireviewer-evaluation"), "workspace root (one directory per case)")
	plugin := fs.String("plugin-dir", root, "aireviewer plugin directory (run)")
	claude := fs.String("claude", "claude", "claude binary (run)")
	maxCost := fs.Float64("max-cost", 0, "stop starting cases after spending this many USD (run; 0 = no limit)")
	out := fs.String("out", "", "write the JSON report here")
	baseline := fs.String("baseline", "", "compare with this earlier report; exit 1 on regression")
	tolerance := fs.Float64("tolerance", 0.05, "allowed absolute drop in precision, recall or F1")
	highRisk := fs.Int("high-risk", golden.DefaultHighRisk, "risk score at or above which a PR counts as high risk")
	runs := fs.String("runs", filepath.Join(root, ".aireviewer", "runs"), "run directories to scan (bias)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	if args[0] == "bias" {
		cs, err := golden.Bias(*runs)
		if err != nil {
			return err
		}
		if len(cs) == 0 {
			_, _ = fmt.Fprintln(stdout, "No automation-bias candidates: every posted review had an edit, a dispute or a human-written comment.")
			return nil
		}
		_, _ = fmt.Fprintf(stdout, "%d review(s) posted every AI-derived comment unchanged with no disputes; audit them by hand (SPEC §16):\n", len(cs))
		for _, c := range cs {
			_, _ = fmt.Fprintf(stdout, "- run %s, proposal %s: %s\n", c.Run, c.Proposal, strings.Join(c.Findings, ", "))
		}
		return nil
	}

	cs, err := golden.LoadCases(*cases)
	if err != nil {
		return err
	}
	if len(cs) == 0 {
		return fmt.Errorf("no golden cases in %s", *cases)
	}
	opts := golden.Options{PluginDir: *plugin, WorkRoot: *work, Claude: *claude, MaxCostUSD: *maxCost, HighRisk: *highRisk, Hosts: hosts}
	rep := golden.Report{Started: time.Now().UTC()}
	switch args[0] {
	case "run":
		rep.Cases = golden.Run(ctx, cs, opts)
	case "score":
		rep.Cases = golden.ScoreExisting(cs, opts)
	default:
		return fmt.Errorf("unknown evaluate command %q", args[0])
	}
	rep.Summary = golden.Summarize(rep.Cases)
	var regressions []string
	if *baseline != "" {
		base, err := golden.LoadReport(*baseline)
		if err != nil {
			return fmt.Errorf("baseline: %w", err)
		}
		regressions = golden.Regression(rep.Summary, base.Summary, *tolerance)
		rep.Notes = append(rep.Notes, regressions...)
	}
	if *out != "" {
		b, _ := json.MarshalIndent(rep, "", "  ")
		if err := os.MkdirAll(filepath.Dir(*out), 0o750); err != nil {
			return err
		}
		if err := os.WriteFile(*out, b, 0o600); err != nil {
			return err
		}
	}
	printReport(stdout, rep)
	if len(regressions) > 0 {
		return errRegression
	}
	return nil
}

func pct(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.0f%%", *p*100)
}

func okMark(b *bool) string {
	switch {
	case b == nil:
		return "–"
	case *b:
		return "✓"
	default:
		return "✗"
	}
}

func printReport(w io.Writer, rep golden.Report) {
	s := rep.Summary
	_, _ = fmt.Fprintf(w, "## aireviewer evaluation (%d cases, %d errors)\n\n", s.Cases, s.Errors)
	_, _ = fmt.Fprintf(w, "Precision %s (lower bound) · Recall %s · F1 %s · trap hits %d · alignment %s · risk %s\n",
		pct(s.Precision), pct(s.Recall), pct(s.F1), s.TrapHits, pct(s.AlignmentAcc), pct(s.RiskAcc))
	_, _ = fmt.Fprintf(w, "Cost $%.4f · tokens in %d / out %d · %s\n\n", s.CostUSD, s.InputTokens, s.OutputTokens,
		(time.Duration(s.DurationMS) * time.Millisecond).Round(time.Second))
	_, _ = fmt.Fprintln(w, "| Case | Found | Precision | Recall | Traps | Alignment | Risk | Cost | Note |")
	_, _ = fmt.Fprintln(w, "|---|---|---|---|---|---|---|---|---|")
	for _, c := range rep.Cases {
		note := c.Error
		if note == "" && len(c.Missed) > 0 {
			note = "missed: " + strings.Join(c.Missed, ", ")
		}
		_, _ = fmt.Fprintf(w, "| %s | %d/%d | %s | %s | %d | %s | %s | $%.4f | %s |\n", c.Name, c.TruePositives, c.Expected,
			pct(c.Precision), pct(c.Recall), len(c.TrapHits), okMark(c.AlignmentOK), okMark(c.RiskOK), c.CostUSD, note)
	}
	if len(s.ByCategory) > 0 {
		_, _ = fmt.Fprintln(w, "\n| Category | Found / expected | Matched / claims |")
		_, _ = fmt.Fprintln(w, "|---|---|---|")
		keys := make([]string, 0, len(s.ByCategory))
		for k := range s.ByCategory {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v := s.ByCategory[k]
			_, _ = fmt.Fprintf(w, "| %s | %d / %d | %d / %d |\n", k, v.Found, v.Expected, v.Matched, v.Claims)
		}
	}
	for _, n := range rep.Notes {
		_, _ = fmt.Fprintf(w, "\n**Regression:** %s\n", n)
	}
}
