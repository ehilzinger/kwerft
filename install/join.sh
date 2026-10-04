#!/usr/bin/env bash
# Served by the console at https://<console>/join.sh with __CONSOLE_URL__
# replaced. Fetches the installer that matches the console's version and runs
# it in join mode, so a node never runs a newer or older installer than the
# cluster it joins.
#
#   curl -fsSL https://ops.example.com/join.sh | sudo bash -s -- --token kwft_join_… --role worker

set -Eeuo pipefail

# hack/release.sh stamps this with the installer of the same release (in the
# public install repository). It is used when the console does not serve
# /install.sh; empty in a checkout.
INSTALLER_URL=""

main() {
  local console="${KWERFT_CONSOLE_URL:-__CONSOLE_URL__}"
  local fallback="${KWERFT_INSTALLER_URL:-$INSTALLER_URL}"
  if [[ "$console" == "__CONSOLE_URL__" ]]; then
    echo "join.sh must be downloaded from your Kwerft console, or set KWERFT_CONSOLE_URL." >&2
    exit 2
  fi
  # Downloaded in full before it runs, so a broken connection never runs half a script.
  local script
  if ! script=$(curl -fsSL "${console%/}/install.sh"); then
    if [[ -z "$fallback" ]]; then
      echo "Could not download the installer from ${console%/}/install.sh." >&2
      exit 20
    fi
    echo "The console does not serve install.sh; using $fallback" >&2
    script=$(curl -fsSL "$fallback") || { echo "Could not download $fallback." >&2; exit 20; }
  fi
  bash -s -- --join "$console" "$@" <<<"$script"
}

main "$@"
