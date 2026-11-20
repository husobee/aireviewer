# aireviewer

Agentic pull request / merge request **creation** and **review** for GitHub and GitLab, packaged as a Claude Code plugin (skills + subagents + a Go MCP server).

aireviewer implements the staged, human-gated framework proposed in
*Rethinking Code Review in the Age of AI: A Vision for Agentic Code Review*
(Kamalı, Tuna, Haratian, Tüzün — [arXiv:2605.17548](https://arxiv.org/abs/2605.17548)):

- **`/pr-create [branch]`**: PR detail generation → issue linking → automated review → fix suggestions → **you approve** → PR created → augmentation → reviewer suggestions.
- **`/pr-review <url>`**: augmentation (alignment, risk, impact, runtime) → semantic diff-map → automated review → conversational review with explanation, toxicity and usefulness checks → **you decide and approve every post**.

> **Status: M4 (pre-alpha).** `/pr-create` turns a local branch into a PR/MR: generated title and description, scored issue links (or a drafted issue), an author-mode review with validated fixes, and an interactive revision gate. Nothing is committed, pushed or created until you confirm the exact plan (ADR 0007). `/pr-review <url>` checks out the PR/MR, builds a semantic diff-map, and runs the paper's augmentation stage: alignment with linked issues, history-based risk and change impact. It suggests reviewers, runs parallel code-review subagents whose findings are validated against the real code, and opens an interactive review session. In that session you draft comments (with toxicity and usefulness checks and one-click fix suggestions) and choose a verdict. Nothing is posted until you confirm the exact content, which the server asks you for itself (ADR 0006). See [SPEC.md](SPEC.md) for the design and the §17 milestones.

## Principles

- **Humans hold the gates.** Nothing is pushed, created or posted to a forge without explicit confirmation of the exact content. The agent never chooses the review verdict.
- **Claims carry evidence.** Every finding points at code, a tool-computed metric, or an execution trace, and has a confidence level. Uncertainty and contradictions are shown, not smoothed over.
- **Deterministic work goes to tools.** Diffs, metrics, callers and executions come from code, not from a model.
- **Forge-neutral core, forge-native voice.** "PR" is the internal term. Users see "merge request"/"MR" on GitLab.

## Quick start (development)

Requirements: Go 1.26+ with a C toolchain (cgo for tree-sitter), git ≥ 2.31, Claude Code, and optionally `gh` for GitHub auth. Docker is needed for runtime analysis from M5 onward.

```bash
make build                       # builds bin/aireviewer-mcp
./bin/aireviewer-mcp inspect https://github.com/cli/cli/pull/15094
./bin/aireviewer-mcp diffmap https://gitlab.com/gitlab-org/cli/-/merge_requests/1500
./bin/aireviewer-mcp augment https://github.com/cli/cli/pull/15065   # issues, risk, impact, reviewers (no tokens)
claude --plugin-dir .            # load the plugin from this checkout
# then, inside Claude Code:
/pr-review https://gitlab.com/gitlab-org/cli/-/merge_requests/1500
```

### Authentication

| Forge | Resolution order |
|---|---|
| GitHub / GHES | `GITHUB_TOKEN`, `GH_TOKEN`, then `gh auth token --hostname <host>` |
| GitLab / self-managed | `GITLAB_TOKEN`, then `CI_JOB_TOKEN` (in CI) |

Public projects work unauthenticated, subject to rate limits. Self-hosted hosts that don't contain "github" or "gitlab" in their name must be declared:

```bash
export AIREVIEWER_GITLAB_HOSTS=code.example.com
export AIREVIEWER_GITHUB_HOSTS=ghe.example.com
```

Run artifacts (diff-maps, validated reports, notes) live in `.aireviewer/` in the directory you run Claude Code from, which ignores itself through its own `.gitignore`. Git mirrors and PR checkouts are cached outside the workspace in `$AIREVIEWER_CACHE_DIR` (default `~/.cache/aireviewer`). Agents read PR code only through git objects (ADR 0005), so PR files are never loaded as instructions.

## Repository layout

```
cmd/aireviewer-mcp/   MCP server entrypoint (stdio) + `inspect` debug command
internal/forge/       forge-neutral model, URL/remote parsing, GitHub & GitLab adapters
internal/gitrepo/     cached mirrors, PR worktrees, merge base
internal/diffmap/     semantic diff-map (tree-sitter)
internal/schema/      report schema + evidence validation
internal/history/     git-history signals, risk score, expertise
internal/reviewers/   CODEOWNERS + deterministic reviewer suggestion
internal/config/      .aireviewer.yml (read from the merge base)
internal/providers/   provider construction & credential resolution
internal/runctx/      .aireviewer/runs store (cross-stage context)
internal/mcpserver/   MCP tool registration
skills/               Claude Code skills (orchestrators)
agents/               Claude Code subagents (from M1)
docs/                 architecture notes and ADRs
```

## Contributing

Contributions are welcome. Read [CONTRIBUTING.md](CONTRIBUTING.md); commits must be signed off under the [DCO](https://developercertificate.org/). Report security issues as described in [SECURITY.md](SECURITY.md).

## Citation

If you use aireviewer in research, cite both the software ([CITATION.cff](CITATION.cff)) and the paper it implements.

## License

[MIT](LICENSE)
