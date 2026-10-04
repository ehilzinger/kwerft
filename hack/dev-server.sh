#!/usr/bin/env bash
# Install or upgrade Werft on a test server from this checkout — no Docker and
# no registry needed. Builds the UI and a console image locally (with ko),
# copies the installer, chart and image over SSH, and runs the installer with
# --image/--image-archive. Re-run after every change to redeploy.
#
#   hack/dev-server.sh root@203.0.113.24 --domain ops.example.com
#
# Every argument after the SSH target is passed to install.sh.

# Remote commands are built on this machine on purpose (paths are constants,
# installer flags are quoted with %q).
# shellcheck disable=SC2029

set -Eeuo pipefail

readonly KO_VERSION="v0.19.1"
readonly REPO="ghcr.io/ehilzinger/werft"
readonly REMOTE_DIR="/opt/werft-src"

say() { printf '\033[34m▸\033[0m %s\n' "$*"; }
die() { printf '\033[31m✗ %s\033[0m\n' "$*" >&2; exit 1; }

main() {
  [[ $# -ge 1 && "$1" != -* ]] || die "Usage: hack/dev-server.sh user@host [install.sh flags...]"
  local target=$1; shift

  local root arch commit tag image
  root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)

  say "Checking $target"
  case "$(ssh "$target" uname -m)" in
    x86_64)  arch=amd64 ;;
    aarch64) arch=arm64 ;;
    *)       die "Unsupported server architecture" ;;
  esac

  commit=$(git -C "$root" rev-parse --short HEAD)
  tag="dev-$commit"
  # Uncommitted changes get a unique tag so Kubernetes rolls out the new image.
  [[ -z "$(git -C "$root" status --porcelain)" ]] || tag="$tag-dirty-$(date +%s)"
  image="$REPO:$tag"
  # Global, not local: the EXIT trap runs after main has returned.
  WORK=$(mktemp -d)
  trap 'rm -rf "$WORK"' EXIT

  say "Building the web UI"
  [[ -d "$root/web/node_modules" ]] || (cd "$root/web" && npm ci --silent)
  (cd "$root/web" && npm run build --silent >/dev/null)

  say "Building $image for linux/$arch"
  (cd "$root" && KO_DOCKER_REPO="$REPO" VERSION="$tag" COMMIT="$commit" \
    go run "github.com/google/ko@$KO_VERSION" build ./cmd/werft \
      --bare --tags "$tag" --platform "linux/$arch" \
      --push=false --tarball "$WORK/werft-image.tar" >/dev/null)

  say "Copying installer, chart and image to $target:$REMOTE_DIR"
  # COPYFILE_DISABLE keeps macOS tar from adding ._ metadata files.
  COPYFILE_DISABLE=1 tar -C "$root" -cf - install charts \
    | ssh "$target" "sudo rm -rf $REMOTE_DIR && sudo mkdir -p $REMOTE_DIR && sudo tar -C $REMOTE_DIR -xf -"
  ssh "$target" "sudo tee $REMOTE_DIR/werft-image.tar >/dev/null" <"$WORK/werft-image.tar"

  say "Running the installer on $target"
  local args
  args=$(printf '%q ' --image "$image" --image-archive "$REMOTE_DIR/werft-image.tar" --yes "$@")
  local tty=()
  [[ -t 1 ]] && tty=(-t)
  ssh ${tty[@]+"${tty[@]}"} "$target" "sudo $REMOTE_DIR/install/install.sh $args"
}

main "$@"
