# ADR 0007: PR creation from a local branch (branch runs, one confirmed create proposal)

- Status: accepted
- Date: 2026-11-20
- Implements: SPEC §8 (paper §4.2) on top of ADR 0003, ADR 0005 and ADR 0006

## Context
`/pr-create` is the first flow that writes to the author's own repository (a commit of accepted fixes, then a push) as well as to the forge (an optional new issue, then the PR). The paper's Gate 1 requires the author to approve the title, description, issue links, any new issue and each patch before anything is created (§4.2.5). The review side already has everything needed to analyse a change: confined code access through the mirror, the diff-map, report slots, findings, `fix_submit` with `git apply --check`, frozen proposals and server-side confirmation.

## Decision
1. **Branch runs.**
   - `branch_checkout` fetches the base from the forge and the local branch from the author's repository into the same blobless mirror, and checks out a worktree in the cache (ADR 0005).
   - The run is keyed by repository and branch (`RunIDForBranch`) and stores `mode: branch` in `checkout.json`, so every review-side tool (diff-map, code access, reports, findings, fixes, disputes) works unchanged.
   - Tools that need an existing PR refuse branch runs. `pr_issues` returns the issues the author linked in the draft, so alignment analysis and issue-quote evidence work before the PR exists.
   - An open PR for the branch is detected first, and creation is skipped (paper §4.2).
2. **The draft is server state.** Title, description, draft flag, labels, scored issue links, the proposed new issue, accepted fixes and the commit message live in `create/draft.json`, edited only through `create_draft_update`.
   - Issue links are verified against the forge.
   - A link defaults to the score threshold (`issue_link.threshold`, default 0.7) unless the author decides explicitly.
   - Accepted fixes are bound to the head they were validated at, and dropped when the branch moves.
3. **One frozen `create` proposal.**
   - `create_prepare` checks that:
     - the local branch is still at the analysed head;
     - the remote still points at the project;
     - no PR exists;
     - the accepted fixes apply together, built in a private index.

     It then freezes the whole plan: the fix commit (full diff and message, signing and sign-off), the new issue, the push target, and the PR title and body with closing keywords and the provenance footer.
   - `proposal_submit` confirms it exactly like a review (elicitation, permission-prompt fallback, strict mode) and runs, in order:
     1. commit the fixes;
     2. create the issue;
     3. push;
     4. open the PR.
   - One confirmation covers the plan because the preview shows every step and its content. Splitting it would ask the author to approve half-finished states.
4. **Author-repository writes.**
   - The fix commit is built with plumbing (`read-tree`, `apply --cached`, `write-tree`, `commit-tree`) against the expected head. The repository's `commit.gpgSign` is honoured.
   - If the branch is checked out, its worktree is fast-forwarded with `merge --ff-only`, which refuses to overwrite local edits. Otherwise the ref moves with a compare-and-swap `update-ref`. The author's working tree is never touched otherwise.
   - The push uses the author's own git configuration and credentials (never an aireviewer token), with no terminal, sets upstream, and never forces.
   - Immediately before pushing, the branch must point at exactly the previewed commit, so the PR contains what was reviewed.
   - The remote URL is read as configured (`remote.<name>.url`), not after `insteadOf` rewriting, so authors who rewrite HTTPS to SSH are still recognised.
5. **Resumable submission.**
   - Each completed step is recorded (`create/progress-<proposal>.json`, `create/issue.json`) and logged. Retrying the same proposal after a failure resumes without committing, creating, pushing or opening twice.
   - An issue created by a failed attempt is linked by later proposals instead of being created again.
   - The error message lists what completed.
6. **After creation** the skill checks out the new PR as a normal PR run and reuses the review-side augmentation, the gated summary comment and the gated reviewer request.

## Consequences
- An injected instruction can at most edit the local draft. Nothing is committed, pushed or created without the author's confirmation of the frozen plan.
- PRs from forks (head in a different repository) are not supported in v1: the branch is pushed to the remote that points at the project.
- `lint_run` (static analyzers as evidence, §8 step 4) needs the M5 sandbox. Until then, the author-mode review relies on the code-reviewer and repository policy.
- Issue retrieval is v1 only (forge search plus LLM re-ranking). The local hybrid index is M6.
