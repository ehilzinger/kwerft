#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Enzo Hilzinger
# SPDX-License-Identifier: AGPL-3.0-only

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
#   hack/release.sh trailers 0.2.0            upgrade_from=, rollback_safe=, channel= (tag trailers
#                                             or their defaults, for $GITHUB_OUTPUT)
#   hack/release.sh crdcompat 0.2.0           chart CRDs vs. the previous release tags; fails on
#                                             incompatible changes unless Rollback-Safe: no
#   hack/release.sh manifest 0.2.0 OUT [--draft]
#                                             OUT/manifest.json from the stamped OUT/install.sh and
#                                             OUT/{image,chart}.txt (--draft: without digests)
#   hack/release.sh install-repo DIR 0.2.0 FILES
#                                             copy the stamped scripts, manifest.json and NOTES.md
#                                             into a checkout of the public install repository,
#                                             update releases.json (and LATEST) and commit
#   hack/release.sh dry-run 0.2.0 OUT         all of the above, nothing published; the install
#                                             repository is previewed in OUT/install-repo
#
# The image and chart locations come from the pinned block of install/install.sh,
# so the installer and the release can never disagree about them. A release's
# tag message may carry the trailers `Upgrade-From: 0.5.0` and
# `Rollback-Safe: no` (RELEASING.md); nothing is bumped in the source tree.

set -Eeuo pipefail

readonly KO_VERSION="v0.19.1"             # keep in step with hack/dev-server.sh
readonly PLATFORMS=(linux/amd64 linux/arm64)
readonly SOURCE_URL="https://github.com/ehilzinger/kwerft"
INSTALL_REPO="${INSTALL_REPO:-ehilzinger/kwerft-install}"
# Where users fetch a release's installer: kwerft.dev/v<version>/install.sh
# redirects to $INSTALL_REPO (kwerft-homepage's netlify.toml). Forks with
# their own INSTALL_REPO set this too.
INSTALLER_BASE="${INSTALLER_BASE:-https://kwerft.dev}"

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

installer_url() { echo "$INSTALLER_BASE/v$1/install.sh"; }

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$@"; else shasum -a 256 "$@"; fi
}

ko() {
  if [[ -n "${KO:-}" ]]; then "$KO" "$@"; else go run "github.com/google/ko@$KO_VERSION" "$@"; fi
}

commit() { git rev-parse "$@" HEAD 2>/dev/null || echo unknown; }

# ---------------------------------------------------------------------------
# Release history and tag trailers. jq does the semver arithmetic: sort -V
# puts 0.2.0 before 0.2.0-rc.1.

# semkey orders versions by semver precedence (a release after its prereleases).
# shellcheck disable=SC2016  # jq variables, not shell ones
readonly JQ_SEMVER='
def semkey: ltrimstr("v") | split("-") as $p
  | ($p[0] | split(".") | map(tonumber))
    + [if ($p | length) > 1 then 0 else 1 end]
    + [$p[1:] | join("-") | split(".") | map(if test("^[0-9]+$") then [0, tonumber] else [1, .] end)];
def line: ltrimstr("v") | split("-")[0] | split(".")[0:2] | map(tonumber);
def is_release_tag: test("^v[0-9]+\\.[0-9]+\\.[0-9]+(-[0-9A-Za-z.-]+)?$");
'

# release_tags prints every release tag of this repository (v0.2.0, v0.2.0-rc.1).
release_tags() { git tag -l 'v*'; }

# previous_releases V prints the releases a rollback from V can land on and
# that crdcompat compares with: the newest stable release before V and, if
# newer, the newest prerelease before it. Nothing for the first release.
previous_releases() {
  release_tags | jq -Rnr --arg v "$1" "$JQ_SEMVER"'
    [inputs | select(is_release_tag) | ltrimstr("v") | select(semkey < ($v | semkey))]
    | sort_by(semkey) as $older
    | [($older | map(select(contains("-") | not)) | last), ($older | last)]
    | map(select(. != null)) | unique | .[]'
}

# default_upgrade_from V: two minor releases back, counted over the minor
# lines that have a release tag (prereleases count, so 0.6.0 after
# 0.5.0-rc.3 gives 0.4.0; a major bump counts the lines before it). With
# fewer lines, the oldest one; without any, V's own line.
default_upgrade_from() {
  local older
  older=$(release_tags | jq -Rn --arg v "$1" "$JQ_SEMVER"'($v | line) as $cur | [inputs | select(is_release_tag) | line | select(. < $cur)] | length')
  if [[ "$older" == 0 ]]; then say "No release tags before v$1 (a shallow clone?): it upgrades only from its own minor line"; fi
  release_tags | jq -Rnr --arg v "$1" "$JQ_SEMVER"'
    ($v | line) as $cur
    | ([inputs | select(is_release_tag) | line | select(. < $cur)] | unique) as $lines
    | (if ($lines | length) == 0 then $cur elif ($lines | length) == 1 then $lines[0] else $lines[-2] end)
    | map(tostring) | join(".") + ".0"'
}

