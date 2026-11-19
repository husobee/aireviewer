# ADR 0001: Orchestrators are skills, not subagents

- Status: accepted
- Date: 2026-11-19

## Context
The paper's framework has three orchestrating agents (PR Creation Agent, PR Augmentation Agent and PR Review Agent), each of which delegates to specialised sub-agents. Claude Code subagents cannot spawn further subagents.

## Decision
Orchestrators are implemented as **skills** that run in the user's main Claude Code session and dispatch subagents. Subagents are leaves: they receive read-only tools and return JSON conforming to the report schema.

## Consequences
- The main session is the natural-language interface the paper describes, used by the author during creation and by the reviewer during review.
- Human gates live in skills, the only place where forge write tools are allow-listed.
- Parallel fan-out (e.g. the four augmentation analysts) is done by the skill issuing multiple subagent calls at once.
