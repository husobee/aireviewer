# Evaluating aireviewer

Measure, don't assume (SPEC §16, paper §5.3). The harness runs whole reviews end to end, not isolated agent calls (paper l.1628), and scores them against golden cases.

## Golden cases
One YAML file per PR in `golden/`:

```yaml
name: retry-backoff                       # letters, digits, . _ -
url: https://github.com/org/repo/pull/123
findings:                                 # ground truth at the PR head
  - id: no-backoff
    file: pkg/http/client.go
    lines: [88, 95]                       # a finding matches when it cites code within ±3 lines
    category: defect                      # optional; results are also broken down by category
    match: "backoff|sleep|retry"          # optional regex on the statement
    severity_min: major                   # optional; reported as severity agreement
traps:                                    # known false-positive locations
  - {file: pkg/http/client.go, lines: [10, 30], why: "unchanged and correct"}
alignment: exact                          # optional expected alignment category
risk: high                                # optional: was the PR followed by fixes?
```

Good sources: PRs later reverted or followed by bug fixes, and PRs with known tangled changes. Aim for about 30 cases across your repositories. The `sandbox-*` cases use husobee/aireviewer-sandbox, whose defects were planted on purpose.

## Running

```sh
make build
bin/aireviewer-mcp evaluate run   -plugin-dir . -max-cost 20 -out evaluation/results/$(date +%F).json
bin/aireviewer-mcp evaluate score -out evaluation/results/rescored.json    # re-score existing runs, no model calls
bin/aireviewer-mcp evaluate run   -baseline evaluation/baseline.json        # exits 1 if precision, recall or F1 drop > 0.05
bin/aireviewer-mcp evaluate bias  -runs ~/somewhere/.aireviewer/runs        # automation-bias audit candidates
```

`run` drives `claude -p "/aireviewer:pr-review <url> --eval"` for each case, in its own workspace under `-work`. In evaluation mode the server posts nothing and runs no PR code. The report records tokens, cost and duration per case and in total (paper §5.1.10); `-max-cost` stops starting new cases once the budget is spent.

## Reading the results
- **Precision is a lower bound.** A correct finding that the golden set does not list counts as a false positive. Read the `unmatched` claims and add any that are real to the case.
- **Recall** is the share of golden findings that were found.
- **Trap hits** are findings at locations known to be correct.
- **By category** breaks precision and recall down by comment type (paper §5.3.1).

## Automation-bias audit
`evaluate bias` lists posted reviews in which every comment came from an AI finding, none was edited, and nothing was disputed. Sample them and check by hand whether the reviewer engaged with the findings (SPEC §16).

## Feedback loop
After several retrospectives, `/pr-retro` calls `feedback_suggest`. It proposes configuration changes (policies for repeatedly disputed finding categories, comment-check modes, review scope) from patterns in local memory. Only a human applies them (paper l.1491-1497).
