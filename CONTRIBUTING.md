# Contributing to aireviewer

Thanks for helping. This document covers setup, conventions, and how changes get merged.

## Ground rules

- Be kind. Participation is governed by our [Code of Conduct](CODE_OF_CONDUCT.md).
- Talk before large changes. Open an issue (or comment on a milestone issue) before starting anything that changes the design in [SPEC.md](SPEC.md). Design changes are recorded as ADRs in `docs/adr/`.
- Report security issues privately ([SECURITY.md](SECURITY.md)), never in public issues.

## Developer Certificate of Origin

All commits must be signed off, certifying the [DCO](https://developercertificate.org/):

```bash
git commit -s -m "forge/gitlab: handle collapsed diffs"
```

CI rejects commits without a `Signed-off-by:` trailer. There is no CLA.

## Setup

```bash
go version            # 1.26+
make check            # fmt, vet, lint, SPDX headers, tests
make build            # bin/aireviewer-mcp
claude --plugin-dir . # try the plugin locally
```

## Conventions

- **License headers.** Every source file starts with `SPDX-License-Identifier: MIT` in the language's comment syntax. `make spdx` checks this.
- **Commits.** Use [Conventional Commits](https://www.conventionalcommits.org/) scoped by area, e.g. `feat(forge/github): …`, `fix(runctx): …`, `docs(spec): …`. The changelog is generated from them.
- **Terminology.** In code and docs, "PR" is the neutral term for a GitHub pull request or a GitLab merge request. Anything shown to users must use the forge-native word (`forge.Kind.Noun()` / `Abbrev()`).
- **Forge writes.** Any new tool that writes to a forge must be marked as such in its MCP annotations, must never be given to a subagent, and must only be called by a skill after an explicit human confirmation step. PRs that weaken a gate will not be merged.
- **Determinism.** Facts (metrics, locations, callers, execution results) come from tools in `internal/`. Agents interpret them.
- **Tests.**
  - Unit tests make no network calls.
  - Forge adapters are tested against `httptest` fixtures.
  - Agent prompt fixtures (from M1) live in `testdata/agent-fixtures/` and run nightly because they cost tokens.
- **Dependencies.** Permissive licenses only: MIT, BSD, Apache-2.0 or ISC. MPL-2.0 is accepted for **unmodified** transitive dependencies; today that means `hashicorp/go-retryablehttp` and `go-cleanhttp`, which come in through the GitLab client. CI (`make licenses`) fails on GPL/AGPL and other restricted licenses.

## Pull requests

1. Fork and branch from `main`.
2. Keep each PR focused. Include tests and update docs and SPEC.md when behaviour changes.
3. `make check` must pass.
4. A maintainer reviews. We dogfood aireviewer on our own PRs once `/pr-review` is useful (M1+).

## Adding a forge

Implement `forge.Provider` in `internal/forge/<name>/`, register it in `internal/providers`, add URL/remote parsing cases, and add fixture tests. Bitbucket, Gitea/Forgejo and Azure Repos adapters are welcome; open an issue using the *forge support* template first.
