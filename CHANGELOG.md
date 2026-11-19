# Changelog

All notable changes to this project are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

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
