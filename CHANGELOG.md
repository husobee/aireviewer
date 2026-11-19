# Changelog

All notable changes to this project are documented here. The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses [Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added
- M0 scaffold: Claude Code plugin manifest, marketplace manifest, and MCP launcher.
- `aireviewer-mcp` server (Go, stdio) with the `forge_detect`, `pr_resolve`, `pr_get`, `pr_diff`, `run_context_get`, `run_context_put` and `run_context_list` tools.
- GitHub (incl. GHES) and GitLab (incl. self-managed) read adapters behind a forge-neutral `Provider` interface.
- Local run-context store under `.aireviewer/runs/`.
- `aireviewer-mcp inspect <url>` debugging command.
- Read-only `/pr-review` skill (metadata and diff stats).
- Agent report schema v1 (`skills/_shared/schema.json`).
