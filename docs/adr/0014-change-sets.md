# ADR 0014: Change sets: reviewing one change across several PRs

- Status: accepted (slice 1: review)
- Date: 2026-11-23
- Implements: SPEC §21 (an extension beyond the paper)

## Context
One logical change often spans several PRs: a stacked series in one repository, or coordinated PRs across repositories and forges. Reviewing each PR alone misses three things: a contract changed in one PR and used in another; merge and deploy order; and requirements satisfied only by the union of the PRs. SPEC §21 left four questions open: discovery versus declaration, authentication across hosts, size limits, and partially merged sets.

## Decision
1. **Declared, not discovered.** A set is declared by giving `/pr-review` 2–8 URLs. `changeset_discover` only suggests candidates: stacked PRs targeting this PR's branch or owning its base branch, and PRs referenced from the description ("depends on #N", PR URLs). The human confirms which to add. Wrong discovery wastes a full review per member.
2. **Members stay normal PR runs.** `changeset_open` checks out each member as an ordinary PR run (diff-map and issues included), so per-PR review, gates and posting are unchanged. Authentication works per host through the existing provider registry, so mixed GitHub and GitLab sets need nothing new.
3. **A change-set run** (`changeset-<hash of member runs>`) holds the set, the union of linked issues, and the set-level analysis. Its staleness key is a hash over every member's head SHA. `changeset_get` re-reads members from the forge and marks the set stale when any head moves.
4. **Cross-PR map** (`changeset_map`, deterministic).
   - For functions, methods and classes each member adds, changes or removes (up to 40 per member), a word-boundary `git grep` finds uses in every other member, at its head and its merge base.
   - Edges carry repo-qualified unit ids (`unit:<host>/<project>:<file>#<name>`), up to 5 use locations, and whether the provider's file is an interface file (proto, OpenAPI, schema, migration).
   - In a stacked pair from the same repository, the consumer's head contains the provider's code, so uses in files only the provider changed are attributed to the provider.
5. **Merge order and compatibility** (`changeset_order`, deterministic).
   - **Constraints:**
     - `stacked` (B targets A's branch);
     - `introduces` (B newly uses a symbol A adds, so A first);
     - `removal` (B stops using a symbol A removes, so B first).
   - **Contracts:** modified symbols used across members. These need review but don't force an order.
   - **Problems:**
     - `incompatible` (a member still uses a symbol another removes);
     - `cycle` (members on the cycle are listed separately from those only blocked behind it);
     - `merged_out_of_order`.
   - **Sequence:** a topological order puts merged members first; they are kept as read-only context.
6. **Set-level report.** A `changeset-analyst` report (stage `changeset`) on the set run is validated like any report, with two extensions:
   - code evidence must name its member run (`member`), and is checked against that member's head or merge base;
   - anchors must be qualified unit ids present in a member's diff-map.

   `member` is refused in ordinary PR reports.
7. **Gates unchanged.** Nothing posts on the set run (`freshPR` refuses it). Comments, fixes, verdicts, reviewer requests and summary comments are per member and gated as before. The set panel feeds the per-member review ("merge after o/api#1").
8. **Limits.** At most 8 members, and the per-member token-limit gate still applies.

## Consequences
- Contract and order checks are textual (identifier matching across repositories), so they can over-match common names. The analyst verifies important edges with `code_read` before claiming anything.
- Deferred to later slices:
  - creating linked PRs from several branches or repositories (`/pr-create` across repos, with a declared merge order);
  - building and running the set together in the sandbox;
  - a combined diff-map object.
