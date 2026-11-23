# ADR 0012: Evaluation harness and feedback loop

- Status: accepted
- Date: 2026-11-23
- Implements: SPEC §16 (paper §5.3, l.1628, §5.1.10, l.1491-1497), part of M6

## Context
The paper warns against assuming agent quality: evaluate end to end rather than per agent (l.1628), stratify by comment type (§5.3.1), track tokens and cost per run (§5.1.10), check for automation bias, and feed lessons back into the configuration, with a human in the loop (l.1491-1497).

## Decision
1. **Golden cases.** Each case is a YAML file listing a PR URL, ground-truth findings (file, line range, optional category, statement pattern and minimum severity), false-positive traps, and optionally the expected alignment category and risk outcome.
2. **Deterministic scoring** (`internal/golden`).
   - **Matching.** A finding matches when one of its head-side code citations falls within ±3 lines of the expected range, and the category and pattern agree.
   - **Precision** is a lower bound: correct findings missing from the golden set count against it. Duplicate findings of the same problem do not count against it.
   - **Recall, F1, trap hits, alignment accuracy and risk agreement** are reported per case. Findings are also broken down by category, and totals are micro-averaged.
3. **End-to-end runs.** `aireviewer-mcp evaluate run` runs `claude -p "/aireviewer:pr-review <url> --eval"` per case, in its own workspace.
   - **Eval mode** (`AIREVIEWER_EVAL=1`, which implies headless): the server refuses every write, including the CI-allowed summary, and runs no PR code.
   - **Cost.** Token use, cost and duration come from Claude Code's JSON result, per case and in total. `-max-cost` stops starting new cases once the budget is spent.
   - **Re-scoring.** `evaluate score` re-scores existing runs without calling a model.
   - **Regressions.** `-baseline` compares against an earlier report and exits non-zero when precision, recall or F1 drops by more than the tolerance (default 0.05), or when trap hits rise.
4. **Automation-bias audit.** `evaluate bias` lists posted reviews in which every comment came from an AI finding, none was edited and nothing was disputed. These are candidates for a manual audit.
5. **Feedback loop.**
   - **What memory records.** Retrospectives store disputed findings by category, the reasons given, comment-check flag rates, AI acceptance and time to first review.
   - **Suggestions.** `feedback_suggest` looks across saved retrospectives for patterns that recur over several PRs and proposes changes: a policy for a repeatedly disputed category, blocking comment checks, a narrower review scope, or reviewer suggestions and the CI summary for slow first reviews.
   - **Human-applied.** Suggestions are text for a human. No tool edits configuration.

## Consequences
- Precision depends on how complete the golden set is, so unmatched findings should be read and the cases extended. This is stated in the output.
- Agent-level regression testing is covered by golden cases rather than separate per-agent fixtures, as the paper recommends end-to-end evaluation. A case can target author mode (`mode: author`).
- Evaluation runs cost model tokens; the budget flag and the cost report make that visible.
