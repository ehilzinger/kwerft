#!/usr/bin/env bash
# Builds the spike kernel: apple/containerization's kernel config at a pinned
# tag plus kwerft.config, compiled with a native arm64 image that has pahole.
#
#   hack/spike-mac/kernel/build.sh      -> bin/spike-mac/vmlinux-kwerft-arm64
#   container system kernel set --arch arm64 --binary "$PWD/bin/spike-mac/vmlinux-kwerft-arm64"

OPTION=kernel
# shellcheck source=hack/spike-mac/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/../common.sh"

readonly CZ_TAG="0.48.0"                      # apple/containerization, 2026-10
readonly CZ="$WORK/containerization"
readonly IMAGE="kwerft-kernel-build:24.04"

[[ -d "$CZ" ]] || git clone -q --depth 1 --branch "$CZ_TAG" https://github.com/apple/containerization.git "$CZ"
git -C "$CZ" checkout -q -- kernel/config-arm64
cat "$SPIKE_DIR/kernel/kwerft.config" >>"$CZ/kernel/config-arm64"

container build -t "$IMAGE" -f "$SPIKE_DIR/kernel/Dockerfile" "$SPIKE_DIR/kernel" >"$WORK/kernel-image.log" 2>&1 \
  || { tail -20 "$WORK/kernel-image.log"; die "Kernel build image failed"; }
timed "kernel-build" make -C "$CZ/kernel" kernel-build kernel-install KIMAGE="$IMAGE" TARGET_ARCH=arm64 \
  >"$WORK/kernel-build.log" 2>&1 || { tail -30 "$WORK/kernel-build.log"; die "Kernel build failed"; }
cp "$CZ/bin/vmlinux-arm64" "$WORK/vmlinux-kwerft-arm64"
ok "Kernel at $WORK/vmlinux-kwerft-arm64"
