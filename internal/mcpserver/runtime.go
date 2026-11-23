// SPDX-License-Identifier: MIT

package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/husobee/aireviewer/internal/forge"
	"github.com/husobee/aireviewer/internal/sandbox"
)

// M5: runtime analysis and static analyzers in a Docker sandbox (SPEC §10,
// §15; ADR 0008).
//
// Running a PR's code executes third-party code on the user's machine, so it
// is gated like a forge write: runtime_prepare freezes the exact plan (image,
// commands, network, limits, commits) as a "runtime" proposal, and nothing
// runs until the human confirms it through proposal_submit. The plan comes
// from the merge-base .aireviewer.yml (or an on-demand command the reviewer
// asked for, shown in the preview), never from the PR head.

func registerRuntimeTools(s *mcp.Server, h *handlers) {
	ro := &mcp.ToolAnnotations{ReadOnlyHint: true}
	local := &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: boolPtr(false), IdempotentHint: false}

	mcp.AddTool(s, &mcp.Tool{Name: "runtime_prepare", Annotations: local,
		Description: "Plan sandboxed execution for the run: the configured runtime commands (or on-demand commands the reviewer asked for) on " +
			"the merge base and the head, plus static analyzers on the head, from the merge-base .aireviewer.yml. Returns not_applicable with " +
			"the reason (no config, no Docker), or a 'runtime' proposal whose preview states exactly what will run, with which image, network " +
			"and limits. Nothing runs until the human confirms it with proposal_submit."},
		h.runtimePrepare)
	mcp.AddTool(s, &mcp.Tool{Name: "runtime_run", Annotations: local,
		Description: "Execute a confirmed runtime plan: each command on the merge base and the head in an isolated container; failures are " +
			"re-run runtime.reruns times and classified per test (go test -json) or per command as pass, deterministic_fail, flaky or " +
			"timeout, then compared base vs head (new_failure, fixed, still_failing, flaky…). Logs are saved as runtime/ artifacts for " +
			"trace evidence and counts are recorded as metrics (source runtime). Can take several minutes."},
		h.runtimeRun)
	mcp.AddTool(s, &mcp.Tool{Name: "runtime_results", Annotations: ro,
		Description: "Return the results of the latest (or a given) runtime run: per-command and per-test classifications, deltas and log artifacts."},
		h.runtimeResults)
	mcp.AddTool(s, &mcp.Tool{Name: "lint_run", Annotations: local,
		Description: "Run the confirmed plan's static analyzers on the head in the sandbox and return normalised findings (file, line, " +
			"linter, message), marked in_diff/changed against the diff-map. Saved to lint/<head>/findings.json; logs are runtime/ artifacts. " +
			"Findings are leads for the code reviewer, not verdicts."},
		h.lintRun)
	mcp.AddTool(s, &mcp.Tool{Name: "runtime_import", Annotations: local,
		Description: "Copy runtime results, logs and lint findings (with their metrics) from another local run of the same repository at " +
			"the exact same head, for example from the /pr-create branch run into the new PR run. Refuses when the heads differ: runtime " +
			"results are never reused across commits."},
		h.runtimeImport)
}

const sandboxUser = "65534:65534"

type runtimePlan struct {
	HeadSHA        string            `json:"head_sha"`
	MergeBase      string            `json:"merge_base"`
	Image          string            `json:"image"`
	Setup          []string          `json:"setup"`
	Commands       []string          `json:"commands"`
	StaticAnalysis []string          `json:"static_analysis"`
	Reruns         int               `json:"reruns"`
	Network        bool              `json:"network"`
	SetupNetwork   bool              `json:"setup_network"`
	TimeoutS       int               `json:"timeout_s"`
	Memory         string            `json:"memory"`
	CPUs           string            `json:"cpus"`
	Env            map[string]string `json:"env,omitempty"`
	OnDemand       bool              `json:"on_demand"`
}

// containerEnv points every common tool cache at the cache volume (the root
// filesystem is read-only) and keeps toolchains from downloading themselves.
func (p runtimePlan) containerEnv() map[string]string {
	env := map[string]string{
		"HOME": "/cache/home", "XDG_CACHE_HOME": "/cache/xdg", "GOPATH": "/cache/go", "GOCACHE": "/cache/go-build",
		"GOTOOLCHAIN": "local", "npm_config_cache": "/cache/npm", "PIP_CACHE_DIR": "/cache/pip", "CARGO_HOME": "/cache/cargo",
		"CI": "true", "TMPDIR": "/tmp",
	}
	for k, v := range p.Env {
		env[k] = v
	}
	return env
}

// ---- runtime_prepare ----

