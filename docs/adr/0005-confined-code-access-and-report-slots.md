# ADR 0005: Confined code access and per-dispatch report slots

- Status: accepted
- Date: 2026-11-19
- Supersedes: the worktree location in SPEC §7 (M1)

## Context
A paper-fidelity audit of M1 found three integrity problems:
1. PR worktrees lived inside the user's workspace (`.aireviewer/worktrees/`). PR-controlled files there (a nested `CLAUDE.md`, skill files, symlinks) could be picked up by Claude Code, or followed by filesystem reads. Subagents' Read, Grep and Glob were not confined to the PR either.
2. `run_context_put` could write any path, and the report loader accepted any `.json` under a stage directory, so a subagent could forge an "accepted" report. `report_submit` also trusted the self-declared agent name, so one agent could overwrite another's report.
3. Review policy (`CLAUDE.md`, `CONTRIBUTING.md`, …) was read from the PR head, so a PR could relax the rules it is reviewed against.

## Decision
- Worktrees move to the cache directory, outside the workspace. They exist only to hydrate the blobless mirror's objects in one batch.
- All PR code access goes through git objects at a fixed commit (`git show`, `git grep`, `git ls-tree`): the diff-map source and the new `code_read`, `code_search` and `code_list` tools. Subagents lose Read, Grep and Glob. A symlink in a PR reads as its target string, never as the target file.
- `report_slot` (orchestrator only) issues a random submit token per agent name, stage and head SHA. `report_submit` rejects reports without the matching token. Tokens live in server memory and are never written to disk.
- `run_context_put` only writes `notes/` or `*.md`. The report loader only reads `.json` files in the stage directory, which only `report_submit` can write.
- `policy_get` reads policy files at the merge base, and lists the policy files the PR modifies.
- Metric evidence must match a value recorded by a tool in `metrics.json`. Trace evidence must point at an existing `runtime/` artifact.

## Consequences
- Agents cannot read anything outside the PR's commits, and PR content cannot be loaded as instructions through the filesystem. Prompt injection through PR *text* is still possible and remains mitigated by prompts and the human gates.
- Tokens do not survive a server restart. That is fine, because slots are needed only while subagents are running, and stored reports remain readable.
- The orchestrator could misuse tokens, since it holds all of them. It runs in the user's own session, under the user's supervision.
