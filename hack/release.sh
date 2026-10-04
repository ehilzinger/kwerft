#!/usr/bin/env bash
# Release building blocks, shared by .github/workflows/release.yml and
# `make release-dry-run` so a release can be rehearsed on a Mac without Docker.
# See RELEASING.md.
#
#   hack/release.sh meta v0.2.0               version=, tag=, prerelease= (for $GITHUB_OUTPUT)
#   hack/release.sh scripts 0.2.0 OUT         stamped install.sh and join.sh + SHA256SUMS
#   hack/release.sh chart 0.2.0 OUT           Helm chart with version = appVersion = 0.2.0
#   hack/release.sh images 0.2.0 OUT          one image tarball per platform + SBOMs (no push)
#   hack/release.sh push-image 0.2.0 OUT      multi-arch image to the registry; writes OUT/image.txt
#   hack/release.sh push-chart 0.2.0 OUT      chart to the OCI registry; writes OUT/chart.txt
#   hack/release.sh notes 0.2.0 OUT           Markdown for the top of the GitHub Release
#   hack/release.sh install-repo DIR 0.2.0 SCRIPTS
#                                             copy stamped scripts into a checkout of the
#                                             public install repository and commit
#   hack/release.sh dry-run 0.2.0 OUT         scripts + chart + images + notes, nothing published
#
# The image and chart locations come from the pinned block of install/install.sh,
# so the installer and the release can never disagree about them.

set -Eeuo pipefail

readonly KO_VERSION="v0.19.1"             # keep in step with hack/dev-server.sh
readonly PLATFORMS=(linux/amd64 linux/arm64)
readonly SOURCE_URL="https://github.com/ehilzinger/kwerft"
INSTALL_REPO="${INSTALL_REPO:-ehilzinger/kwerft-install}"

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

die() { printf 'release: %s\n' "$*" >&2; exit 1; }
say() { printf '▸ %s\n' "$*" >&2; }

# pin NAME prints a variable from the pinned block of install/install.sh.
pin() {
  local value
  value=$(sed -n -E "s/^$1=\"([^\"]*)\".*/\1/p" install/install.sh)
  [[ -n "$value" ]] || die "$1 not found in install/install.sh"
  echo "$value"
}

