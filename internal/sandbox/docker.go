// SPDX-License-Identifier: MIT

// Package sandbox runs untrusted PR code in Docker containers for runtime
// analysis and static analyzers (SPEC §10 runtime-analyst, §15, ADR 0008).
//
// Every container drops all capabilities, cannot gain privileges, has a
// read-only root filesystem, a non-root user (except the copy step), no
// network unless the repository's merge-base config enables it, and CPU,
// memory, process and wall-clock limits. The code is a git-archive copy,
// mounted read-only and copied into a per-run volume; no host paths other
// than that copy and no credentials are passed in.
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// Mount is a bind mount (Source is a host path) or a named volume.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
	Volume   bool
}

// Spec describes one container run.
type Spec struct {
	Name      string
	Image     string
	Cmd       string // run with /bin/sh -c
	Workdir   string
	User      string // empty = image default (root); used only for the copy step
	Network   bool
	Mounts    []Mount
	Env       map[string]string
	CapAdd    []string
	Timeout   time.Duration
	Memory    string
	CPUs      string
	PidsLimit int
	MaxOutput int // bytes of combined output kept (the tail); default 1 MiB
}

// Result is the outcome of one run.
type Result struct {
	Exit      int           `json:"exit"`
	TimedOut  bool          `json:"timed_out"`
	Duration  time.Duration `json:"duration_ns"`
	Output    []byte        `json:"-"`
	Truncated bool          `json:"truncated"`
}

// Runner executes containers. Docker implements it; tests use fakes.
type Runner interface {
	Available(ctx context.Context) error
	EnsureImage(ctx context.Context, image string) error
	CreateVolume(ctx context.Context, name string) error
	RemoveVolume(ctx context.Context, name string)
	Run(ctx context.Context, s Spec) (Result, error)
}

// Docker runs containers with the docker CLI.
type Docker struct {
	Bin string // default "docker"
}

func (d Docker) bin() string {
	if d.Bin == "" {
		return "docker"
	}
	return d.Bin
}

func (d Docker) cli(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, d.bin(), args...) //nolint:gosec // fixed binary, argv built internally
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("docker %s: %w: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// Available reports whether the Docker daemon can be reached.
func (d Docker) Available(ctx context.Context) error {
	_, err := d.cli(ctx, 15*time.Second, "version", "--format", "{{.Server.Version}}")
	return err
}

// EnsureImage pulls the image unless it is present locally.
func (d Docker) EnsureImage(ctx context.Context, image string) error {
	if _, err := d.cli(ctx, 30*time.Second, "image", "inspect", "--format", "{{.Id}}", "--", image); err == nil {
		return nil
	}
	_, err := d.cli(ctx, 20*time.Minute, "pull", "--quiet", "--", image)
	return err
}

// CreateVolume creates a labelled named volume.
func (d Docker) CreateVolume(ctx context.Context, name string) error {
	_, err := d.cli(ctx, time.Minute, "volume", "create", "--label", "aireviewer=1", "--", name)
	return err
}

// RemoveVolume removes a volume, ignoring errors.
func (d Docker) RemoveVolume(ctx context.Context, name string) {
	_, _ = d.cli(context.WithoutCancel(ctx), time.Minute, "volume", "rm", "--force", "--", name)
}

// Args returns the docker CLI arguments for s (exported for tests and audit).
func Args(s Spec) []string {
	a := []string{"run", "--rm", "--name", s.Name, "--label", "aireviewer=1",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--read-only",
		"--tmpfs", "/tmp:rw,exec,nosuid,size=1g"}
	if s.Network {
		a = append(a, "--network", "bridge")
	} else {
		a = append(a, "--network", "none")
	}
	for _, c := range s.CapAdd {
		a = append(a, "--cap-add", c)
	}
	pids := s.PidsLimit
	if pids <= 0 {
		pids = 512
	}
	a = append(a, "--pids-limit", fmt.Sprint(pids))
	if s.Memory != "" {
		a = append(a, "--memory", s.Memory, "--memory-swap", s.Memory)
	}
	if s.CPUs != "" {
		a = append(a, "--cpus", s.CPUs)
	}
	if s.User != "" {
		a = append(a, "--user", s.User)
	}
	if s.Workdir != "" {
		a = append(a, "--workdir", s.Workdir)
	}
	keys := make([]string, 0, len(s.Env))
	for k := range s.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a = append(a, "--env", k+"="+s.Env[k])
	}
	for _, m := range s.Mounts {
		t := "bind"
		if m.Volume {
			t = "volume"
		}
		v := "type=" + t + ",source=" + m.Source + ",target=" + m.Target
		if m.ReadOnly {
			v += ",readonly"
		}
		a = append(a, "--mount", v)
	}
	return append(a, "--entrypoint", "/bin/sh", s.Image, "-c", s.Cmd)
}

// tail keeps the last n bytes written.
type tail struct {
	mu        sync.Mutex
	n         int
	buf       []byte
	truncated bool
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.n {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.n:]...)
		t.truncated = true
	}
	return len(p), nil
}

// Run executes the container. A non-zero exit of the command is a Result,
// not an error; errors are reserved for Docker itself failing (exit 125).
func (d Docker) Run(ctx context.Context, s Spec) (Result, error) {
	if s.Name == "" || s.Image == "" {
		return Result{}, errors.New("sandbox: name and image are required")
	}
	limit := s.MaxOutput
	if limit <= 0 {
		limit = 1 << 20
	}
	out := &tail{n: limit}
	cmd := exec.Command(d.bin(), Args(s)...) //nolint:gosec,noctx // argv built internally; lifetime handled with docker kill below
	cmd.Stdout, cmd.Stderr = out, out
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var res Result
	var werr error
	select {
	case werr = <-done:
	case <-timer.C:
		res.TimedOut = true
		_, _ = d.cli(context.WithoutCancel(ctx), time.Minute, "kill", s.Name)
		werr = <-done
	case <-ctx.Done():
		_, _ = d.cli(context.WithoutCancel(ctx), time.Minute, "kill", s.Name)
		<-done
		return Result{}, ctx.Err()
	}
	res.Duration = time.Since(start)
	res.Output, res.Truncated = out.buf, out.truncated
	if werr != nil {
		var ee *exec.ExitError
		if !errors.As(werr, &ee) {
			return res, werr
		}
		res.Exit = ee.ExitCode()
		if res.Exit == 125 && !res.TimedOut {
			return res, fmt.Errorf("docker could not start the container: %s", strings.TrimSpace(lastLines(out.buf, 5)))
		}
	}
	return res, nil
}

func lastLines(b []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
