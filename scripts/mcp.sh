#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
#
# Launcher used by the mcpServers entry in .claude-plugin/plugin.json. Runs the prebuilt aireviewer-mcp binary from
# the plugin's bin/ directory, building it first from source when it is
# missing (development installs). Release installs ship the binary.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bin="$root/bin/aireviewer-mcp"

# Rebuild when missing, or (in a source checkout) when any Go source or
# go.mod/go.sum is newer than the binary, so dev sessions never run stale code.
stale=0
if [[ ! -x "$bin" ]]; then
  stale=1
elif [[ -f "$root/go.mod" ]] && [[ -n "$(find "$root/cmd" "$root/internal" "$root/go.mod" "$root/go.sum" -newer "$bin" \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' -o -name '*.json' \) -print -quit 2>/dev/null)" ]]; then
  stale=1
fi
if [[ $stale == 1 ]]; then
  if ! command -v go >/dev/null 2>&1; then
    if [[ -x "$bin" ]]; then
      echo "aireviewer-mcp: sources changed but Go is not installed; running the existing binary" >&2
    else
      echo "aireviewer-mcp: binary not found at $bin and Go is not installed; run 'make build' or install a release" >&2
      exit 1
    fi
  else
    (cd "$root" && go build -trimpath -o "$bin" ./cmd/aireviewer-mcp) >&2
  fi
fi

exec "$bin" "$@"
