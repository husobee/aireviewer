// SPDX-License-Identifier: MIT

package mcpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/husobee/aireviewer/internal/sandbox"
)

// TestRuntimeDocker is the M5 exit criterion against a real Docker daemon:
// a seeded flaky test is classified flaky, a seeded regression
// deterministic_fail / new_failure, and the isolation holds from inside the
// container. Run with AIREVIEWER_DOCKER_TESTS=1 (pulls alpine).
func TestRuntimeDocker(t *testing.T) {
	if os.Getenv("AIREVIEWER_DOCKER_TESTS") != "1" {
		t.Skip("set AIREVIEWER_DOCKER_TESTS=1 to run against Docker")
	}
	yml := `runtime:
  enabled: true
  image: alpine:3.20
  commands:
    - "sh flaky.sh"
    - "sh broken.sh"
    - "test \"$(id -u)\" = 65534 && ! touch /rootfs-write 2>/dev/null && ! wget -q -T 3 -O /dev/null http://1.1.1.1"
  reruns: 3
  timeout_s: 60
static_analysis: ["sh lint.sh"]
`
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	src := t.TempDir()
	write := func(files map[string]string) {
		for p, c := range files {
			_ = os.MkdirAll(filepath.Join(src, filepath.Dir(p)), 0o755)
			if err := os.WriteFile(filepath.Join(src, p), []byte(c), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	gitT(t, src, "init", "-q", "-b", "main")
	write(map[string]string{".aireviewer.yml": yml, "svc/svc.go": svcBase, "flaky.sh": "exit 0\n", "broken.sh": "exit 0\n",
		"lint.sh": "echo 'svc/svc.go:4:2: loop has an empty body'\n"})
	gitT(t, src, "add", "-A")
	gitT(t, src, "commit", "-q", "-m", "base")
	gitT(t, src, "checkout", "-q", "-b", "feat")
	write(map[string]string{
		// Fails on its first run only: state lives in the run's work volume.
		"flaky.sh":        "n=$(cat .attempts 2>/dev/null || echo 0); n=$((n+1)); echo $n > .attempts; echo attempt $n; [ \"$n\" -ne 1 ]\n",
		"broken.sh":       "echo boom >&2; exit 1\n",
		"svc/svc.go":      svcHead,
		".aireviewer.yml": strings.Replace(yml, "reruns: 3", "reruns: 3\n  network: true", 1),
	})
	gitT(t, src, "commit", "-q", "-am", "head")
	head := gitT(t, src, "rev-parse", "HEAD")
	gitT(t, src, "update-ref", "refs/pull/3/head", head)
	gitT(t, src, "checkout", "-q", "main")

	cs := runtimeServer(t, src, head, sandbox.Docker{})
	run := call[CheckoutOut](t, cs, "pr_checkout", map[string]any{"url": "https://github.com/o/r/pull/3"}).RunID
	call[DiffmapOut](t, cs, "diffmap_build", map[string]any{"run_id": run})
	prep := call[RuntimePrepareOut](t, cs, "runtime_prepare", map[string]any{"run_id": run})
	if !prep.Applicable {
		t.Fatalf("not applicable: %s", prep.Reason)
	}
	call[SubmitProposalOut](t, cs, "proposal_submit", map[string]any{"run_id": run, "proposal_id": prep.Proposal.ProposalID, "hash": prep.Proposal.Hash})
	rr := call[RuntimeRunOut](t, cs, "runtime_run", map[string]any{"run_id": run})
	got := []string{rr.Commands[0].Delta, rr.Commands[1].Delta, rr.Commands[2].Delta}
	want := []string{"flaky", "new_failure", "pass"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("command %d delta = %s, want %s (head %s, base %s)", i+1, got[i], want[i], rr.Commands[i].Head.Class, rr.Commands[i].Base.Class)
		}
	}
	if rr.Commands[1].Head.Class != sandbox.ClassFail || len(rr.Commands[1].Head.Runs) != 4 {
		t.Errorf("regression = %+v", rr.Commands[1].Head)
	}
	res := call[RuntimeResults](t, cs, "runtime_results", map[string]any{"run_id": run})
	if res.Commands[2].Head.Class != sandbox.ClassPass {
		t.Errorf("isolation check failed inside the container; see %s", res.Commands[2].Head.Runs[0].Log)
	}
	lo := call[LintOut](t, cs, "lint_run", map[string]any{"run_id": run})
	if lo.Counts["changed"] != 1 {
		t.Errorf("lint = %+v", lo)
	}
}