type RuntimePrepareIn struct {
	RunID    string   `json:"run_id"`
	Commands []string `json:"commands,omitempty" jsonschema:"on-demand commands requested by the reviewer, replacing the configured runtime.commands (image, limits and network still come from the merge-base config)"`
}

type RuntimePrepareOut struct {
	Applicable bool         `json:"applicable"`
	Reason     string       `json:"reason,omitempty" jsonschema:"why runtime analysis is not applicable; report it as not_applicable, never guess results"`
	Proposal   *ProposalOut `json:"proposal,omitempty"`
}

func (h *handlers) runtimePrepare(ctx context.Context, _ *mcp.CallToolRequest, in RuntimePrepareIn) (*mcp.CallToolResult, RuntimePrepareOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, RuntimePrepareOut{}, err
	}
	cfg, cfgErr := h.loadConfig(ctx, st)
	na := func(reason string) (*mcp.CallToolResult, RuntimePrepareOut, error) {
		return nil, RuntimePrepareOut{Reason: reason}, nil
	}
	if h.cfg.Headless {
		return na("headless (CI) mode: running PR code needs a human to confirm the plan (ADR 0008, 0011)")
	}
	if cfgErr != "" {
		return na("the merge-base .aireviewer.yml is invalid: " + cfgErr)
	}
	rc := cfg.Runtime
	if strings.TrimSpace(rc.Image) == "" {
		return na("no runtime.image is configured in the merge-base .aireviewer.yml")
	}
	commands := rc.Commands
	if !rc.Enabled {
		commands = nil
	}
	if len(in.Commands) > 0 {
		commands = in.Commands
	}
	for _, c := range append(append([]string{}, commands...), rc.Setup...) {
		if strings.TrimSpace(c) == "" || len(c) > 2000 || strings.ContainsRune(c, 0) {
			return nil, RuntimePrepareOut{}, fmt.Errorf("invalid command %.60q", c)
		}
	}
	if len(commands) == 0 && len(cfg.StaticAnalysis) == 0 {
		if !rc.Enabled {
			return na("runtime.enabled is false and no static_analysis is configured in the merge-base .aireviewer.yml")
		}
		return na("no runtime.commands or static_analysis are configured")
	}
	if h.cfg.Sandbox == nil {
		return na("no sandbox runner is configured")
	}
	if err := h.cfg.Sandbox.Available(ctx); err != nil {
		return na("Docker is not available: " + err.Error())
	}
	plan := runtimePlan{HeadSHA: st.HeadSHA, MergeBase: st.MergeBase, Image: rc.Image, Setup: rc.Setup, Commands: commands,
		StaticAnalysis: cfg.StaticAnalysis, Reruns: rc.Reruns, Network: rc.Network, SetupNetwork: rc.SetupNetwork,
		TimeoutS: rc.TimeoutS, Memory: rc.Memory, CPUs: rc.CPUs, Env: rc.Env, OnDemand: len(in.Commands) > 0}
	preview := h.runtimePreview(in.RunID, st, plan)
	p, err := freeze("runtime", st.HeadSHA, plan, preview, h.now())
	if err != nil {
		return nil, RuntimePrepareOut{}, err
	}
	if err := h.saveProposal(in.RunID, p); err != nil {
		return nil, RuntimePrepareOut{}, err
	}
	_, _ = h.logEvent(in.RunID, "proposal_created", "orchestrator", map[string]any{"id": p.ID, "kind": p.Kind, "hash": p.Hash,
		"image": plan.Image, "commands": len(plan.Commands), "analyzers": len(plan.StaticAnalysis), "network": plan.Network, "on_demand": plan.OnDemand})
	warnings := []string{}
	if !strings.Contains(plan.Image, "@sha256:") {
		warnings = append(warnings, "the image is not pinned by digest; its content can change between runs")
	}
	if plan.Network || plan.SetupNetwork {
		warnings = append(warnings, "network access is enabled for part of the run")
	}
	return nil, RuntimePrepareOut{Applicable: true, Proposal: &ProposalOut{ProposalID: p.ID, Hash: p.Hash, Preview: p.Preview, Warnings: warnings}}, nil
}

func onOff(b bool) string {
	if b {
		return "**on**"
	}
	return "off"
}

func codeList(cmds []string) string {
	if len(cmds) == 0 {
		return "_none_\n"
	}
	return "```sh\n" + strings.Join(cmds, "\n") + "\n```\n"
}