# tag_message V prints the message of the annotated tag vV without its
# signature; nothing when there is no such tag (rehearsals).
tag_message() {
  local ref="refs/tags/v$1" type
  type=$(git cat-file -t "$ref" 2>/dev/null) || return 0
  if [[ "$type" != tag ]]; then
    say "v$1 is a lightweight tag: it has no trailers, so the defaults apply (tag releases with git tag -a)"
    return 0
  fi
  git for-each-ref --format='%(contents)' "$ref" | sed '/^-----BEGIN [A-Z ]*SIGNATURE-----$/,$d'
}

# trailer V KEY prints the last value of the trailer KEY (any case) in the
# tag message of vV.
trailer() {
  local key
  key=$(tr '[:upper:]' '[:lower:]' <<<"$2")
  tag_message "$1" | git interpret-trailers --parse \
    | awk -v k="$key" 'tolower(substr($0, 1, index($0, ":") - 1)) == k { v = substr($0, index($0, ":") + 1); sub(/^ +/, "", v); sub(/ +$/, "", v) } END { if (v != "") print v }'
}

# upgrade_from V: the oldest release that may upgrade to V directly. The tag
# trailer Upgrade-From overrides the default.
upgrade_from() {
  local v=$1 from
  from=$(trailer "$v" Upgrade-From)
  if [[ -z "$from" ]]; then default_upgrade_from "$v"; return; fi
  from=${from#v}
  [[ "$from" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || die "tag v$v: Upgrade-From must be a version such as 0.5.0 (got '$from')"
  jq -en --arg a "$from" --arg b "$v" "$JQ_SEMVER"'($a | semkey) <= ($b | semkey)' >/dev/null \
    || die "tag v$v: Upgrade-From $from is newer than the release itself"
  echo "$from"
}

# rollback_safe V prints true or false: false when the tag says Rollback-Safe: no.
rollback_safe() {
  local value
  value=$(trailer "$1" Rollback-Safe | tr '[:upper:]' '[:lower:]')
  case "$value" in
    "" | yes | true) echo true ;;
    no | false) echo false ;;
    *) die "tag v$1: Rollback-Safe must be yes or no (got '$value')" ;;
  esac
}

channel() { if is_prerelease "$1"; then echo edge; else echo stable; fi; }

# published V: when the tag was made, in UTC; now for a rehearsal.
published() {
  local d=""
  if git rev-parse -q --verify "refs/tags/v$1" >/dev/null; then
    d=$(TZ=UTC0 git for-each-ref --format='%(creatordate:format-local:%Y-%m-%dT%H:%M:%SZ)' "refs/tags/v$1")
  fi
  [[ -n "$d" ]] || d=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  echo "$d"
}

# tag_notes V prints the body of vV's tag message, without the subject line
# and the trailers: hand-written notes go on top of the release notes.
tag_notes() {
  local msg skip=0
  msg=$(tag_message "$1")
  [[ -n "$msg" ]] || return 0
  if [[ -n "$(git interpret-trailers --parse <<<"$msg")" ]]; then skip=1; fi
  awk -v RS= -v skip="$skip" '{ p[NR] = $0 } END { for (i = 2; i <= NR - skip; i++) printf "%s\n\n", p[i] }' <<<"$msg"
}

