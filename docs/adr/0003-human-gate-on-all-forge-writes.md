# ADR 0003: Every forge write requires explicit human confirmation

- Status: accepted
- Date: 2026-11-19

## Context
The paper requires human-in-the-loop quality gates to counter automation bias. Forge content is untrusted and may contain prompt injection.

## Decision
No push, PR/MR or issue creation, reviewer request, comment, sticky analysis comment, or review submission happens without the user confirming the exact content in the current session. The agent never chooses the review verdict.

The only exception is headless CI. There, a repository owner may opt in, through committed configuration (`ci.post_augmentation: true`), to posting the augmentation summary comment. CI never posts inline comments or verdicts.

## Consequences
- Write tools are annotated as non-read-only, are never given to subagents, and are called only from skill gate steps.
- Changes that weaken this rule require unanimous maintainer approval (GOVERNANCE.md).