func (h *handlers) runtimePreview(runID string, st *checkoutState, p runtimePlan) string {
	var b strings.Builder
	whose := "your branch `" + st.HeadBranch + "`"
	target := "branch " + st.HeadBranch
	if !st.isBranch() {
		author := "the author"
		if pr, err := h.loadPR(runID); err == nil && pr.Author != "" {
			author = "@" + pr.Author
		}
		whose = author + "'s " + st.Repo.Forge.Abbrev() + " head (third-party code)"
		target = fmt.Sprintf("%s %s", st.Repo.Forge.Abbrev(), runIDRef(st))
	}
	fmt.Fprintf(&b, "**Run code in the local Docker sandbox** for %s\n\n", target)
	fmt.Fprintf(&b, "- Code: %s at head `%s`, compared with the merge base `%s`\n", whose, short(p.HeadSHA), short(p.MergeBase))
	fmt.Fprintf(&b, "- Image: `%s`\n", p.Image)
	fmt.Fprintf(&b, "- Network: setup %s, commands and analyzers %s\n", onOff(p.SetupNetwork), onOff(p.Network))
	fmt.Fprintf(&b, "- Limits: %s CPUs, %s memory, 512 processes, %ds per command; all capabilities dropped, non-root user, read-only root filesystem, no credentials or host paths except a copy of the code\n",
		p.CPUs, p.Memory, p.TimeoutS)
	src := "`.aireviewer.yml` at the merge base"
	if p.OnDemand {
		src = "on-demand request (image, limits and network from the merge-base config)"
	}
	fmt.Fprintf(&b, "- Source of the commands: %s\n", src)
	fmt.Fprintf(&b, "\n#### Setup (on each side)\n%s", codeList(p.Setup))
	fmt.Fprintf(&b, "\n#### Commands (merge base and head; failures re-run %d×)\n%s", p.Reruns, codeList(p.Commands))
	fmt.Fprintf(&b, "\n#### Static analyzers (head only, with lint_run)\n%s", codeList(p.StaticAnalysis))
	return b.String()
}

func runIDRef(st *checkoutState) string {
	return forge.PRRef{Repo: st.Repo, Number: st.Number}.String()
}

// ---- allowance (proposal_submit for kind "runtime") ----

type allowedFile struct {
	Proposals []string `json:"proposals"`
}

func (h *handlers) allowRuntime(runID string, st *checkoutState, p *Proposal) (string, error) {
	h.stateMu.Lock()
	defer h.stateMu.Unlock()
	var af allowedFile
	path := "sandbox/" + st.HeadSHA + "/allowed.json"
	if err := h.loadJSON(runID, path, &af); err != nil {
		return "", err
	}
	af.Proposals = append(af.Proposals, p.ID)
	if err := h.saveJSON(runID, path, af); err != nil {
		return "", err
	}
	return "Execution allowed for head " + short(st.HeadSHA) + ". Next: runtime_run (and lint_run for the analyzers).", nil
}

// allowedPlan returns a confirmed runtime plan for the current head.
func (h *handlers) allowedPlan(runID string, st *checkoutState, id string) (*Proposal, runtimePlan, error) {
	var af allowedFile
	if err := h.loadJSON(runID, "sandbox/"+st.HeadSHA+"/allowed.json", &af); err != nil {
		return nil, runtimePlan{}, err
	}
	if len(af.Proposals) == 0 {
		return nil, runtimePlan{}, errors.New("no confirmed runtime plan for this head: call runtime_prepare, show the preview, and have the human confirm it with proposal_submit")
	}
	if id == "" {
		id = af.Proposals[len(af.Proposals)-1]
	}
	allowed := false
	for _, a := range af.Proposals {
		allowed = allowed || a == id
	}
	if !allowed {
		return nil, runtimePlan{}, fmt.Errorf("runtime plan %s was not confirmed for this head", id)
	}
	var p Proposal
	if err := h.loadJSON(runID, "proposals/"+id+".json", &p); err != nil || p.ID == "" {
		return nil, runtimePlan{}, fmt.Errorf("no proposal %q", id)
	}
	if !p.Submitted || p.Kind != "runtime" || p.HeadSHA != st.HeadSHA || !proposalIntact(&p) {
		return nil, runtimePlan{}, fmt.Errorf("proposal %s is not a confirmed runtime plan for this head", id)
	}
	var plan runtimePlan
	if err := json.Unmarshal(p.Payload, &plan); err != nil {
		return nil, runtimePlan{}, err
	}
	return &p, plan, nil
}

// ---- execution ----

type RunRecord struct {
	Attempt    int               `json:"attempt"`
	Exit       int               `json:"exit"`
	TimedOut   bool              `json:"timed_out"`
	DurationMS int64             `json:"duration_ms"`
	Log        string            `json:"log" jsonschema:"runtime/ artifact; cite it as trace evidence"`
	Truncated  bool              `json:"truncated,omitempty" jsonschema:"only the tail of the output was kept"`
	tests      map[string]string // per-test outcomes of this attempt (go test -json)
}

