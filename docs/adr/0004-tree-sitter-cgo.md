# ADR 0004: Official tree-sitter bindings (cgo) for the diff-map

- Status: accepted
- Date: 2026-11-19

## Context
The semantic diff-map (SPEC.md §7) needs accurate definition boundaries (functions, methods, classes) across languages. The options were:
1. the official `tree-sitter/go-tree-sitter` bindings with upstream grammars (cgo);
2. a young pure-Go reimplementation;
3. regex or indentation heuristics.

## Decision
Use the official bindings and grammars: Go, Python, JavaScript, TypeScript/TSX, Java, Rust and Ruby at M1. Files in other languages fall back to hunk units, and config and doc files become one unit per file.

## Consequences
- Accuracy matches what editors use, and adding a language means adding a grammar module plus a node-kind table in `internal/diffmap/lang.go`.
- `CGO_ENABLED=1` is required. Releases are built per platform on native runners instead of cross-compiled with goreleaser (`.github/workflows/release.yml`). Contributors need a C toolchain.
- Files larger than 1 MiB skip parsing and fall back to hunk units.
