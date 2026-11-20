# ADR 0009: The reviewer's voice

- Status: accepted
- Date: 2026-11-20
- Extends: SPEC §9.3 (comment composition), ADR 0006

## Context
Reviewers want the comments and review bodies that aireviewer drafts for them to sound like them, not like a generic assistant. Teams also have house styles ("concise, prefer questions"). A voice is persistent text that ends up in prompts, which makes it a prompt-injection channel if anything other than the reviewer can set it. It must also not undermine the comment checks: a "blunt" voice must not make a rude comment pass.

## Decision
1. **Two layers.**
   - **Personal profile.** A description plus up to 5 example comments, stored in the reviewer's configuration directory (`$AIREVIEWER_CONFIG_DIR` or `<user config>/aireviewer/voice.json`, mode 0600). It applies in every repository.
   - **Team default.** `comments.voice` in the merge-base `.aireviewer.yml`, so a PR cannot change it.

   `voice_get` composes them, with the personal voice taking precedence on conflicts. A reviewer can opt out of the team default (`use_team_default: false`).
2. **Wording only.** The composed guidance opens with fixed rules: the voice affects tone, wording and structure, never what is claimed. It never softens or drops a problem, adds unearned praise, or overrides the review rules or checks. Lengths are capped (1000 characters per description, 500 per example).
3. **Where it applies.**
   - **Applies to:** comment bodies and review bodies that aireviewer *proposes*, plus the rewrites and expansions from the toxicity and usefulness agents.
   - **Never applies to:** text the reviewer wrote, which is used verbatim, or to the check verdicts. The agents receive the voice only to word their `rewrite`, and are told to judge exactly as they would without it.
4. **Setting it is human-only.** `voice_set` and `voice_clear` are not pre-approved by any skill. The client's permission prompt shows the exact text before it is written, and the skill calls them only on the reviewer's explicit request, never because PR text asked.

## Consequences
- The same comment checks apply whatever the voice, so a voice cannot make a toxic comment pass.
- A team default is maintainer-controlled text from the merge base, with the same trust as review policy files. It is capped in length and wrapped in the fixed rules.
- Learning a voice from the reviewer's forge history was considered and left out: it reads more data and needs an extra approval step. Examples pasted by the reviewer give most of the benefit.
- `/pr-create` descriptions are the author's text and do not use the reviewer voice in v1.
