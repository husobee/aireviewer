// SPDX-License-Identifier: MIT

package golden

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/runctx"
)

// Usage is what `claude -p --output-format json` reports for one run
// (paper §5.1.10: track tokens and cost per run).
type Usage struct {
	CostUSD      float64 `json:"total_cost_usd"`
	DurationMS   int64   `json:"duration_ms"`
	NumTurns     int     `json:"num_turns"`
	IsError      bool    `json:"is_error"`
	Result       string  `json:"result"`
	InputTokens  int     `json:"-"`
	OutputTokens int     `json:"-"`
}

// ParseClaudeJSON reads the final result object of a headless Claude Code
// run. Input tokens include cache reads and writes.
func ParseClaudeJSON(b []byte) (Usage, error) {
	var raw struct {
		Usage
		Use struct {
			Input       int `json:"input_tokens"`
			Output      int `json:"output_tokens"`
			CacheRead   int `json:"cache_read_input_tokens"`
			CacheCreate int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	// The JSON result is the last line; tolerate leading log output.
	i := bytes.LastIndex(b, []byte("\n{"))
	if i >= 0 {
		b = b[i+1:]
	}
	if err := json.Unmarshal(bytes.TrimSpace(b), &raw); err != nil {
		return Usage{}, fmt.Errorf("unreadable claude output: %w", err)
	}
	u := raw.Usage
	u.InputTokens = raw.Use.Input + raw.Use.CacheRead + raw.Use.CacheCreate
	u.OutputTokens = raw.Use.Output
	return u, nil
}

// Exec runs a command in dir with extra environment and returns stdout.
type Exec func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error)

// OSExec is the default Exec.
func OSExec(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // fixed binary (claude) with internally built args
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil && out.Len() == 0 {
		return nil, fmt.Errorf("%s: %w: %s", name, err, lastN(errb.String(), 400))
	}
	return out.Bytes(), nil
}

func lastN(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// Options configures Run.
type Options struct {
	PluginDir  string        // the aireviewer plugin
	WorkRoot   string        // one workspace per case under it
	Claude     string        // claude binary (default "claude")
	MaxCostUSD float64       // stop starting cases once spent (0 = no limit)
	Timeout    time.Duration // per case (default 30m)
	HighRisk   int
	Hosts      forge.HostConfig
	Exec       Exec
}

var safeName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// RunDir returns the run directory a case produces in its workspace.
func RunDir(o Options, c Case) (string, error) {
	ref, err := o.Hosts.ParsePRURL(c.URL)
	if err != nil {
		return "", err
	}
	return filepath.Join(o.WorkRoot, c.Name, ".aireviewer", "runs", runctx.RunIDForPR(ref)), nil
}

// Run executes each case end to end with `claude -p` in eval mode (the
// server refuses every forge write and runs no code) and scores it.
func Run(ctx context.Context, cases []Case, o Options) []CaseResult {
	if o.Claude == "" {
		o.Claude = "claude"
	}
	if o.Exec == nil {
		o.Exec = OSExec
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Minute
	}
	var out []CaseResult
	spent := 0.0
	for _, c := range cases {
		r := CaseResult{Name: c.Name}
		switch {
		case !safeName.MatchString(c.Name):
			r.Error = "case name must be letters, digits, dot, dash or underscore"
		case o.MaxCostUSD > 0 && spent >= o.MaxCostUSD:
			r.Error = fmt.Sprintf("skipped: cost budget of $%.2f reached", o.MaxCostUSD)
		default:
			r = runOne(ctx, c, o)
			spent += r.CostUSD
		}
		out = append(out, r)
	}
	return out
}

func runOne(ctx context.Context, c Case, o Options) CaseResult {
	fail := func(err error) CaseResult { return CaseResult{Name: c.Name, Error: err.Error()} }
	runDir, err := RunDir(o, c)
	if err != nil {
		return fail(err)
	}
	ws := filepath.Join(o.WorkRoot, c.Name)
	_ = os.RemoveAll(ws)
	if err := os.MkdirAll(ws, 0o750); err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	prompt := "/aireviewer:pr-review " + c.URL + " --eval"
	if c.Mode == "author" {
		prompt += " --author-mode"
	}
	raw, err := o.Exec(ctx, ws, []string{"AIREVIEWER_EVAL=1", "AIREVIEWER_HEADLESS=1"}, o.Claude,
		"-p", "--plugin-dir", o.PluginDir, "--output-format", "json", "--allowedTools", "Agent", prompt)
	if err != nil {
		return fail(err)
	}
	u, err := ParseClaudeJSON(raw)
	if err != nil {
		return fail(err)
	}
	obs, err := Observe(runDir, o.HighRisk)
	r := Score(c, obs)
	r.CostUSD, r.InputTokens, r.OutputTokens, r.DurationMS = u.CostUSD, u.InputTokens, u.OutputTokens, u.DurationMS
	if err != nil {
		r.Error = err.Error()
	} else if u.IsError {
		r.Error = "claude reported an error: " + lastN(u.Result, 300)
	}
	return r
}

// ScoreExisting re-scores runs already in the workspaces (no model calls).
func ScoreExisting(cases []Case, o Options) []CaseResult {
	var out []CaseResult
	for _, c := range cases {
		runDir, err := RunDir(o, c)
		if err != nil {
			out = append(out, CaseResult{Name: c.Name, Error: err.Error()})
			continue
		}
		obs, err := Observe(runDir, o.HighRisk)
		if err != nil {
			out = append(out, CaseResult{Name: c.Name, Error: err.Error()})
			continue
		}
		out = append(out, Score(c, obs))
	}
	return out
}

// Report is a stored evaluation result.
type Report struct {
	Started time.Time    `json:"started"`
	Summary Summary      `json:"summary"`
	Cases   []CaseResult `json:"cases"`
	Notes   []string     `json:"notes,omitempty"`
}

// LoadReport reads a stored report (for a baseline).
func LoadReport(path string) (Report, error) {
	var r Report
	if err := readJSON(path, &r); err != nil {
		return r, err
	}
	if r.Summary.Cases == 0 {
		return r, errors.New("baseline has no cases")
	}
	return r, nil
}
