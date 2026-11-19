#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Launcher used by .mcp.json. Runs the prebuilt aireviewer-mcp binary from
# the plugin's bin/ directory, building it first from source when it is
# missing (development installs). Release installs ship the binary.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bin="$root/bin/aireviewer-mcp"

if [[ ! -x "$bin" ]]; then
  if ! command -v go >/dev/null 2>&1; then
    echo "aireviewer-mcp: binary not found at $bin and Go is not installed; run 'make build' or install a release" >&2
    exit 1
  fi
  (cd "$root" && go build -trimpath -o "$bin" ./cmd/aireviewer-mcp) >&2
fi

exec "$bin" "$@"
