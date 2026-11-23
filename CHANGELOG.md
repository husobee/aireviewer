# Changelog

All notable changes to this project are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-11-23

First public release (pre-alpha). It contains every milestone below.

### Added (§21 slice 1: change sets; ADR 0014)
- `/pr-review <url> <url> …`: review 2–8 PRs (any mix of repositories and forges) as one change.
  - **Set panel:** merge order, blocking problems, contracts, union alignment, and per-PR notes.
  - **Per member:** each member gets its usual per-PR review.
- Tools:
  - `changeset_open`;
  - `changeset_get` (staleness over every member's head);
  - `changeset_discover` (suggestions: stacked and referenced PRs);
  - `changeset_map` (cross-PR symbol edges with repo-qualified unit ids);
  - `changeset_order` (stacked, introduces and removal constraints; contracts; a merge sequence; incompatible, cycle and merged-out-of-order problems).
- `changeset-analyst` agent (Opus) and the `changeset` report stage. In a change-set report, code evidence names its member run (`member`), and anchors are repo-qualified.
- Forge adapters: open PRs by target branch, for stack discovery.

### Security
- `run_context_put` also refuses `retro/`, `changeset/` and `changeset-data/`.

### Added (M6d: issue index v2, BM25; ADR 0013)
- `issues_index_sync`: a per-repository SQLite FTS5 issue index in the cache directory. Ranking is BM25 with porter stemming, weighting titles above labels above bodies. The first sync is full; later ones fetch only issues updated since the last sync.
- `issues_search` uses the index when present (`source: index`, `bm25` scores) and falls back to the forge search. Query terms are quoted, so FTS syntax cannot be injected.
- Forge adapters: incremental issue listing on GitHub (`since`) and GitLab (`updated_after`).
- `/pr-create` syncs the index before issue linking.
- Dependency: `modernc.org/sqlite` (pure Go, BSD-3-Clause).

### Added (M6c: evaluation harness and feedback loop; ADR 0012)
- `aireviewer-mcp evaluate run|score|bias`:
  - golden-case scoring (precision as a lower bound, recall, F1, false-positive traps, alignment and risk agreement, broken down by category);
  - end-to-end runs through `claude -p` in eval mode, with tokens, cost and duration per case, a `-max-cost` budget, and `-baseline` regression checks that exit non-zero;
  - an automation-bias audit listing.
- Eval mode (`AIREVIEWER_EVAL=1`): the full analysis runs, and the server refuses every write.
- `/pr-review --eval [--author-mode]`.
- `feedback_suggest`: configuration suggestions from patterns across saved retrospectives, never applied automatically. Retrospective memory now records disputes by category, dispute reasons and flag rates. `/pr-retro` presents the suggestions.
- Golden cases from the sandbox repository, plus `evaluation/README.md`.

### Added (M6b: headless CI; ADR 0011)
- Headless mode (`AIREVIEWER_HEADLESS=1`), enforced by the server. `proposal_submit` posts only the augmentation summary, and only when the merge-base config sets `ci.post_augmentation: true`. Runtime, reviews, reviewer requests, creation, and voice and memory writes are refused. CI summaries carry a footer saying no human reviewed them.
- `/pr-review --headless` (augmentation only) and `/pr-retro --headless` (job-log summary, nothing posted or saved).
- `ci/`: a Docker image (Claude Code, the plugin and a prebuilt server), the `aireviewer-ci` wrapper, a GitHub Actions workflow and a GitLab CI template, with a setup guide.
- Tests for the review-activity adapters on both forges.

### Added (M6a: retrospectives, memory, bug labels; ADR 0010)
- `/pr-retro <url>`:
  - deterministic `review_metrics` (time to first review, decision latency, review rounds, commits after review started, comments by origin, AI findings posted and disputed, check-flag rates, overrides);
  - a `retro-summarizer` report;
  - an optional final PR comment (`retro_prepare`, gated);
  - optional local memory.
- Local memory (`memory_save`, `memory_query`, `memory_forget`) under the user's data directory, honouring `memory.enabled` / `memory.retention_days`. The code reviewer, the explainer and `/pr-review` can query past decisions as untrusted context.
- `risk.bug_labels`: bug-labelled merged PRs/MRs count as fixes alongside keywords (GitHub search OR, GitLab per label), cached daily. `repo_metrics` reports fixes by keyword, by label, and both, falling back to keywords when the forge is unavailable.
- Forge adapters: review activity (reviews, approvals, comments, commits) and labelled-PR commits on both forges.

### Added (M5.1: reviewer voice; ADR 0009)
- `voice_get`, `voice_set`, `voice_clear`: a personal voice profile (description plus up to 5 example comments) stored in the reviewer's configuration directory, combined with an optional team default (`comments.voice`, read from the merge base). The personal voice takes precedence.
- `/pr-review` words the comments and review bodies it proposes in the voice. The toxicity and usefulness agents use it only to word their rewrites, never for verdicts. The reviewer's own words are never restyled.
- Saving a voice is never pre-approved: the reviewer approves the exact text.

### Added (M5: sandboxed runtime analysis; ADR 0008)
- `runtime_prepare` → confirmed `runtime` proposal → `runtime_run`. Each command runs on the merge base and the head in Docker, and failures are re-run `runtime.reruns` times. Results are classified per test (`go test -json`) or per command as `pass`, `deterministic_fail`, `flaky` or `timeout`, then compared base vs head (`new_failure`, `fixed`, `still_failing`, `flaky`, …). Logs are `runtime/` trace artifacts and counts are metrics.
- Container isolation:
  - a `git archive` copy of the code;
  - non-root (UID 65534), all capabilities dropped, `no-new-privileges`, read-only root filesystem;
  - no network by default (`runtime.network`, `runtime.setup_network`);
  - CPU, memory, process and time limits;
  - no credentials;
  - volumes removed after each run.
- `lint_run`: configured `static_analysis` commands in the sandbox, findings normalised (golangci-lint JSON or `file:line:col: message`) and marked `in_diff` / `changed` for the code reviewer.
- On-demand runs ("run X base vs head") through the same confirmation, and `runtime_results`.
- Agent: `runtime-analyst` (Sonnet). The summary generator, explainer and code reviewer now use runtime and analyzer results.
- `/pr-review` (`--no-runtime`) and `/pr-create` run the sandbox steps after confirmation.
- `runtime_import`: `/pr-create` carries the branch run's runtime and lint results into the new PR run when the head is unchanged. If fixes were committed after testing, it refuses as stale and offers a re-run on the final head.
- Review planning uses augmentation: `repo_metrics` and `symbol_refs` run before `review_plan`, which returns risk-ordered batches (`risk_ordered`) and per-unit `signals` (file risk, hotspot percentile, recent bug fixes, caller counts) passed to each code reviewer.
- Config: `runtime.setup_network`, `runtime.memory`, `runtime.cpus`, `runtime.env`.

### Security
- `run_context_put` can no longer write into tool-owned directories (`runtime/`, `lint/`, `review/`, `augmentation/`, `create/`, `proposals/`, `sandbox/`), so a note cannot pose as an execution log or gate state.

### Added (M4: PR creation; ADR 0007)
- `/pr-create [branch] [--base] [--issue] [--draft] [--fast]`: preflight (skips creation when an open PR exists), detail generation from the diff, commits and the repo template, issue linking with 0–1 scores and a drafted issue when none fits, an author-mode code review, validated fix patches, Gate 1 revision, then one confirmed submission, followed by augmentation and reviewer suggestions on the new PR.
- Branch runs: `branch_checkout` fetches the local branch into the mirror, so every review-side tool works before a PR exists.
- Tools: `create_context`, `issues_search`, `issue_get`, `create_draft_get`, `create_draft_update`, `create_prepare`.
- `proposal_submit` for `create` proposals runs, in order, with every step recorded so a retry resumes without duplicates:
  1. commit the accepted fixes (private-index plumbing; fast-forwards a checked-out branch; honours `commit.gpgSign`);
  2. create the issue;
  3. push with the author's credentials;
  4. open the PR.
- Forge adapters (GitHub, GitLab): default branch, open PR by branch, issue search and lookup, issue creation, PR/MR creation (labels; GitLab draft prefix).
- Agents: `pr-detail-generator` and `issue-linker` (Sonnet).
- Config: `issue_link.threshold` / `include_closed_days` / `allow_create` / `max_candidates` (SPEC §13), `create.signoff`.

### Fixed
- The confirmation dialog's **Accept** now confirms on its own; declines record their reason (found in the first live posting test).

### Added (M3: gated posting; ADR 0006)
- Forge writes on both GitHub and GitLab:
  - batched reviews with inline comments and suggestion blocks;
  - reviewer requests;
  - a sticky analysis comment;
  - current-user identity.

  On GitLab, reviews post as draft notes that are bulk-published, then approved.
- Two-phase gate: `review_prepare`, `reviewers_prepare` and `sticky_prepare` freeze the exact content and return a preview and a hash. `proposal_submit` posts it only with the matching hash, on an unchanged head, after the **server asks the human** through MCP elicitation (SEP-2322 multi round-trip). The fallback is the client's permission prompt; `AIREVIEWER_STRICT_GATE=1` requires elicitation.
- Comment pipeline: server-side drafts validated against diff positions; toxicity and usefulness checks bound to the comment text; fix suggestions that must apply to the head and become suggestion blocks only after reviewer approval.
- Paper §5 mitigations:
  - **circuit breaker:** low-confidence or disputed findings need a logged override to back a posted comment;
  - **hash-chained decision log** with its head hash anchored in posted footers;
  - **reviewer-first mode:** findings stay hidden until every unit has been viewed;
  - optional **PR Critique** checkpoint;
  - **self-review refusal.**
- `augmentation_import`: re-uses a posted summary only from the authenticated user or trusted bots, for the same head, and re-validates every report.
- Agents: `comment-toxicity` and `comment-usefulness` (Haiku), and `fix-suggester` (Opus).
- `/pr-review`: drafting, checks, fixes, disputes, Gate 2, posting the summary, requesting reviewers, and `log`.
- Config: `comments.toxicity` / `comments.usefulness` (`off | warn | block`), `comments.provenance_footer`, `review.require_critique` / `review.reviewer_first` / `review.low_confidence_breaker`, `gate.trusted_bots`.

### Added (M2: PR augmentation)
- `pr_issues`: linked issues from GitHub `closingIssuesReferences` (GraphQL) or GitLab `closes_issues`, plus closing keywords in the PR body.
- `repo_metrics`: deterministic history signals per changed file (commits over 30/90/365 days, bug-fix commits, authors, hotspot percentile), and a reproducible risk score with a published formula and explicit confidence. It is anchored to the merge base and works on blobless mirrors.
- `symbol_refs`: references and fan-in of changed symbols, dangling references to deleted symbols, and changed interface files. Unexported Go identifiers are package-scoped.
- `reviewers_suggest`: a deterministic reviewer suggestion (no LLM). It uses CODEOWNERS (including GitLab sections), recency-weighted git expertise, live workload from GitHub/GitLab, risk-aware weighting, GitHub login mapping, and a labelled email-local-part heuristic for GitLab.
- `.aireviewer.yml` is parsed, and always read from the merge base.
- Report `facets` for structured analyst results, and an `issue` evidence kind with a verbatim-quote check.
- Agents: `alignment-analyst`, `risk-analyst` and `impact-analyst`. `summary-generator` returns to the paper's role (augmentation synthesis) and has a separate `findings-digest` mode.
- `/pr-review` runs augmentation and the code review in parallel, suggests reviewers when none are assigned, leads the panel with the augmentation summary, and orders the diff-map by risk.
- `aireviewer-mcp augment <url>`: debugging command for the deterministic augmentation tools.
- The code reviewer and explainer now receive the linked-issue context (paper §4.2.3, §4.5.1).

### Changed
- The sticky analysis comment and reviewer requests moved from M2 to M3 (gated writes).

### Security / hardening (paper-fidelity audit, ADR 0005)
- PR worktrees moved out of the workspace and into the cache. All code access reads git objects at a fixed commit through the new `code_read`, `code_search` and `code_list` tools. Subagents no longer get Read, Grep or Glob.
- Per-dispatch submit tokens (`report_slot`): `report_submit` rejects reports without the matching token, so agents cannot forge or overwrite each other's reports.
- `run_context_put` restricted to `notes/` and `*.md`. Only `report_submit` output is loaded as a report.
- `policy_get`: review policy is read from the merge base, and PR changes to policy files are flagged.
- Metric evidence must match the value a tool recorded. Trace evidence must be a `runtime/` artifact.
- `review_plan`: deterministic batching plus the paper's token-limit gate. Above the threshold a human chooses: narrow the scope, ask for a split, or proceed.
- `code-reviewer` reports style findings in author mode (paper §4.2.3). The summary is labelled a findings digest until the M2 analysts exist.
- SPEC corrections: wrong "concurrently" attribution, strict per-SHA runtime reuse, re-entry wording, more deviations listed in §18, and §5 mitigations (circuit breaker, decision log, PR Critique checkpoint, reviewer-first mode, evaluation harness) scheduled into M3 and M6.

### Added (review walkthrough)
- Highlighted diffs for the interactive review. `finding_get` renders a finding with a diff excerpt around each piece of code evidence, falling back to the file itself for unchanged code. `diffmap_unit` gains a `highlighted` view, and `findings_list` lists findings in severity order. In both views, cited lines get a `▶` gutter and the finding is shown inline as a coloured `@@ ▲ …` line inside a ```diff block.
- `/pr-review` gains `walk` / `next finding` / `F3`, and always prints the server-rendered blocks verbatim.
- `scripts/mcp.sh` rebuilds the server whenever sources are newer than the binary.

### Added (M1)
- `pr_checkout`: cached blobless mirror per project plus a per-run worktree at the PR head. It falls back to fetching by SHA when the forge has pruned PR refs, computes the merge base against the forge-recorded base SHA (so merged PRs work), passes tokens via env rather than argv, and is immune to `url.*.insteadOf` rewrites.
- Semantic diff-map (`diffmap_build`, `diffmap_get`, `diffmap_unit`) using tree-sitter for Go, Python, JavaScript, TypeScript/TSX, Java, Rust and Ruby, with config/doc/hunk fallbacks.
- Report validation (`report_submit`, `reports_get`, `evidence_check`, `report_schema`): schema checks plus semantic evidence checks that drop claims citing non-existent files or lines, invented metrics, or missing artifacts.
- Subagents: `code-reviewer`, `summary-generator`, `explainer`.
- `/pr-review` M1: parallel batched review, evidence-backed panel, and an interactive Q&A loop.
- `aireviewer-mcp diffmap <url>` debugging command.
- ADR 0004 (tree-sitter via cgo).

### Changed
- Releases are built natively per platform (cgo) instead of with goreleaser.

### Added (M0)
- M0 scaffold: Claude Code plugin manifest, marketplace manifest, and MCP launcher.
- `aireviewer-mcp` server (Go, stdio) with the `forge_detect`, `pr_resolve`, `pr_get`, `pr_diff`, `run_context_get`, `run_context_put` and `run_context_list` tools.
- GitHub (incl. GHES) and GitLab (incl. self-managed) read adapters behind a forge-neutral `Provider` interface.
- Local run-context store under `.aireviewer/runs/`.
- `aireviewer-mcp inspect <url>` debugging command.
- Read-only `/pr-review` skill (metadata and diff stats).
- Agent report schema v1 (`skills/_shared/schema.json`).
