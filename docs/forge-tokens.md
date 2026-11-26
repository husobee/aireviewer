# Forge tokens: what aireviewer needs

aireviewer calls the forge API with one token per host and uses the same token for HTTPS git fetches of PR heads. This page lists the least privilege for each feature. It is derived from every API call the adapters make (`internal/forge/github`, `internal/forge/gitlab`). If a call is added, this page must change with it.

Public projects work without a token, subject to rate limits. Pushing a branch (`/pr-create`) uses **your own git credentials**, never the aireviewer token.

## Where a token is sent

A token is only ever sent to:

- `github.com` (API: `api.github.com`) for the GitHub token;
- `gitlab.com` for the GitLab token;
- hosts you declare in `AIREVIEWER_GITHUB_HOSTS` / `AIREVIEWER_GITLAB_HOSTS` (comma-separated), each for its own forge only;
- `CI_JOB_TOKEN` only to `CI_SERVER_HOST`, the GitLab instance running the job.

A PR URL or git remote on any other host is rejected before any request is made. API and git requests do not follow redirects off the host that holds the credential. See [ADR 0015](adr/0015-host-bound-forge-credentials.md).

## Features

| Feature | What it touches |
|---|---|
| **Read** | `/pr-review` analysis, `inspect`, `diffmap`, `augment`, change sets, `/pr-retro`, issue index, reviewer suggestions, risk history |
| **Post a review** | Gated: review with inline comments and verdict, summary comment (CI), request reviewers |
| **Create** | `/pr-create`: open the PR/MR with labels, optionally open an issue |

## GitHub: fine-grained personal access token

Under **Repository access**, choose *Only select repositories* and pick the repositories you review. Under **Account permissions**, grant nothing.

| Repository permission | Read | Post a review | Create | Used for |
|---|---|---|---|---|
| Metadata | Read | Read | Read | Mandatory; repository lookup and default branch |
| Contents | Read | Read | Read | `git fetch` of the PR head and base (private repositories) and the commit history used for reviewer suggestions |
| Pull requests | Read | **Read and write** | **Read and write** | PR, files, commits, reviews, and review comments. GraphQL closing-issue references. Write: submit the review (approve, request changes, inline comments), request reviewers, open the PR, label it, and post the summary comment |
| Issues | Read | Read | **Read and write** | Linked issues, the issue index, and PR conversation comments (read). Write: open a new issue from `/pr-create` |

Notes:

- PR conversation comments (the summary comment) go through GitHub's issue-comments endpoint. GitHub accepts **Pull requests: write** *or* **Issues: write** for it on a pull request. If posting the summary returns 403 with only Pull requests: write, also grant Issues: Read and write.
- Approving or requesting changes on your **own** PR is refused by GitHub whatever the permissions.
- Search (`review-requested:` workload counts, issue search, fix-commit history) needs no extra permission. It only sees repositories the token can access.
- If the organization requires approval for fine-grained tokens, the token works only after an owner approves it.
- **GitHub Actions** (`ci/github/aireviewer.yml`): the workflow's `GITHUB_TOKEN` uses `contents: read`, `pull-requests: write`, `issues: write`. It needs no personal token.
- **Classic token or `gh auth token`**: classic scopes are coarse. `public_repo` covers public repositories, and private ones need `repo`, which grants far more than the table above. Prefer a fine-grained token in `GITHUB_TOKEN`. The `gh` fallback only runs when neither `GITHUB_TOKEN` nor `GH_TOKEN` is set.

## GitLab: personal, project or group access token

GitLab tokens are scoped per **token scope** plus the **role** of the token's user in the project. For a single project, prefer a project access token, which acts as a bot user limited to that project.

| | Read | Post a review | Create |
|---|---|---|---|
| Scopes | `read_api`, `read_repository` | `api` | `api` |
| Minimum role | Reporter | Reporter (notes); **Developer** to request reviewers or approve | **Developer** |

What each needs:

- **Read**: merge request, diffs, commits, notes, closing issues, project, issue listing (issue index), and `git fetch` of `refs/merge-requests/<iid>/head`. `read_repository` is what allows the git fetch on private projects. Guest cannot read code in private projects, so the minimum role is Reporter.
- **Post a review**: draft notes then publish them (inline comments and summary), create or update the summary note. Reporter is enough. *Approve* additionally needs the token's user to be an eligible approver (Developer by default, or as your approval rules say). Authors cannot approve their own MR by default. *Request reviewers* updates the MR's reviewers and looks users up by username, which needs Developer (or being the MR author).
- **Create**: open the MR (Developer; the branch is pushed with your own credentials) and optionally an issue.
- The `api` scope also grants Git-over-HTTP access, so `read_repository` is not needed alongside it.
- **`CI_JOB_TOKEN`** is used only when `GITLAB_TOKEN` is unset, and only for `CI_SERVER_HOST`. It can read but cannot post MR notes, so CI posting needs `GITLAB_TOKEN` (`ci/gitlab/aireviewer.gitlab-ci.yml`).

## Resolution order

| Forge | Order |
|---|---|
| GitHub / GHES | `GITHUB_TOKEN`, `GH_TOKEN`, then `gh auth token --hostname <host>` |
| GitLab / self-managed | `GITLAB_TOKEN`, then `CI_JOB_TOKEN` (CI only, own instance only) |

One environment token serves every declared host of that forge. If you declare several self-hosted instances, make sure the token is valid on each, or run separate sessions.
