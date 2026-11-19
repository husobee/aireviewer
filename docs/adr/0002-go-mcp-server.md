# ADR 0002: A single Go MCP server with forge adapters

- Status: accepted
- Date: 2026-11-19

## Context
The system must support GitHub and GitLab, including self-hosted instances. Existing per-forge MCP servers expose different tool names and data shapes. Skills would have to branch on forge, and this matters most for inline comment positions and review submission.

## Decision
Ship one MCP server, `aireviewer-mcp`, written in Go, with a forge-neutral `forge.Provider` interface and one adapter per forge (go-github, gitlab client-go). Deterministic analysis tools (diff-map, metrics, symbol refs, sandbox) live in the same binary.

## Consequences
- Skills and agents are forge-agnostic. "PR" is the internal term, and user-facing text uses `Kind.Noun()` / `Abbrev()`.
- The server is a single binary per platform (cgo, see ADR 0004), built on native CI runners and easy to ship in the plugin and in CI images.
- Adding a forge means implementing `Provider` (see CONTRIBUTING.md).
