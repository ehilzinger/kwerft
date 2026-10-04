#!/usr/bin/env bash
# Served by the console at https://<console>/join.sh with __CONSOLE_URL__
# replaced. Fetches the installer that matches the console's version and runs
# it in join mode, so a node never runs a newer or older installer than the
# cluster it joins.
#
#   curl -fsSL https://ops.example.com/join.sh | sudo bash -s -- --token kwft_join_… --role worker

set -Eeuo pipefail

main() {
  local console="${KWERFT_CONSOLE_URL:-__CONSOLE_URL__}"
  if [[ "$console" == "__CONSOLE_URL__" ]]; then
    echo "join.sh must be downloaded from your Kwerft console, or set KWERFT_CONSOLE_URL." >&2
    exit 2
  fi
  curl -fsSL "${console%/}/install.sh" | bash -s -- --join "$console" "$@"
}

main "$@"
