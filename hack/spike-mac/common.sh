# SPDX-FileCopyrightText: 2026 Enzo Hilzinger
# SPDX-License-Identifier: AGPL-3.0-only

# Shared helpers for the Phase 7 spike scripts. Sourced, not executed.
# shellcheck shell=bash

set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
readonly ROOT
# shellcheck disable=SC2034  # used by the scripts that source this file
readonly SPIKE_DIR="$ROOT/hack/spike-mac"
readonly WORK="$ROOT/bin/spike-mac"      # rendered manifests, logs, timings
mkdir -p "$WORK"

# Everything is built and installed from a committed ref, exported with
# git archive, so uncommitted work in the checkout never leaks into the spike.
SPIKE_REF=$(git -C "$ROOT" rev-parse --short "${SPIKE_REF:-HEAD}")
readonly SPIKE_REF
readonly SRC="$WORK/src-$SPIKE_REF"
if [[ ! -d "$SRC" ]]; then
  mkdir -p "$SRC"
  git -C "$ROOT" archive "$SPIKE_REF" | tar -x -C "$SRC"
fi

# Platform versions come from the installer so the spike runs what production runs.
eval "$(grep -E '^(CILIUM|CERT_MANAGER|GATEWAY_API|TRAEFIK_CHART)(_CHART)?_VERSION=' "$SRC/install/install.sh")"

say()  { printf '\033[34m▸\033[0m %s\n' "$*"; }
ok()   { printf '\033[32m✓\033[0m %s\n' "$*"; }
warn() { printf '\033[33m!\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

# Records "<step> <seconds>" in $WORK/timings-<option>.txt.
timed() {
  local step=$1; shift
  local t0=$SECONDS
  "$@"
  printf '%s %s\n' "$step" "$((SECONDS - t0))" >>"$WORK/timings-${OPTION:?}.txt"
  ok "$step ($((SECONDS - t0)) s)"
}

# The Traefik values the installer writes, extracted so both stay identical.
traefik_values() {
  awk '/values\/traefik.yaml" <<.EOF.$/ {on=1; next} on && /^EOF$/ {exit} on' "$SRC/install/install.sh"
}

# Builds the Kwerft image with Apple's BuildKit and tags it for the cluster.
readonly KWERFT_IMAGE="ghcr.io/ehilzinger/kwerft:spike-$SPIKE_REF"
build_kwerft_image() {
  container build --platform linux/arm64 -t "$KWERFT_IMAGE" \
    --build-arg VERSION="spike-$SPIKE_REF" --build-arg COMMIT="$SPIKE_REF" \
    "$SRC" >"$WORK/build.log" 2>&1 || { tail -30 "$WORK/build.log"; die "Image build failed (log: $WORK/build.log)"; }
}

# Containers lose the internet while a VPN on the Mac is connected (vmnet's NAT
# is not routed through the tunnel). Fail fast instead of timing out later.
check_egress() {
  container run --rm docker.io/library/alpine:3.22 wget -T8 -q -O /dev/null https://get.k3s.io >/dev/null 2>&1 \
    || die "Containers cannot reach the internet. Is a VPN connected? Disconnect it or exclude 192.168.64.0/24."
}
