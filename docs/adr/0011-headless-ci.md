# ADR 0011: Headless CI mode

- Status: accepted
- Date: 2026-11-23
- Implements: SPEC §12 under ADR 0003 (the human gate)

## Context
Teams want the augmentation summary on every PR without someone running `/pr-review`. In CI no human is present: there is no elicitation and no permission prompt. ADR 0003 allows exactly one automated post, the augmentation summary, and only when a repository owner opts in through committed config. A skill instruction alone cannot enforce that.

## Decision
1. **The server enforces it.** With `AIREVIEWER_HEADLESS=1` (set by the CI image and wrapper), `proposal_submit`:
   - **Allowed:** a `sticky` proposal (the augmentation summary), when the merge-base `.aireviewer.yml` sets `ci.post_augmentation: true`. The decision log records the confirmation as `ci_policy`.
   - **Refused:** everything else — reviews, reviewer requests, PR or issue creation, retrospective comments, runtime plans. Each refusal is logged.

   Because the config comes from the merge base, a PR cannot opt itself in.
2. **No other side effects in CI.**
   - `runtime_prepare` reports `not_applicable`, since running PR code needs a human to confirm the plan (ADR 0008).
   - `voice_set`, `voice_clear`, `memory_save` and `memory_forget` are refused.
3. **Honest provenance.** Summaries posted from CI carry a footer saying they were generated automatically and not reviewed by a human, never the human-confirmation footer.
4. **Skills.** `/pr-review --headless` runs augmentation only: analysts, the runtime analyst reporting not-run, the summary, then `sticky_prepare` and `proposal_submit`, then stop. `/pr-retro --headless` computes the metrics and summary into the job log, and posts or saves nothing.
5. **CI kit (`ci/`).**
   - **Image.** A Docker image containing Claude Code, the plugin and a prebuilt server.
   - **Wrapper.** `aireviewer-ci` accepts only https URLs and passes them as arguments, never as interpolated script.
   - **Templates.** GitHub Actions on `pull_request` (never `pull_request_target`), with the token scoped to contents read plus PR and issue comment write. GitLab CI uses a project access token. Both run only for same-project branches and keep the run directory as an artifact.

## Consequences
- An injected instruction in a PR can at most change what the summary says. It cannot post a verdict, comments or code changes from CI.
- For fork PRs, the jobs are skipped (GitHub would give a read-only token, and GitLab would expose a token to fork pipelines).
- Reviewers reuse the CI summary locally only if the CI bot is listed in `gate.trusted_bots`. Its reports are re-validated as usual (ADR 0006).