func (r RunRecord) outcome() string {
	switch {
	case r.TimedOut:
		return sandbox.Timeout
	case r.Exit == 0:
		return sandbox.Pass
	default:
		return sandbox.Fail
	}
}

type SideCommand struct {
	Class string      `json:"class"`
	Runs  []RunRecord `json:"runs"`
}

type TestResult struct {
	Name  string `json:"name"`
	Base  string `json:"base"`
	Head  string `json:"head"`
	Delta string `json:"delta"`
}

type CommandResult struct {
	Index      int            `json:"index"`
	Cmd        string         `json:"cmd"`
	Base       SideCommand    `json:"base"`
	Head       SideCommand    `json:"head"`
	Delta      string         `json:"delta"`
	Tests      []TestResult   `json:"tests,omitempty" jsonschema:"per-test results when the command emits go test -json"`
	TestDeltas map[string]int `json:"test_deltas,omitempty"`
}

type RuntimeResults struct {
	ProposalID  string                `json:"proposal_id"`
	HeadSHA     string                `json:"head_sha"`
	MergeBase   string                `json:"merge_base"`
	Image       string                `json:"image"`
	Network     bool                  `json:"network"`
	Setup       map[string]*RunRecord `json:"setup,omitempty"`
	SetupFailed map[string]bool       `json:"setup_failed,omitempty"`
	Commands    []CommandResult       `json:"commands"`
	Summary     map[string]int        `json:"summary" jsonschema:"counts of deltas across commands and tests"`
	Started     time.Time             `json:"started"`
	Finished    time.Time             `json:"finished"`
}

type sideEnv struct {
	name, sha, work, cache string
}

func (h *handlers) artifactDir(st *checkoutState, p *Proposal) string {
	return "runtime/" + st.HeadSHA + "/" + p.ID + "/"
}

// prepSide exports the tree at sha, creates the volumes, copies the code in
// and runs setup. The returned cleanup removes the volumes and the copy.
func (h *handlers) prepSide(ctx context.Context, runID string, st *checkoutState, p *Proposal, plan runtimePlan, side, sha string) (*sideEnv, *RunRecord, func(), error) {
	r := h.cfg.Sandbox
	dir := filepath.Join(h.cfg.CacheDir, "sandbox", runID, p.ID, side)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // must be readable by the container user
		return nil, nil, nil, err
	}
	name := "aireviewer-" + sha256Short(runID+"\x00"+p.ID+"\x00"+side)
	env := &sideEnv{name: side, sha: sha, work: name + "-work", cache: name + "-cache"}
	cleanup := func() {
		r.RemoveVolume(ctx, env.work)
		r.RemoveVolume(ctx, env.cache)
		_ = os.RemoveAll(dir)
	}
	if err := h.git(st.Repo).Archive(ctx, sha, dir); err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	for _, v := range []string{env.work, env.cache} {
		r.RemoveVolume(ctx, v)
		if err := r.CreateVolume(ctx, v); err != nil {
			cleanup()
			return nil, nil, nil, err
		}
	}
	vols := []sandbox.Mount{{Source: env.work, Target: "/work", Volume: true}, {Source: env.cache, Target: "/cache", Volume: true}}
	// The copy step is the only one that runs as root, with CHOWN as its
	// only capability and no network; it never runs repository code.
	res, err := r.Run(ctx, sandbox.Spec{Name: name + "-copy", Image: plan.Image, Workdir: "/",
		Cmd:    "cp -R /src/. /work/ && chown -R " + sandboxUser + " /work /cache",
		Mounts: append([]sandbox.Mount{{Source: dir, Target: "/src", ReadOnly: true}}, vols...), CapAdd: []string{"CHOWN"},
		Timeout: 10 * time.Minute, Memory: plan.Memory, CPUs: plan.CPUs})
	if err == nil && res.Exit != 0 {
		err = fmt.Errorf("copying the code into the sandbox failed (exit %d): %s", res.Exit, lastLinesOf(res.Output, 5))
	}
	if err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	if len(plan.Setup) == 0 {
		return env, nil, cleanup, nil
	}
	rec, err := h.sandboxExec(ctx, runID, plan, env, strings.Join(plan.Setup, " && "), plan.SetupNetwork,
		h.artifactDir(st, p)+side+"-setup.log", 0)
	if err != nil {
		cleanup()
		return nil, nil, nil, err
	}
	return env, &rec, cleanup, nil
}

