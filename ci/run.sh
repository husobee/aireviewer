#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# aireviewer-ci <review|retro> <pr-or-mr-url> [workdir]
#
# Runs aireviewer headless in CI (ADR 0011). review = augmentation only (the
# summary comment is posted only if the merge-base .aireviewer.yml sets
# ci.post_augmentation: true); retro = metrics and summary in the job log,
# nothing posted. Run artifacts land in <workdir>/.aireviewer/runs.
set -euo pipefail

mode="${1:?usage: aireviewer-ci <review|retro> <url> [workdir]}"
url="${2:?usage: aireviewer-ci <review|retro> <url> [workdir]}"
workdir="${3:-$PWD/.aireviewer-ci}"

case "$url" in
  https://*) ;;
  *) echo "aireviewer-ci: expected an https PR/MR URL, got: $url" >&2; exit 2 ;;
esac
case "$mode" in
  review) cmd="/aireviewer:pr-review $url --headless" ;;
  retro)  cmd="/aireviewer:pr-retro $url --headless" ;;
  *) echo "aireviewer-ci: mode must be review or retro" >&2; exit 2 ;;
esac
: "${ANTHROPIC_API_KEY:?ANTHROPIC_API_KEY is required}"

export AIREVIEWER_HEADLESS=1
mkdir -p "$workdir"
cd "$workdir"
# proposal_submit is allowed so the server can apply its CI policy; the
# server refuses everything except the opted-in augmentation summary.
exec claude -p \
  --plugin-dir /opt/aireviewer \
  --allowedTools "Agent,mcp__plugin_aireviewer_aireviewer__proposal_submit" \
  --output-format text \
  "$cmd"