# Overridable only to rehearse pushes against a local registry.
IMAGE_REPO=${IMAGE_REPO:-$(pin KWERFT_IMAGE_REPO)}   # ghcr.io/ehilzinger/kwerft
CHART_REF=${CHART_REF:-$(pin KWERFT_CHART_REPO)}     # oci://ghcr.io/ehilzinger/charts/kwerft
CHART_REGISTRY=${CHART_REF%/*}                       # oci://ghcr.io/ehilzinger/charts

# Semver without build metadata: OCI tags cannot contain "+".
check_version() {
  [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] \
    || die "version must look like 0.2.0 or 0.2.0-rc.1 (got '$1')"
}
is_prerelease() { [[ "$1" == *-* ]]; }

installer_url() { echo "https://raw.githubusercontent.com/$INSTALL_REPO/main/v$1/install.sh"; }

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$@"; else shasum -a 256 "$@"; fi
}

ko() {
  if [[ -n "${KO:-}" ]]; then "$KO" "$@"; else go run "github.com/google/ko@$KO_VERSION" "$@"; fi
}

commit() { git rev-parse "$@" HEAD 2>/dev/null || echo unknown; }

# ---------------------------------------------------------------------------

cmd_meta() {
  local ref=${1:?usage: meta TAG}
  local version=${ref#refs/tags/}
  version=${version#v}
  check_version "$version"
  local pre=false
  if is_prerelease "$version"; then pre=true; fi
  printf 'version=%s\ntag=v%s\nprerelease=%s\n' "$version" "$version" "$pre"
}

cmd_scripts() {
  local v=${1:?version} out=${2:?out dir}
  check_version "$v"
  mkdir -p "$out"
  say "Stamping install.sh and join.sh with $v"
  sed -E "s/^KWERFT_VERSION_DEFAULT=\"[^\"]*\"/KWERFT_VERSION_DEFAULT=\"$v\"/" install/install.sh >"$out/install.sh"
  grep -qx "KWERFT_VERSION_DEFAULT=\"$v\"" "$out/install.sh" || die "could not stamp install.sh"
  sed -E "s|^INSTALLER_URL=\"[^\"]*\"|INSTALLER_URL=\"$(installer_url "$v")\"|" install/join.sh >"$out/join.sh"
  grep -qx "INSTALLER_URL=\"$(installer_url "$v")\"" "$out/join.sh" || die "could not stamp join.sh"
  chmod 0755 "$out/install.sh" "$out/join.sh"
  bash -n "$out/install.sh"
  bash -n "$out/join.sh"
  "$out/install.sh" --help | grep -q "Kwerft installer $v" || die "stamped install.sh does not report $v"
  (cd "$out" && sha256 install.sh join.sh >SHA256SUMS)
}

cmd_chart() {
  local v=${1:?version} out=${2:?out dir}
  check_version "$v"
  mkdir -p "$out"
  say "Packaging the chart as $v"
  helm package charts/kwerft --version "$v" --app-version "$v" --destination "$out" >/dev/null
  local tgz="$out/kwerft-$v.tgz" meta
  meta=$(helm show chart "$tgz")
  grep -qx "version: $v" <<<"$meta" || die "$tgz: chart version is not $v"
  grep -qx "appVersion: $v" <<<"$meta" || die "$tgz: appVersion is not $v"
}

# ko_args VERSION sets KO_ARGS to the flags every image build shares.
ko_args() {
  local v=$1 tags="$1,v$1"
  is_prerelease "$v" || tags+=",latest"
  KO_ARGS=(--bare --tags "$tags"
    --image-label "org.opencontainers.image.source=$SOURCE_URL"
    --image-label "org.opencontainers.image.version=$v"
    --image-label "org.opencontainers.image.revision=$(commit)"
    --image-label "org.opencontainers.image.title=kwerft"
    --image-label "org.opencontainers.image.description=Kwerft console: API server and web UI"
    --image-annotation "org.opencontainers.image.source=$SOURCE_URL"
    --image-annotation "org.opencontainers.image.version=$v"
    --image-annotation "org.opencontainers.image.description=Kwerft console: API server and web UI")
}

require_ui() {
  [[ -f web/dist/index.html ]] || die "web/dist is missing: run 'make web' first"
  if grep -q 'UI not built' web/dist/index.html; then die "web/dist is the placeholder: run 'make web' first"; fi
}

# ko cannot write several platforms into one tarball, and k3s imports
# single-platform tarballs anyway (install.sh --image-archive), so one per platform.
cmd_images() {
  local v=${1:?version} out=${2:?out dir}
  check_version "$v"
  require_ui
  mkdir -p "$out/sbom"
  local platform arch
  ko_args "$v"
  for platform in "${PLATFORMS[@]}"; do
    arch=${platform#*/}
    say "Building $IMAGE_REPO:$v for $platform"
    KO_DOCKER_REPO=$IMAGE_REPO VERSION=$v COMMIT=$(commit --short) \
      ko build ./cmd/kwerft "${KO_ARGS[@]}" --platform "$platform" \
        --push=false --tarball "$out/kwerft-image-$v-linux-$arch.tar" --sbom-dir "$out/sbom" >/dev/null
  done
}

cmd_push_image() {
  local v=${1:?version} out=${2:?out dir}
  check_version "$v"
  require_ui
  mkdir -p "$out/sbom"
  local ref platforms
  ko_args "$v"
  platforms=$(IFS=,; echo "${PLATFORMS[*]}")
  say "Pushing $IMAGE_REPO:$v for $platforms"
  # ko also uploads an SPDX SBOM next to the image (tag sha256-<digest>.sbom).
  ref=$(KO_DOCKER_REPO=$IMAGE_REPO VERSION=$v COMMIT=$(commit --short) \
    ko build ./cmd/kwerft "${KO_ARGS[@]}" --platform "$platforms" --sbom-dir "$out/sbom" | tail -n1)
  [[ "$ref" == *@sha256:* ]] || die "ko did not report a digest (got '$ref')"
  echo "$IMAGE_REPO@${ref#*@}" | tee "$out/image.txt"
}

