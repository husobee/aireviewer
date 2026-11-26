# ADR 0015: Forge credentials are bound to declared hosts

- Status: accepted
- Date: 2026-11-26
- Supersedes: the host-guessing rule in `forge.HostConfig.KindForHost`

## Context
PR URLs reach aireviewer from users, from change-set discovery and from tool calls made by the model. The model can be steered by PR content. Before this decision, three paths sent the environment token to whatever host a URL named:

1. Any URL with GitLab's `/-/merge_requests/<n>` shape was treated as GitLab without a host check, so `GITLAB_TOKEN` (and `CI_JOB_TOKEN`) went to `https://<any host>/api/v4`.
2. A `/<o>/<r>/pull/<n>` URL on an unknown host fell back to GitHub, so `GITHUB_TOKEN` went to `https://<any host>/api/v3`, to its GraphQL endpoint, and into git's `Authorization` header on clone.
3. Hosts containing "github" or "gitlab" (`gitlab.attacker.example`) were accepted by name, in URLs and in git remotes.

Redirects also leaked credentials, even from trusted hosts:

- Go strips `Authorization` on cross-domain redirects but forwards GitLab's `Private-Token` / `Job-Token` headers.
- git re-sends `http.extraHeader` to wherever a redirect points.

## Decision
1. **Declared hosts only.** `KindForHost` knows `github.com`, `gitlab.com` and `AIREVIEWER_GITHUB_HOSTS` / `AIREVIEWER_GITLAB_HOSTS`, and nothing else. There is no guessing by name or URL shape. `ParsePRURL` and `ParseRemote` reject any other host, and a URL's shape must match its host's configured forge.
2. **Gate at the credential.** `providers.Registry.Token` returns a token only when `HostConfig.Trusted(kind, host)` holds. `Registry.Get` refuses an undeclared host. The git fetch path uses `Registry.Token` too. This holds even if a future caller builds a `forge.Repo` without the parser.
3. **Job tokens stay home.** `CI_JOB_TOKEN` is used only for `CI_SERVER_HOST`.
4. **No credentialed redirects off-host.** Both adapters use `forge.SameHostClient`, which follows redirects only within the original scheme and host and otherwise returns the 3xx unfollowed. Authenticated git sets `http.followRedirects=false`.
5. **Documented least privilege** in [docs/forge-tokens.md](../forge-tokens.md).

## Consequences
- Self-hosted instances whose names merely contain "github" or "gitlab" must now be declared. The error says how.
- Authenticated git fetches fail on redirects. GitHub serves redirects for renamed repositories, so use the current URL.
- An attacker-supplied URL can at most cause an unauthenticated error. It cannot exfiltrate a token.
