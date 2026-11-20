# ADR 0008: Sandboxed runtime analysis and static analyzers

- Status: accepted
- Date: 2026-11-20
- Implements: SPEC §10 (runtime-analyst, paper §4.3.4), §8 step 4 (static analyzers) and §15

## Context
Runtime analysis executes the PR's code: its tests, build scripts and anything they invoke. For a PR under review, that is third-party code running on the reviewer's machine. Static analyzers can also run repository code (for example, ESLint configs are JavaScript). The paper requires that failures are re-run and classified before any claim is made, so flaky tests are not reported as regressions.

## Decision
1. **Running code is a gated action.**
   - `runtime_prepare` freezes the exact plan as a `runtime` proposal: image, setup, commands, analyzers, network settings, limits, and the head and merge-base commits.
   - The preview says whose code runs ("@author's PR head (third-party code)" or "your branch").
   - Nothing executes until the human confirms it through `proposal_submit`, with the same elicitation, permission-prompt fallback and strict mode as forge writes (ADR 0006).
   - `runtime_run` and `lint_run` execute only a confirmed, intact plan for the current head.
2. **The plan comes from the merge base.** Image, commands, network and limits are read from the merge-base `.aireviewer.yml`, so a PR cannot change what runs or switch the network on.
   - The reviewer may ask for on-demand commands. These replace the configured commands but keep the merge-base image, limits and network, and the preview marks them as on-demand.
3. **Container isolation.**
   - **Code copy.** The code is a `git archive` of the commit, never a worktree or the author's checkout. It is bind-mounted read-only and copied into a per-run Docker volume by a copy step, the only step that runs as root (with `CHOWN` as its only capability and no network).
   - **Process restrictions.** Every other step runs as UID 65534 with all capabilities dropped, `no-new-privileges`, a read-only root filesystem, a 1 GiB `/tmp` tmpfs, 512 processes, the configured memory (no swap) and CPU limits, and a wall-clock timeout per command enforced with `docker kill`.
   - **Network.** Off unless `runtime.network` is set. `runtime.setup_network` can enable it for setup only, for dependency downloads.
   - **Nothing else passed in.** No credentials, Docker socket or host paths other than the code copy. Tool caches point at a separate cache volume.
   - **Cleanup.** Volumes and copies are removed after each run.
4. **Classification before claims.**
   - Each command runs on the merge base and the head. Anything that fails is re-run `runtime.reruns` times (default 3).
   - Per test (`go test -json`) or per command, the attempts are classified `pass`, `deterministic_fail`, `flaky`, `timeout` or `absent`.
   - Base and head are then compared: `new_failure`, `fixed`, `still_failing`, `flaky`, `new_timeout`, `removed`, `new_passing`, or `not_comparable` when setup failed.
5. **Evidence.**
   - Every execution log is a `runtime/` artifact (trace evidence), with a header recording side, commit, attempt, exit code and duration.
   - Classifications and counts are recorded as metrics (source `runtime`); analyzer counts use source `lint`.
   - `run_context_put` can no longer write into `runtime/` or any other tool-owned directory, so a note cannot pose as a log.
   - Analyzer findings are normalised: golangci-lint JSON, or the common `file:line[:col]: message` form. They are marked `in_diff` / `changed` against the diff-map and given to the code reviewer as leads to confirm, not as findings.

## Consequences
- Runtime analysis is opt-in per repository (`runtime.enabled`, `runtime.image`) and per run (the confirmation). Without either, or without Docker, the runtime-analyst reports `not_applicable` with the reason, as the spec requires.
- Docker isolation is not a VM. A container escape through a kernel or runtime vulnerability remains possible; reviewers can decline to run untrusted code, and gVisor or a rootless daemon can be configured underneath Docker.
- An image that is not pinned by digest can change between runs; the preview warns about it.
- Per-test classification currently understands `go test -json`; other test runners are classified per command until parsers are added.