cmd_push_chart() {
  local v=${1:?version} out=${2:?out dir} log digest
  check_version "$v"
  [[ -f "$out/kwerft-$v.tgz" ]] || cmd_chart "$v" "$out"
  say "Pushing chart $v to $CHART_REGISTRY"
  local flags=()
  [[ "${HELM_PLAIN_HTTP:-}" == 1 ]] && flags=(--plain-http)
  log=$(helm push "$out/kwerft-$v.tgz" "$CHART_REGISTRY" ${flags[@]+"${flags[@]}"} 2>&1) || die "helm push failed: $log"
  printf '%s\n' "$log" >&2
  digest=$(sed -n -E 's/^Digest: *(sha256:[0-9a-f]+).*/\1/p' <<<"$log")
  [[ -n "$digest" ]] || die "helm push did not report a digest"
  echo "${CHART_REF#oci://}@$digest" | tee "$out/chart.txt"
}

cmd_notes() {
  local v=${1:?version} out=${2:?out dir} image="" chart=""
  check_version "$v"
  [[ -f "$out/image.txt" ]] && image=" · \`$(<"$out/image.txt")\`"
  [[ -f "$out/chart.txt" ]] && chart=" · \`$(<"$out/chart.txt")\`"
  cat <<EOF
## Install

On a fresh Ubuntu server (Hetzner Cloud or dedicated); re-run it on an existing
Kwerft server to upgrade:

\`\`\`bash
curl -fsSL $(installer_url "$v") | sudo bash -s -- --domain ops.example.com --yes
\`\`\`

The \`install.sh\` attached here is the same file; \`SHA256SUMS\` lists both scripts.

| | |
|---|---|
| Image | \`$IMAGE_REPO:$v\` (linux/amd64, linux/arm64)$image |
| Helm chart | \`$CHART_REF\` version \`$v\`$chart |

EOF
}

cmd_install_repo() {
  local dir=${1:?install repo checkout} v=${2:?version} scripts=${3:?stamped scripts dir}
  check_version "$v"
  [[ -d "$dir/.git" ]] || die "$dir is not a git checkout"
  local f
  for f in install.sh join.sh SHA256SUMS; do [[ -f "$scripts/$f" ]] || die "missing $scripts/$f"; done

  mkdir -p "$dir/v$v"
  cp "$scripts/install.sh" "$scripts/join.sh" "$scripts/SHA256SUMS" "$dir/v$v/"
  # main's top level is the latest stable release; a patch for an older line
  # or a prerelease only gets its versioned directory.
  local latest=""
  [[ -f "$dir/LATEST" ]] && latest=$(<"$dir/LATEST")
  if ! is_prerelease "$v" && [[ "$(printf '%s\n%s\n' "${latest:-0.0.0}" "$v" | sort -V | tail -n1)" == "$v" ]]; then
    cp "$scripts/install.sh" "$scripts/join.sh" "$scripts/SHA256SUMS" "$dir/"
    echo "$v" >"$dir/LATEST"
    if [[ "$latest" != "$v" ]]; then say "v$v becomes the latest stable release in $INSTALL_REPO"; fi
  fi

  git -C "$dir" add -A
  if git -C "$dir" diff --cached --quiet; then
    say "$INSTALL_REPO already has v$v"
    return 0
  fi
  git -C "$dir" commit -q -m "Kwerft v$v" -m "Released from $SOURCE_URL at $(commit)."
}

cmd_dry_run() {
  local v=${1:?version} out=${2:?out dir}
  check_version "$v"
  rm -rf "$out"
  cmd_scripts "$v" "$out"
  cmd_chart "$v" "$out"
  cmd_images "$v" "$out"
  cmd_notes "$v" "$out" >"$out/NOTES.md"
  say "Release $v rehearsed in $out (nothing was published):"
  (cd "$out" && find . -type f | sort | sed 's|^\./|  |') >&2
}

main() {
  local cmd=${1:-}
  [[ $# -gt 0 ]] && shift
  case "$cmd" in
    meta)         cmd_meta "$@" ;;
    scripts)      cmd_scripts "$@" ;;
    chart)        cmd_chart "$@" ;;
    images)       cmd_images "$@" ;;
    push-image)   cmd_push_image "$@" ;;
    push-chart)   cmd_push_chart "$@" ;;
    notes)        cmd_notes "$@" ;;
    install-repo) cmd_install_repo "$@" ;;
    dry-run)      cmd_dry_run "$@" ;;
    *) sed -n '2,17p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
  esac
}

main "$@"
