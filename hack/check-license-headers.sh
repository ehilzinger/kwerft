#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Enzo Hilzinger
# SPDX-License-Identifier: AGPL-3.0-only

# Fails when a tracked source file lacks its license header: the line
# `SPDX-License-Identifier: AGPL-3.0-only` within its first 5 lines (after the
# shebang or the //go:build line, if any). Generated Go files get the header
# from hack/boilerplate.go.txt via `make generate`. See CONTRIBUTING.md.
#
#   hack/check-license-headers.sh

set -Eeuo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

readonly WANT='SPDX-License-Identifier: AGPL-3.0-only'

missing=()
while IFS= read -r -d '' f; do
  [[ -f "$f" ]] || continue # deleted in the work tree, not yet committed
  head -n 5 -- "$f" | grep -qF "$WANT" || missing+=("$f")
done < <(git ls-files -z -- \
  '*.go' '*.ts' '*.tsx' '*.js' '*.mjs' '*.css' '*.sh' '*.bats' \
  ':!:web/node_modules/**' ':!:web/dist/**' ':!:bin/**' \
  ':!:hack/spike-mac/**') # another session's spike: left as it is

if ((${#missing[@]} > 0)); then
  echo "Missing the license header (\"$WANT\" in the first 5 lines):" >&2
  printf '  %s\n' "${missing[@]}" >&2
  echo "Add the two SPDX lines from CONTRIBUTING.md › License headers." >&2
  exit 1
fi