func (h *handlers) sandboxExec(ctx context.Context, runID string, plan runtimePlan, env *sideEnv, cmd string, network bool, logPath string, attempt int) (RunRecord, error) {
	name := fmt.Sprintf("aireviewer-%s", sha256Short(runID+"\x00"+logPath+"\x00"+fmt.Sprint(time.Now().UnixNano())))
	res, err := h.cfg.Sandbox.Run(ctx, sandbox.Spec{Name: name, Image: plan.Image, Cmd: cmd, Workdir: "/work", User: sandboxUser,
		Network: network, Env: plan.containerEnv(), Timeout: time.Duration(plan.TimeoutS) * time.Second, Memory: plan.Memory, CPUs: plan.CPUs,
		Mounts: []sandbox.Mount{{Source: env.work, Target: "/work", Volume: true}, {Source: env.cache, Target: "/cache", Volume: true}}})
	if err != nil {
		return RunRecord{}, err
	}
	header := fmt.Sprintf("# aireviewer sandbox: side=%s commit=%s attempt=%d exit=%d timed_out=%v duration=%s\n# $ %s\n",
		env.name, env.sha, attempt, res.Exit, res.TimedOut, res.Duration.Round(time.Millisecond), cmd)
	if res.Truncated {
		header += "# (output truncated: only the tail was kept)\n"
	}
	if _, err := h.cfg.Store.Put(runID, logPath, append([]byte(header), res.Output...)); err != nil {
		return RunRecord{}, err
	}
	rec := RunRecord{Attempt: attempt, Exit: res.Exit, TimedOut: res.TimedOut, DurationMS: res.Duration.Milliseconds(), Log: logPath, Truncated: res.Truncated}
	if tests, ok := sandbox.ParseGoTestJSON(res.Output); ok {
		rec.tests = tests
	}
	return rec, nil
}

func failed(r RunRecord) bool {
	if r.Exit != 0 || r.TimedOut {
		return true
	}
	for _, o := range r.tests {
		if o == sandbox.Fail {
			return true
		}
	}
	return false
}

// runWithReruns runs cmd once and, if anything failed, re-runs it
// plan.Reruns more times so failures can be classified before any claim.
func (h *handlers) runWithReruns(ctx context.Context, runID string, st *checkoutState, p *Proposal, plan runtimePlan, env *sideEnv, i int, cmd string) ([]RunRecord, error) {
	var recs []RunRecord
	for k := 1; k <= plan.Reruns+1; k++ {
		rec, err := h.sandboxExec(ctx, runID, plan, env, cmd, plan.Network, fmt.Sprintf("%s%s-cmd%d-run%d.log", h.artifactDir(st, p), env.name, i+1, k), k)
		if err != nil {
			return recs, err
		}
		recs = append(recs, rec)
		if !failed(recs[0]) {
			break
		}
	}
	return recs, nil
}

func sideClass(recs []RunRecord) string {
	outs := make([]string, len(recs))
	for i, r := range recs {
		outs[i] = r.outcome()
	}
	return sandbox.Classify(outs)
}

func testClasses(recs []RunRecord) map[string]string {
	names := map[string]bool{}
	for _, r := range recs {
		for n := range r.tests {
			names[n] = true
		}
	}
	out := map[string]string{}
	for n := range names {
		var outs []string
		for _, r := range recs {
			if o, ok := r.tests[n]; ok {
				outs = append(outs, o)
			} else if r.TimedOut {
				outs = append(outs, sandbox.Timeout)
			}
		}
		out[n] = sandbox.Classify(outs)
	}
	return out
}

type RuntimeRunIn struct {
	RunID      string `json:"run_id"`
	ProposalID string `json:"proposal_id,omitempty" jsonschema:"confirmed runtime proposal (default: the latest for this head)"`
}

type RuntimeRunOut struct {
	ProposalID string          `json:"proposal_id"`
	Summary    map[string]int  `json:"summary"`
	Commands   []CommandResult `json:"commands" jsonschema:"tests are limited to those whose delta is not pass"`
	Results    string          `json:"results" jsonschema:"run-context path of the full results"`
	Notes      []string        `json:"notes"`
}