crdcompat() {
  if [[ -n "${CRDCOMPAT:-}" ]]; then "$CRDCOMPAT" "$@"; else go run ./hack/crdcompat "$@"; fi
}

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
  local v=${1:?version} out=${2:?out dir} image="" chart="" from safe k3s
  check_version "$v"
  from=$(upgrade_from "$v")
  safe=$(rollback_safe "$v")
  k3s=$(pin K3S_VERSION)
  [[ -f "$out/image.txt" ]] && image=" · \`$(<"$out/image.txt")\`"
  [[ -f "$out/chart.txt" ]] && chart=" · \`$(<"$out/chart.txt")\`"
  tag_notes "$v"
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
| Kubernetes | k3s \`$k3s\` for new installs |
| Upgrades from | $from and later |
EOF
  if [[ "$safe" == false ]]; then
    cat <<EOF

**Not rollback-safe.** If an upgrade to $v fails, rolling back also restores
the console's database from the copy taken before the upgrade: changes made
in between are lost.
EOF
  fi
  echo
}

cmd_trailers() {
  local v=${1:?version} from safe
  check_version "$v"
  from=$(upgrade_from "$v")
  safe=$(rollback_safe "$v")
  printf 'upgrade_from=%s\nrollback_safe=%s\nchannel=%s\n' "$from" "$safe" "$(channel "$v")"
}

# The chart's CRDs must stay a compatible extension of those of the releases
# a failed upgrade rolls back to, because CRDs are never rolled back
# (docs/phase6-upgrades.md, Compatibility rules).
cmd_crdcompat() {
  local v=${1:?version} base tmp rc failed=0 baselines safe
  check_version "$v"
  safe=$(rollback_safe "$v")
  baselines=$(previous_releases "$v")
  if [[ -z "$baselines" ]]; then
    say "No release before $v: no CRDs to compare with"
    return 0
  fi
  tmp=$(mktemp -d)
  for base in $baselines; do
    if ! git cat-file -e "v$base:charts/kwerft/crds" 2>/dev/null; then
      say "v$base has no charts/kwerft/crds: skipped"
      continue
    fi
    say "Comparing the chart's CRDs with v$base"
    mkdir -p "$tmp/$base"
    git archive "v$base" charts/kwerft/crds | tar -x -C "$tmp/$base"
    rc=0
    crdcompat -name "v$base" "$tmp/$base/charts/kwerft/crds" charts/kwerft/crds >&2 || rc=$?
    case $rc in
      0) ;;
      1) failed=1 ;;
      *) rm -rf "$tmp"; die "crdcompat could not compare with v$base" ;;
    esac
  done
  rm -rf "$tmp"
  if [[ "$failed" == 1 ]]; then
    if [[ "$safe" == false ]]; then
      say "Incompatible CRD changes accepted: the tag of v$v says Rollback-Safe: no"
      return 0
    fi
    die "the CRDs changed incompatibly: keep them compatible (docs/phase6-upgrades.md, Compatibility rules) or tag v$v with the trailer 'Rollback-Safe: no'"
  fi
}

# manifest.json tells consoles what a release is and how to upgrade to it
# (docs/phase6-upgrades.md, Releases). Everything comes from the pinned block
# of the stamped install.sh, the tag, and the digests the pushes reported.
cmd_manifest() {
  local v=${1:?version} out=${2:?out dir} draft=${3:-} script pins
  check_version "$v"
  [[ -z "$draft" || "$draft" == --draft ]] || die "manifest: unknown flag $draft"
  script="$out/install.sh"
  [[ -f "$script" ]] || die "$script is missing: run 'hack/release.sh scripts $v $out' first"

  # NAME=value for every NAME="value" line of the pinned block, which ends
  # where the exit codes (the first readonly) begin.
  pins=$(sed -n -E '/^readonly /q; s/^([A-Z][A-Z0-9_]*)="([^"]*)".*/\1=\2/p' "$script")
  pinned() {
    local value
    value=$(sed -n "s/^$1=//p" <<<"$pins")
    [[ -n "$value" ]] || die "$1 is not in the pinned block of $script"
    echo "$value"
  }

  local stamped k3s image_repo chart_repo image chart_digest="" from safe when
  stamped=$(pinned KWERFT_VERSION_DEFAULT)
  [[ "$stamped" == "$v" ]] || die "$script is stamped with $stamped, not $v"
  k3s=$(pinned K3S_VERSION)
  [[ "$k3s" =~ ^v1\.([0-9]+)\.[0-9]+\+k3s[0-9]+$ ]] || die "K3S_VERSION $k3s does not look like v1.37.1+k3s1"
  # A release supports its pinned Kubernetes minor and the one before.
  local minor=${BASH_REMATCH[1]}
  image_repo=$(pinned KWERFT_IMAGE_REPO)
  chart_repo=$(pinned KWERFT_CHART_REPO)

  if [[ -f "$out/image.txt" ]]; then
    image=$(<"$out/image.txt")
    [[ "$image" =~ ^${image_repo//./\\.}@sha256:[0-9a-f]{64}$ ]] || die "$out/image.txt: '$image' is not $image_repo@sha256:…"
  elif [[ -n "$draft" ]]; then
    image="$image_repo:$v"
  else
    die "$out/image.txt is missing: push the image first (or --draft)"
  fi
  if [[ -f "$out/chart.txt" ]]; then
    chart_digest=$(<"$out/chart.txt")
    [[ "$chart_digest" =~ ^${chart_repo#oci://}@sha256:[0-9a-f]{64}$ ]] || die "$out/chart.txt: '$chart_digest' is not ${chart_repo#oci://}@sha256:…"
    chart_digest=${chart_digest#*@}
  elif [[ -z "$draft" ]]; then
    die "$out/chart.txt is missing: push the chart first (or --draft)"
  fi

  from=$(upgrade_from "$v")
  safe=$(rollback_safe "$v")
  when=$(published "$v")
  jq -n --arg version "$v" --arg channel "$(channel "$v")" --arg published "$when" \
    --arg upgradeFrom "$from" --argjson rollbackSafe "$safe" \
    --arg k3s "$k3s" --argjson minor "$minor" --arg pins "$pins" \
    --arg image "$image" --arg chartRef "$chart_repo" --arg chartDigest "$chart_digest" '
    # CILIUM_VERSION → cilium, CERT_MANAGER_VERSION → certManager,
    # TRAEFIK_CHART_VERSION → traefikChart
    def camel: ascii_downcase | split("_") | .[0] + ([.[1:][] | (.[0:1] | ascii_upcase) + .[1:]] | join(""));
    {
      version: $version,
      channel: $channel,
      published: $published,
      upgradeFrom: $upgradeFrom,
      rollbackSafe: $rollbackSafe,
      kubernetes: {pinned: $k3s, supported: ["1.\($minor - 1)", "1.\($minor)"]},
      components: ($pins | split("\n")
        | map(capture("^(?<k>[A-Z0-9_]+)_VERSION=(?<v>.+)$") | select(.k != "K3S") | {key: (.k | camel), value: .v})
        | from_entries),
      image: $image,
      chart: {ref: $chartRef, version: $version, digest: (if $chartDigest == "" then null else $chartDigest end)}
    }' >"$out/manifest.json"
  say "manifest.json: $(channel "$v"), upgrades from $from, rollback-safe $safe, k3s $k3s${draft:+ (draft: no digests)}"
}

# releases.json in the install repository: every release with a manifest,
# newest (by version) first.
update_index() {
  local dir=$1 manifest=$2 index="$1/releases.json" current='[]'
  [[ -f "$index" ]] && current=$(<"$index")
  jq --argjson m "$(jq -c '{version, channel, published}' "$manifest")" "$JQ_SEMVER"'
    map(select(.version != $m.version)) + [$m] | sort_by(.version | semkey) | reverse' <<<"$current" >"$index.tmp"
  mv "$index.tmp" "$index"
}

cmd_install_repo() {
  local dir=${1:?install repo checkout} v=${2:?version} files=${3:?dir with the stamped scripts, manifest.json and NOTES.md}
  check_version "$v"
  [[ -d "$dir/.git" ]] || die "$dir is not a git checkout"
  local f
  for f in install.sh join.sh SHA256SUMS manifest.json NOTES.md; do [[ -f "$files/$f" ]] || die "missing $files/$f"; done
  jq -e --arg v "$v" '.version == $v and (.image | contains("@sha256:")) and (.chart.digest != null)' "$files/manifest.json" >/dev/null \
    || [[ -n "${ALLOW_DRAFT_MANIFEST:-}" ]] \
    || die "$files/manifest.json is not the final manifest of $v (version, image and chart digests)"

  mkdir -p "$dir/v$v"
  cp "$files/install.sh" "$files/join.sh" "$files/SHA256SUMS" "$files/manifest.json" "$files/NOTES.md" "$dir/v$v/"
  update_index "$dir" "$files/manifest.json"
  # main's top level is the latest stable release; a patch for an older line
  # or a prerelease only gets its versioned directory.
  local latest=""
  [[ -f "$dir/LATEST" ]] && latest=$(<"$dir/LATEST")
  if ! is_prerelease "$v" && [[ "$(printf '%s\n%s\n' "${latest:-0.0.0}" "$v" | sort -V | tail -n1)" == "$v" ]]; then
    cp "$files/install.sh" "$files/join.sh" "$files/SHA256SUMS" "$dir/"
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
  cmd_crdcompat "$v"
  cmd_chart "$v" "$out"
  cmd_images "$v" "$out"
  cmd_notes "$v" "$out" >"$out/NOTES.md"
  cmd_manifest "$v" "$out" --draft
  # What the install repository would get, in a scratch repository.
  mkdir -p "$out/install-repo"
  git -C "$out/install-repo" -c init.defaultBranch=main init -q
  GIT_AUTHOR_NAME=release-dry-run GIT_AUTHOR_EMAIL=release-dry-run@localhost \
    GIT_COMMITTER_NAME=release-dry-run GIT_COMMITTER_EMAIL=release-dry-run@localhost \
    ALLOW_DRAFT_MANIFEST=1 cmd_install_repo "$out/install-repo" "$v" "$out"
  say "Release $v rehearsed in $out (nothing was published):"
  (cd "$out" && find . -path ./install-repo/.git -prune -o -type f -print | sort | sed 's|^\./|  |') >&2
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
    trailers)     cmd_trailers "$@" ;;
    crdcompat)    cmd_crdcompat "$@" ;;
    manifest)     cmd_manifest "$@" ;;
    *) sed -n '5,33p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2; exit 2 ;;
  esac
}

main "$@"
