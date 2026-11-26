# aireviewer in CI (headless mode)

Runs the PR augmentation stage on every pull or merge request, and a retrospective after merge, without a human (SPEC §12, ADR 0011).

**What CI may do:** post the augmentation summary as one PR comment, and only if the default branch's `.aireviewer.yml` contains:

```yaml
ci:
  post_augmentation: true
gate:
  trusted_bots: ["github-actions[bot]"]   # or your GitLab token's bot user, so reviewers can import the summary
```

**What CI never does:** post reviews, inline comments or verdicts; request reviewers; create PRs or issues; run PR code; change anyone's voice or memory. The MCP server refuses these in headless mode, whatever the prompt says. Without the opt-in, the summary only appears in the job log and artifacts.

## 1. Build the image

```sh
docker build -f ci/Dockerfile --build-arg CLAUDE_CODE_VERSION=<tested version> -t <registry>/aireviewer-ci:<version> .
docker push <registry>/aireviewer-ci:<version>
```

## 2a. GitHub Actions
Copy `ci/github/aireviewer.yml` to `.github/workflows/`, set `AIREVIEWER_IMAGE`, and add the `ANTHROPIC_API_KEY` repository secret. The workflow uses `pull_request`, not `pull_request_target`, and skips fork PRs. Pin `actions/upload-artifact` to a commit SHA.

## 2b. GitLab CI
Copy `ci/gitlab/aireviewer.gitlab-ci.yml` into the repository and `include` it, set `AIREVIEWER_IMAGE`, and add masked variables:
- `ANTHROPIC_API_KEY`;
- `GITLAB_TOKEN`: a project access token with the `api` scope and the Reporter role. `CI_JOB_TOKEN` cannot post MR notes. See [docs/forge-tokens.md](../docs/forge-tokens.md) for what each feature needs.

Jobs run only for MRs from the same project.

## Running locally

```sh
docker run --rm -e ANTHROPIC_API_KEY -e GITHUB_TOKEN <image> aireviewer-ci review https://github.com/o/r/pull/12 /tmp/run
```

## Reviewing after CI
A reviewer running `/pr-review <url>` locally imports the CI summary through `augmentation_import`. That works when the CI bot is in `gate.trusted_bots` and the head is unchanged, and the imported reports are re-validated against the local checkout.
