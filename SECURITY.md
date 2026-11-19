# Security Policy

## Reporting a vulnerability

**Do not open a public issue.** Report vulnerabilities privately through the hosting platform's private vulnerability reporting feature (GitHub: *Security → Report a vulnerability*).
<!-- TODO(maintainers): add a fallback security contact before the repository goes public. -->

We aim to acknowledge reports within 3 business days and to agree on a disclosure timeline with you.

## Supported versions

Before 1.0, only the latest release receives security fixes.

## In scope

aireviewer reads untrusted content (PR/MR text, comments and code), passes it to an LLM, and from M5 executes PR code. We especially want reports about:

- **Prompt injection that bypasses a human gate**: forge content that causes aireviewer to push, create or post anything without explicit user confirmation, or to change a review verdict. This includes posting content that differs from the confirmed preview, forging an elicitation answer, or editing the decision log undetected (ADR 0006).
- **Sandbox escape or secret exposure** in runtime analysis (`sandbox_run`): host filesystem access, network egress when disabled, or tokens visible inside the container.
- **Credential leakage**: tokens written to run artifacts, logs, posted comments, or sent to unintended hosts.
- **Path traversal** in the run-context store or other file-writing tools.
- **Confinement bypasses**: reading outside a PR's git objects through the `code_*` tools, forging or overwriting a report without a valid submit token, or getting PR-controlled files loaded by Claude Code as instructions (ADR 0005).
- **Exfiltration** of private repository content to anywhere other than the configured model provider.

## Out of scope

- Model quality issues, such as a missed bug or a wrong explanation, unless they are caused by a gate or isolation failure. Please open a normal issue for these.
- Vulnerabilities in upstream dependencies that have already been reported upstream.
