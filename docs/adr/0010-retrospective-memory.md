# ADR 0010: Retrospectives, review metrics and local memory

- Status: accepted
- Date: 2026-11-20
- Implements: SPEC §11.2 (paper §4.6), part of M6

## Context
The paper closes the loop with a PR Retrospective: a review summary (§4.6.1) and computed review metrics (§4.6.2), stored so later reviews can draw on past decisions, with retention handled explicitly (§5.1.7). Retrospective data mixes forge activity, which anyone can read, with the reviewer's local trail (drafts, disputes, overrides), which exists only on the machine where the review happened.

## Decision
1. **Deterministic metrics.** `review_metrics` reads the PR's activity from the forge: reviews, approvals, conversation and inline comments, commits. It combines that with this machine's decision log. Everything it computes is a formula, with no LLM:
   - time to first review, by someone other than the author or a bot;
   - decision latency, review rounds, changes requested, and commits after review started;
   - comments by origin: human, or aireviewer-assisted, identified by the provenance footer;
   - AI findings posted and disputed (with reasons), acceptance and dismissal rates, comment-check flag rates, and overrides.

   Metrics are recorded (source `review_metrics`) so the retrospective can cite them.
2. **Honest about missing data.** When the review was not done with aireviewer on this machine, `local.has_data` is false and AI-finding outcomes are reported as unknown, never inferred.
3. **Summary agent.** `retro-summarizer` writes a stage-`retro` report: the outcome, key threads with links, how the analysis changed across heads, and points worth remembering, each with evidence. Blameless by instruction.
4. **Posting is gated.** `retro_prepare` freezes a final PR comment (a marker comment, replaced on re-run) and posts it through `proposal_submit`, like every other write (ADR 0006).
5. **Local memory.**
   - **Storage.** `memory_save` stores the retrospective per host and project under the user's data directory (`$AIREVIEWER_DATA_DIR`, `$XDG_DATA_HOME/aireviewer/memory`, or `~/.local/share/aireviewer/memory`), as JSON plus Markdown with owner-only permissions. It is not stored in the workspace, because reviews run from any directory.
   - **Repository settings.** The repository's merge-base config controls it: `memory.enabled` and `memory.retention_days` (default 365). Expired entries are pruned on save.
   - **Querying.** `memory_query` (keywords, plus same-file or same-directory overlap) is available to the code reviewer and the explainer. Its results are untrusted context, never evidence about the current code.
   - **Erasure.** `memory_forget` erases one PR's entry or a whole project's. No skill pre-approves it.
6. **Label-based bug history** (`risk.bug_labels`, same milestone).
   - **What counts.** A commit counts as a bug fix if its subject matches the keywords, or if it belongs to a merged PR/MR with a bug label. That covers the PR's own commits plus its merge or squash commit. Fixes are labelled within the year before the merge base.
   - **Lookup.** Results are cached per repository for a day, and `repo_metrics` records how many fixes were found by keyword, by label, or both. If the forge is unavailable, it falls back to keywords only, stated in the output.

## Consequences
- Retrospectives are most informative on the reviewer's own machine. Elsewhere they still report forge-side timing and comment origin.
- Memory is per user, not shared with the team. Sharing happens through the optional posted retrospective comment.
- Label lookup costs two API calls per labelled PR (capped at 100 PRs per day per repository through the cache).
