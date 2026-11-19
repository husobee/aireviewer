# ADR 0006: Gated forge writes (proposals, server-side confirmation, decision log)

- Status: accepted
- Date: 2026-11-19
- Implements: ADR 0003 and SPEC §4 rule 6

## Context
M3 introduces the first forge writes: batched reviews with inline comments and suggestion blocks, reviewer requests, and the sticky augmentation summary. ADR 0003 requires a human to confirm the exact content of every write. A rule written into a skill prompt is not enforcement: the model could call a write tool on its own, or be steered into it by injected PR text.

## Decision
1. **Frozen proposals.**
   - `review_prepare`, `reviewers_prepare` and `sticky_prepare` render the exact payload, run every gate check, and store it as a proposal whose hash is the SHA-256 of `kind ‖ head ‖ canonical JSON payload`.
   - `proposal_submit` posts only that stored payload, only with the matching hash, and only if the PR head has not moved.
   - The model cannot change what gets posted between preview and submit.
2. **The server asks the human.**
   - With an elicitation-capable client, `proposal_submit` returns an MCP input request containing the preview (SEP-2322 multi round-trip; the SDK falls back to direct elicitation for older protocol versions).
   - The answer comes back on the retried call, bound to that proposal by `RequestState = id:hash`. The model never sees or forges the answer.
   - Without elicitation, the fallback is the client's tool-permission prompt: `proposal_submit` is never pre-approved by the skill.
   - `AIREVIEWER_STRICT_GATE=1` refuses to post without elicitation, for clients configured to auto-approve tools.
3. **Gate checks at prepare time.** Each check below blocks the proposal unless noted.
   - **Diff positions:** inline comments must target lines in the PR diff, within one unit.
   - **Quality checks:** toxicity and usefulness verdicts must exist for each comment's *current* text (bound by body hash). A `fail` in `block` mode needs a logged override.
   - **Circuit breaker** (paper l.1802): comments backed by low-confidence or disputed findings need an explicit, logged override.
   - **Self-review:** the PR author cannot approve or request changes.
   - **Reviewer-first mode** (paper §5.1.8): findings stay hidden, and a review cannot be prepared, until every reviewable unit has been viewed.
   - **Optional "PR Critique" checkpoint** (paper l.1748): `review.require_critique` requires the reviewer's own note, which is posted under "Reviewer notes".
4. **Decision log** (paper l.1687).
   - `decisions.jsonl` is an append-only, hash-chained record of drafts, checks, disputes, fixes, attachments, proposals, confirmations (with the mechanism used), declines, failures and posts. It is written only by the server; `run_context_put` cannot touch it.
   - Truncation of the tail is not detectable from inside the log, so every posted artifact's provenance footer carries the current log head hash as an external anchor.
5. **Fix suggestions** must apply cleanly to the PR head (`git apply --check`), change one hunk of one file in the diff, and state what they modify and what they leave intact (paper §4.2.4). They become forge suggestion blocks only after the reviewer approves them.
6. **Sticky summary re-use.** It embeds the augmentation reports (gzip and base64 in a hidden comment). `augmentation_import` accepts them only from the authenticated user or `gate.trusted_bots`, only for the same head, and re-validates every report against the local checkout before trusting it.

## Consequences
- An injected instruction or a misbehaving model can at most *prepare* a proposal. Posting needs the human's answer to the server's question, or the human's tool approval.
- Running in auto-approve mode without elicitation weakens the gate to the permission layer. The log records `human_via_permission_prompt` so this is visible, and strict mode closes it.
- GitLab has no API for the "request changes" review state. The verdict is stated in the summary note, the MR is not approved, and the result says so.
