#!/usr/bin/env bash
# SPDX-License-Identifier: MIT
# Fails if any tracked or new source file lacks an SPDX MIT header in its first 5 lines.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."
missing=0
while IFS= read -r f; do
  if ! head -n 5 "$f" | grep -q 'SPDX-License-Identifier: MIT'; then
    echo "missing SPDX header: $f"
    missing=1
  fi
done < <(git ls-files --cached --others --exclude-standard -- '*.go' '*.sh' 'Makefile')
exit $missing