func (h *handlers) runtimeRun(ctx context.Context, _ *mcp.CallToolRequest, in RuntimeRunIn) (*mcp.CallToolResult, RuntimeRunOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, RuntimeRunOut{}, err
	}
	p, plan, err := h.allowedPlan(in.RunID, st, in.ProposalID)
	if err != nil {
		return nil, RuntimeRunOut{}, err
	}
	if len(plan.Commands) == 0 {
		return nil, RuntimeRunOut{}, errors.New("this plan has no runtime commands (only static analyzers: use lint_run)")
	}
	if err := h.readySandbox(ctx, plan); err != nil {
		return nil, RuntimeRunOut{}, err
	}
	res := RuntimeResults{ProposalID: p.ID, HeadSHA: st.HeadSHA, MergeBase: st.MergeBase, Image: plan.Image, Network: plan.Network,
		Setup: map[string]*RunRecord{}, SetupFailed: map[string]bool{}, Summary: map[string]int{}, Started: h.now()}
	_, _ = h.logEvent(in.RunID, "runtime_started", "server", map[string]any{"id": p.ID, "head": st.HeadSHA})
	runs := map[string][][]RunRecord{}
	for _, side := range []struct{ name, sha string }{{"base", st.MergeBase}, {"head", st.HeadSHA}} {
		env, setup, cleanup, err := h.prepSide(ctx, in.RunID, st, p, plan, side.name, side.sha)
		if err != nil {
			_, _ = h.logEvent(in.RunID, "runtime_failed", "server", map[string]any{"id": p.ID, "side": side.name, "error": err.Error()})
			return nil, RuntimeRunOut{}, fmt.Errorf("%s side: %w", side.name, err)
		}
		if setup != nil {
			res.Setup[side.name] = setup
			if setup.Exit != 0 || setup.TimedOut {
				res.SetupFailed[side.name] = true
			}
		}
		for i, cmd := range plan.Commands {
			if res.SetupFailed[side.name] {
				runs[side.name] = append(runs[side.name], nil)
				continue
			}
			recs, err := h.runWithReruns(ctx, in.RunID, st, p, plan, env, i, cmd)
			if err != nil {
				cleanup()
				return nil, RuntimeRunOut{}, fmt.Errorf("%s side, command %d: %w", side.name, i+1, err)
			}
			runs[side.name] = append(runs[side.name], recs)
		}
		cleanup()
	}
	metrics := map[string]any{"commands": len(plan.Commands)}
	for i, cmd := range plan.Commands {
		cr := CommandResult{Index: i + 1, Cmd: cmd, TestDeltas: map[string]int{}}
		for _, side := range []string{"base", "head"} {
			sc := SideCommand{Class: sandbox.ClassSetupFailed}
			if !res.SetupFailed[side] {
				sc = SideCommand{Class: sideClass(runs[side][i]), Runs: runs[side][i]}
			}
			if side == "base" {
				cr.Base = sc
			} else {
				cr.Head = sc
			}
		}
		cr.Delta = sandbox.Delta(cr.Base.Class, cr.Head.Class)
		if !res.SetupFailed["base"] && !res.SetupFailed["head"] {
			bt, ht := testClasses(runs["base"][i]), testClasses(runs["head"][i])
			names := map[string]bool{}
			for n := range bt {
				names[n] = true
			}
			for n := range ht {
				names[n] = true
			}
			for n := range names {
				b, hc := bt[n], ht[n]
				if b == "" {
					b = sandbox.ClassAbsent
				}
				if hc == "" {
					hc = sandbox.ClassAbsent
				}
				d := sandbox.Delta(b, hc)
				cr.Tests = append(cr.Tests, TestResult{Name: n, Base: b, Head: hc, Delta: d})
				cr.TestDeltas[d]++
				res.Summary["tests_"+d]++
			}
			sort.Slice(cr.Tests, func(a, b int) bool { return cr.Tests[a].Name < cr.Tests[b].Name })
		}
		res.Summary["commands_"+cr.Delta]++
		k := fmt.Sprintf("cmd%d.", i+1)
		metrics[k+"delta"] = cr.Delta
		metrics[k+"base.class"], metrics[k+"head.class"] = cr.Base.Class, cr.Head.Class
		metrics[k+"head.runs"], metrics[k+"head.failed_runs"] = len(cr.Head.Runs), countFailed(cr.Head.Runs)
		metrics[k+"base.runs"], metrics[k+"base.failed_runs"] = len(cr.Base.Runs), countFailed(cr.Base.Runs)
		res.Commands = append(res.Commands, cr)
	}
	for k, v := range res.Summary {
		metrics[k] = v
	}
	res.Finished = h.now()
	path := h.artifactDir(st, p) + "results.json"
	if err := h.saveJSON(in.RunID, path, res); err != nil {
		return nil, RuntimeRunOut{}, err
	}
	if err := h.saveJSON(in.RunID, "runtime/"+st.HeadSHA+"/latest.json", map[string]string{"proposal_id": p.ID, "results": path}); err != nil {
		return nil, RuntimeRunOut{}, err
	}
	if err := h.recordMetrics(in.RunID, "runtime", metrics); err != nil {
		return nil, RuntimeRunOut{}, err
	}
	_, _ = h.logEvent(in.RunID, "runtime_ran", "server", map[string]any{"id": p.ID, "summary": res.Summary})
	out := RuntimeRunOut{ProposalID: p.ID, Summary: res.Summary, Results: path, Notes: []string{}}
	for _, cr := range res.Commands {
		c := cr
		c.Tests = nil
		for _, t := range cr.Tests {
			if t.Delta != "pass" {
				c.Tests = append(c.Tests, t)
			}
		}
		out.Commands = append(out.Commands, c)
	}
	for side := range res.SetupFailed {
		out.Notes = append(out.Notes, "setup failed on the "+side+" side; see its setup log. Results for that side are not comparable.")
	}
	return nil, out, nil
}

