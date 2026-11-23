# ADR 0013: Local issue index (BM25 first)

- Status: accepted
- Date: 2026-11-23
- Implements: SPEC §8.2 v2 (paper §4.2.2), part of M6

## Context
Issue linking v1 relies on the forge's search API: rate-limited, with ranking we cannot see, and weak on large backlogs. The spec's v2 is a local hybrid index (BM25 plus embeddings). Anthropic offers no embeddings API, so the embedding half needs a provider decision. The user chose to ship BM25 first.

## Decision
1. **SQLite FTS5 per repository**, at `<cache>/issues/<host>/<project>.db`, using the pure-Go `modernc.org/sqlite` driver (no cgo build tags; BSD-3-Clause).
   - It indexes titles, bodies and labels with the porter/unicode61 tokenizer, so "retries" matches "retry".
   - Ranking is BM25 with column weights title 10, body 1, labels 3.
2. **Incremental sync.** `issues_index_sync` lists issues (open and closed, PRs excluded) in order of update time from a stored cursor, upserting them. Triggers keep FTS consistent on edits. The first sync is full, capped at 5,000 issues per call; the next call continues from the cursor.
3. **Search.** `issues_search` uses the index when it exists, applying the same open-or-recently-closed window as v1, and returns BM25 scores alongside the candidates. Without an index it falls back to the forge search.
4. **Safe queries.** Free text becomes up to 32 quoted, OR-joined terms, so FTS operators, column filters and quotes in the input are inert.
5. **Re-ranking unchanged.** The `issue-linker` still reads candidates and assigns the calibrated 0-1 relevance score. BM25 only decides what it reads first.

## Consequences
- Issue linking works offline after the first sync, and costs one cheap incremental call per `/pr-create`.
- The index holds issue text on the reviewer's machine (cache directory, owner-only permissions), and it is shared across that user's runs.
- Embeddings can be added later as a second retriever behind `issues_search`, merged with BM25 (for example by reciprocal rank fusion), without changing callers.
