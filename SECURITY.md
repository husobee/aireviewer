# Security Policy

## Reporting a vulnerability

**Do not open a public issue.** Report vulnerabilities privately through the hosting platform's private vulnerability reporting feature (GitHub: *Security → Report a vulnerability*).
<!-- TODO(maintainers): add a fallback security contact before the repository goes public. -->

We aim to acknowledge reports within 3 business days and to agree on a disclosure timeline with you.

## Supported versions

Before 1.0, only the latest release receives security fixes.

## In scope

aireviewer reads untrusted content (PR/MR text, comments and code), passes it to an LLM, and from M5 executes PR code. We especially want reports about:

- **Prompt injection that bypasses a human gate**: forge content that causes aireviewer to push, create or post anything without explicit user confirmation, or to change a review verdict. This includes posting content that differs from the confirmed preview, forging an elicitation answer, or editing the decision log undetected (ADR 0006). For `/pr-create`, this also covers committing anything other than the confirmed fixes, pushing a commit other than the previewed one, force-pushing, or creating issues or PRs without confirmation (ADR 0007).
- **Sandbox escape or secret exposure** in runtime analysis (`runtime_run`, `lint_run`; ADR 0008): host filesystem access, network egress when disabled, privilege gain inside the container, or tokens visible inside it.
- **Headless (CI) bypasses**: anything other than the opted-in augmentation summary being posted from CI, a PR opting itself in, PR code running in CI, or a CI summary claiming human review (ADR 0011).
- **Memory poisoning or leakage**: retrospective memory crossing projects, being written outside the user's data directory, or being treated by agents as evidence or instructions rather than untrusted context (ADR 0010).
- **Voice poisoning**: getting a reviewer voice saved without the reviewer's approval, or a voice that changes what a comment claims or a check verdict (ADR 0009).
- **Running code without confirmation**: executing a plan the human did not confirm, a plan for a different head, commands taken from the PR head's config, or a forged runtime artifact accepted as trace evidence.
- **Credential leakage**: tokens written to run artifacts, logs, posted comments, or sent to unintended hosts.
- **Path traversal** in the run-context store or other file-writing tools.
- **Confinement bypasses**: reading outside a PR's git objects through the `code_*` tools, forging or overwriting a report without a valid submit token, or getting PR-controlled files loaded by Claude Code as instructions (ADR 0005).
- **Exfiltration** of private repository content to anywhere other than the configured model provider.

## Out of scope

- Model quality issues, such as a missed bug or a wrong explanation, unless they are caused by a gate or isolation failure. Please open a normal issue for these.
- Vulnerabilities in upstream dependencies that have already been reported upstream.