func countFailed(recs []RunRecord) int {
	n := 0
	for _, r := range recs {
		if failed(r) {
			n++
		}
	}
	return n
}

func (h *handlers) readySandbox(ctx context.Context, plan runtimePlan) error {
	if h.cfg.Sandbox == nil {
		return errors.New("no sandbox runner is configured")
	}
	if err := h.cfg.Sandbox.Available(ctx); err != nil {
		return fmt.Errorf("docker is not available: %w", err)
	}
	return h.cfg.Sandbox.EnsureImage(ctx, plan.Image)
}

// ---- runtime_results ----

func (h *handlers) runtimeResults(_ context.Context, _ *mcp.CallToolRequest, in RuntimeRunIn) (*mcp.CallToolResult, RuntimeResults, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, RuntimeResults{}, err
	}
	path := ""
	if in.ProposalID != "" {
		path = "runtime/" + st.HeadSHA + "/" + in.ProposalID + "/results.json"
	} else {
		var latest map[string]string
		if err := h.loadJSON(in.RunID, "runtime/"+st.HeadSHA+"/latest.json", &latest); err != nil || latest["results"] == "" {
			return nil, RuntimeResults{}, errors.New("no runtime results for this head (runtime analysis was not run: report not_applicable or not_run, never guess)")
		}
		path = latest["results"]
	}
	var res RuntimeResults
	if err := h.loadJSON(in.RunID, path, &res); err != nil || res.HeadSHA == "" {
		return nil, RuntimeResults{}, fmt.Errorf("no runtime results at %s", path)
	}
	return nil, res, nil
}

// ---- lint_run ----

type LintOut struct {
	ProposalID string            `json:"proposal_id"`
	Findings   []sandbox.Finding `json:"findings" jsonschema:"findings on lines shown in the diff (at most 200); all findings are in lint/<head>/findings.json"`
	Counts     map[string]int    `json:"counts"`
	Logs       []string          `json:"logs"`
	Notes      []string          `json:"notes"`
}

func (h *handlers) lintRun(ctx context.Context, _ *mcp.CallToolRequest, in RuntimeRunIn) (*mcp.CallToolResult, LintOut, error) {
	st, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, LintOut{}, err
	}
	p, plan, err := h.allowedPlan(in.RunID, st, in.ProposalID)
	if err != nil {
		return nil, LintOut{}, err
	}
	if len(plan.StaticAnalysis) == 0 {
		return nil, LintOut{}, errors.New("no static_analysis commands are configured in the merge-base .aireviewer.yml")
	}
	m, err := h.loadMap(in.RunID)
	if err != nil {
		return nil, LintOut{}, err
	}
	if err := h.readySandbox(ctx, plan); err != nil {
		return nil, LintOut{}, err
	}
	env, setup, cleanup, err := h.prepSide(ctx, in.RunID, st, p, plan, "lint", st.HeadSHA)
	if err != nil {
		return nil, LintOut{}, err
	}
	defer cleanup()
	out := LintOut{ProposalID: p.ID, Findings: []sandbox.Finding{}, Counts: map[string]int{}, Logs: []string{}, Notes: []string{}}
	if setup != nil && (setup.Exit != 0 || setup.TimedOut) {
		out.Notes = append(out.Notes, "setup failed; analyzers ran anyway, results may be incomplete (see "+setup.Log+")")
	}
	var all []sandbox.Finding
	for i, cmd := range plan.StaticAnalysis {
		logPath := fmt.Sprintf("%slint-%d.log", h.artifactDir(st, p), i+1)
		rec, err := h.sandboxExec(ctx, in.RunID, plan, env, cmd, plan.Network, logPath, 1)
		if err != nil {
			return nil, LintOut{}, err
		}
		out.Logs = append(out.Logs, logPath)
		raw, _ := h.cfg.Store.Get(in.RunID, logPath)
		fs := sandbox.ParseLint(raw)
		if rec.TimedOut {
			out.Notes = append(out.Notes, fmt.Sprintf("analyzer %d timed out", i+1))
		} else if rec.Exit != 0 && len(fs) == 0 {
			out.Notes = append(out.Notes, fmt.Sprintf("analyzer %d exited %d without parseable findings; see %s", i+1, rec.Exit, logPath))
		}
		for _, f := range fs {
			f.Command = i + 1
			if l, _ := diffLine(m, f.File, "new", f.Line); l != nil {
				f.InDiff, f.Changed = true, l.Op == "+"
			}
			all = append(all, f)
		}
	}
	if all == nil {
		all = []sandbox.Finding{}
	}
	if err := h.saveJSON(in.RunID, "lint/"+st.HeadSHA+"/findings.json", all); err != nil {
		return nil, LintOut{}, err
	}
	for _, f := range all {
		out.Counts["findings"]++
		if f.InDiff {
			out.Counts["in_diff"]++
			if len(out.Findings) < 200 {
				out.Findings = append(out.Findings, f)
			}
		}
		if f.Changed {
			out.Counts["changed"]++
		}
	}
	metrics := map[string]any{"findings": out.Counts["findings"], "in_diff": out.Counts["in_diff"], "changed": out.Counts["changed"]}
	if err := h.recordMetrics(in.RunID, "lint", metrics); err != nil {
		return nil, LintOut{}, err
	}
	_, _ = h.logEvent(in.RunID, "lint_ran", "server", map[string]any{"id": p.ID, "counts": out.Counts})
	return nil, out, nil
}

// ---- runtime_import ----

type RuntimeImportIn struct {
	RunID     string `json:"run_id" jsonschema:"the run to import into (for example the new PR run after /pr-create)"`
	FromRunID string `json:"from_run_id" jsonschema:"the run whose runtime and lint results to copy (for example the /pr-create branch run)"`
}

type RuntimeImportOut struct {
	Imported []string `json:"imported" jsonschema:"artifacts copied"`
	HeadSHA  string   `json:"head_sha"`
	Note     string   `json:"note,omitempty"`
}

// runtimeImport copies runtime results, logs, lint findings and their
// metrics between two local runs of the same repository, only when both are
// at the exact same head (SPEC §5 staleness rule: runtime results are never
// reused across SHAs).
func (h *handlers) runtimeImport(_ context.Context, _ *mcp.CallToolRequest, in RuntimeImportIn) (*mcp.CallToolResult, RuntimeImportOut, error) {
	if in.RunID == in.FromRunID {
		return nil, RuntimeImportOut{}, errors.New("source and target run are the same")
	}
	dst, err := h.loadCheckout(in.RunID)
	if err != nil {
		return nil, RuntimeImportOut{}, err
	}
	src, err := h.loadCheckout(in.FromRunID)
	if err != nil {
		return nil, RuntimeImportOut{}, err
	}
	if src.Repo != dst.Repo {
		return nil, RuntimeImportOut{}, errors.New("the runs belong to different repositories")
	}
	if src.HeadSHA != dst.HeadSHA {
		return nil, RuntimeImportOut{}, fmt.Errorf("stale: results are for %s but this run is at %s (commits were added after testing, for example accepted fixes); run the sandbox again on this head",
			short(src.HeadSHA), short(dst.HeadSHA))
	}
	files, err := h.cfg.Store.List(in.FromRunID)
	if err != nil {
		return nil, RuntimeImportOut{}, err
	}
	out := RuntimeImportOut{Imported: []string{}, HeadSHA: dst.HeadSHA}
	for _, f := range files {
		if !strings.HasPrefix(f, "runtime/"+src.HeadSHA+"/") && !strings.HasPrefix(f, "lint/"+src.HeadSHA+"/") {
			continue
		}
		b, err := h.cfg.Store.Get(in.FromRunID, f)
		if err != nil {
			return nil, RuntimeImportOut{}, err
		}
		if _, err := h.cfg.Store.Put(in.RunID, f, b); err != nil {
			return nil, RuntimeImportOut{}, err
		}
		out.Imported = append(out.Imported, f)
	}
	if len(out.Imported) == 0 {
		out.Note = "the source run has no runtime or lint results for this head"
		return nil, out, nil
	}
	var mf metricsFile
	if raw, err := h.cfg.Store.Get(in.FromRunID, "metrics.json"); err == nil && json.Unmarshal(raw, &mf) == nil {
		for _, source := range []string{"runtime", "lint"} {
			if kv, ok := mf[source]; ok {
				if err := h.recordMetrics(in.RunID, source, kv); err != nil {
					return nil, RuntimeImportOut{}, err
				}
			}
		}
	}
	_, _ = h.logEvent(in.RunID, "runtime_imported", "server", map[string]any{"from": in.FromRunID, "head": dst.HeadSHA, "files": len(out.Imported)})
	return nil, out, nil
}

func lastLinesOf(b []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func sha256Short(s string) string { return sha(s)[:16] }
