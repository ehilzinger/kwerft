#!/usr/bin/env bats
# hack/release.sh: the parts that need neither a registry nor ko.

setup() {
  RELEASE="$BATS_TEST_DIRNAME/../../hack/release.sh"
}

@test "meta: a stable tag" {
  run "$RELEASE" meta refs/tags/v0.2.0
  [ "$status" -eq 0 ]
  [ "$output" = "$(printf 'version=0.2.0\ntag=v0.2.0\nprerelease=false')" ]
}

@test "meta: a release candidate is a prerelease" {
  run "$RELEASE" meta v0.2.0-rc.1
  [ "$status" -eq 0 ]
  [[ "$output" == *"version=0.2.0-rc.1"* ]]
  [[ "$output" == *"prerelease=true"* ]]
}

@test "meta: rejects tags that are not semver" {
  run "$RELEASE" meta v0.2
  [ "$status" -ne 0 ]
  run "$RELEASE" meta v0.2.0+build.1
  [ "$status" -ne 0 ]
}

@test "scripts: stamps both scripts with the release and checksums them" {
  out="$BATS_TEST_TMPDIR/out"
  run "$RELEASE" scripts 0.2.0 "$out"
  [ "$status" -eq 0 ]
  grep -qx 'KWERFT_VERSION_DEFAULT="0.2.0"' "$out/install.sh"
  grep -qx 'INSTALLER_URL="https://kwerft.dev/v0.2.0/install.sh"' "$out/join.sh"
  run "$out/install.sh" --help
  [[ "$output" == *"Kwerft installer 0.2.0"* ]]
  if command -v sha256sum >/dev/null; then
    (cd "$out" && sha256sum -c SHA256SUMS)
  else
    (cd "$out" && shasum -a 256 -c SHA256SUMS)
  fi
  # The version line is the only difference from the source.
  [ "$(diff "$BATS_TEST_DIRNAME/../install.sh" "$out/install.sh" | grep -c '^[<>]')" -eq 2 ]
}

# --- manifest, trailers, install repository, crdcompat ------------------------
# These run hack/release.sh in a scratch repository with its own tags, so the
# release history is under the test's control.

make_repo() {
  export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
  export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.com
  export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.com
  REPO="$BATS_TEST_TMPDIR/repo"
  mkdir -p "$REPO/hack" "$REPO/install" "$REPO/charts/kwerft/crds"
  cp "$BATS_TEST_DIRNAME/../../hack/release.sh" "$REPO/hack/"
  cp "$BATS_TEST_DIRNAME/../install.sh" "$BATS_TEST_DIRNAME/../join.sh" "$REPO/install/"
  echo "# crds" >"$REPO/charts/kwerft/crds/README.md"
  git -C "$REPO" -c init.defaultBranch=main init -q
  git -C "$REPO" add -A
  git -C "$REPO" commit -q -m initial
  RELEASE="$REPO/hack/release.sh"
}

# tag VERSION [MESSAGE]: an annotated tag made at a fixed time.
tag() {
  GIT_COMMITTER_DATE="2026-11-02T10:00:00Z" git -C "$REPO" tag -a "v$1" -m "${2:-Kwerft v$1}"
}

# fake_push VERSION OUT: the files push-image and push-chart write.
fake_push() {
  echo "ghcr.io/ehilzinger/kwerft@sha256:$(printf 'a%.0s' {1..64})" >"$2/image.txt"
  echo "ghcr.io/ehilzinger/charts/kwerft@sha256:$(printf 'b%.0s' {1..64})" >"$2/chart.txt"
}

# release VERSION OUT: everything install-repo needs, without a registry.
release() {
  "$RELEASE" scripts "$1" "$2"
  fake_push "$1" "$2"
  "$RELEASE" manifest "$1" "$2"
  "$RELEASE" notes "$1" "$2" >"$2/NOTES.md"
}

@test "trailers: default to two minor lines back and rollback-safe" {
  make_repo
  tag 0.3.0; tag 0.4.0; tag 0.4.1; tag 0.5.0-rc.1
  run "$RELEASE" trailers 0.6.0
  [ "$status" -eq 0 ]
  [ "$output" = "$(printf 'upgrade_from=0.4.0\nrollback_safe=true\nchannel=stable')" ]
  # A patch release counts from its own line; a prerelease is on edge.
  run "$RELEASE" trailers 0.4.2-rc.1
  [ "$status" -eq 0 ]
  [ "$output" = "$(printf 'upgrade_from=0.3.0\nrollback_safe=true\nchannel=edge')" ]
  # A major release counts the lines before it.
  run "$RELEASE" trailers 1.0.0
  [[ "$output" == *"upgrade_from=0.4.0"* ]] || false
}

@test "trailers: the first release upgrades only within its own line" {
  make_repo
  run "$RELEASE" trailers 0.1.0
  [ "$status" -eq 0 ]
  [[ "$output" == *"upgrade_from=0.1.0"* ]] || false
  [[ "$output" == *"No release tags before v0.1.0"* ]] || false
}

@test "trailers: the tag message overrides the defaults" {
  make_repo
  tag 0.4.0; tag 0.5.0
  tag 0.6.0 "$(printf 'Kwerft v0.6.0\n\nThe store moves to a new schema.\n\nupgrade-from: v0.5.0\nRollback-Safe: No')"
  run "$RELEASE" trailers 0.6.0
  [ "$status" -eq 0 ]
  [ "$output" = "$(printf 'upgrade_from=0.5.0\nrollback_safe=false\nchannel=stable')" ]
}

@test "trailers: a lightweight tag gets the defaults, with a warning" {
  make_repo
  tag 0.4.0; tag 0.5.0
  git -C "$REPO" tag v0.6.0
  run "$RELEASE" trailers 0.6.0
  [ "$status" -eq 0 ]
  [[ "$output" == *"lightweight tag"* ]] || false
  [[ "$output" == *"upgrade_from=0.4.0"* ]] || false
}

@test "trailers: rejects values that are not versions or yes/no" {
  make_repo
  tag 0.6.0 "$(printf 'Kwerft v0.6.0\n\nRollback-Safe: maybe')"
  run "$RELEASE" trailers 0.6.0
  [ "$status" -ne 0 ]
  [[ "$output" == *"Rollback-Safe must be yes or no"* ]] || false
  tag 0.7.0 "$(printf 'Kwerft v0.7.0\n\nUpgrade-From: 0.8.0')"
  run "$RELEASE" trailers 0.7.0
  [ "$status" -ne 0 ]
  [[ "$output" == *"newer than the release"* ]] || false
  tag 0.8.0 "$(printf 'Kwerft v0.8.0\n\nUpgrade-From: latest')"
  run "$RELEASE" trailers 0.8.0
  [ "$status" -ne 0 ]
}

@test "manifest: from the stamped installer's pinned block, the tag and the digests" {
  make_repo
  tag 0.4.0; tag 0.5.0
  tag 0.6.0 "$(printf 'Kwerft v0.6.0\n\nRollback-Safe: no')"
  out="$BATS_TEST_TMPDIR/out"
  "$RELEASE" scripts 0.6.0 "$out"
  fake_push 0.6.0 "$out"
  run "$RELEASE" manifest 0.6.0 "$out"
  [ "$status" -eq 0 ]
  m="$out/manifest.json"
  pin() { sed -n -E "s/^$1=\"([^\"]*)\".*/\1/p" "$BATS_TEST_DIRNAME/../install.sh"; }
  k3s=$(pin K3S_VERSION)
  minor=$(sed -E 's/^v1\.([0-9]+)\..*/\1/' <<<"$k3s")
  jq -e --arg k3s "$k3s" --arg cilium "$(pin CILIUM_VERSION)" --arg helm "$(pin HELM_VERSION)" \
    --arg cm "$(pin CERT_MANAGER_VERSION)" --arg traefik "$(pin TRAEFIK_CHART_VERSION)" \
    --arg prev "1.$((minor - 1))" --arg cur "1.$minor" '
      .version == "0.6.0" and .channel == "stable" and .published == "2026-11-02T10:00:00Z"
      and .upgradeFrom == "0.4.0" and .rollbackSafe == false
      and .kubernetes == {pinned: $k3s, supported: [$prev, $cur]}
      and .components.cilium == $cilium and .components.helm == $helm
      and .components.certManager == $cm and .components.traefikChart == $traefik
      and (.components | has("k3s") or has("kwerftVersionDefault") or has("hcloudLocations") | not)
      and .image == "ghcr.io/ehilzinger/kwerft@sha256:\("a" * 64)"
      and .chart == {ref: "oci://ghcr.io/ehilzinger/charts/kwerft", version: "0.6.0", digest: "sha256:\("b" * 64)"}
    ' "$m"
  # Every *_VERSION of the pinned block but k3s is a component.
  [ "$(jq '.components | length' "$m")" -eq "$(sed -n '/^readonly /q; /^[A-Z0-9_]*_VERSION="/p' "$BATS_TEST_DIRNAME/../install.sh" | grep -vc '^K3S_VERSION=')" ]
}

@test "manifest: refuses missing or foreign digests and a script of another version" {
  make_repo
  out="$BATS_TEST_TMPDIR/out"
  "$RELEASE" scripts 0.6.0 "$out"
  run "$RELEASE" manifest 0.6.0 "$out"
  [ "$status" -ne 0 ]
  [[ "$output" == *"image.txt is missing"* ]] || false
  # --draft (make release-dry-run): the tag instead of the digest, no chart digest.
  run "$RELEASE" manifest 0.6.0 "$out" --draft
  [ "$status" -eq 0 ]
  jq -e '.image == "ghcr.io/ehilzinger/kwerft:0.6.0" and .chart.digest == null and .channel == "stable"' "$out/manifest.json"
  echo "docker.io/someone/else@sha256:$(printf 'a%.0s' {1..64})" >"$out/image.txt"
  run "$RELEASE" manifest 0.6.0 "$out"
  [ "$status" -ne 0 ]
  fake_push 0.6.0 "$out"
  run "$RELEASE" manifest 0.7.0 "$out"
  [ "$status" -ne 0 ]
  [[ "$output" == *"stamped with 0.6.0, not 0.7.0"* ]] || false
}

@test "notes: hand-written notes from the tag, the upgrade path, rollback safety" {
  make_repo
  tag 0.4.0; tag 0.5.0
  tag 0.6.0 "$(printf 'Kwerft v0.6.0\n\nUpgrades from the console.\n\nUpgrade-From: 0.5.0\nRollback-Safe: no')"
  run "$RELEASE" notes 0.6.0 "$BATS_TEST_TMPDIR/out"
  [ "$status" -eq 0 ]
  [ "${lines[0]}" = "Upgrades from the console." ]
  [[ "$output" == *"| Upgrades from | 0.5.0 and later |"* ]] || false
  [[ "$output" == *"**Not rollback-safe.**"* ]] || false
  [[ "$output" != *"Upgrade-From"* ]] || false
  [[ "$output" != *"Kwerft v0.6.0"* ]] || false
  # Without a tag (a rehearsal) there are no hand-written notes.
  run "$RELEASE" notes 0.7.0 "$BATS_TEST_TMPDIR/out"
  [ "${lines[0]}" = "## Install" ]
  [[ "$output" != *"rollback-safe"* ]] || false
}

@test "install-repo: versioned files, releases.json newest first, LATEST for stable only" {
  make_repo
  tag 0.4.0; tag 0.5.0
  repo="$BATS_TEST_TMPDIR/kwerft-install"
  mkdir -p "$repo"
  git -C "$repo" -c init.defaultBranch=main init -q
  for v in 0.5.0 0.6.0-rc.1 0.4.1; do
    release "$v" "$BATS_TEST_TMPDIR/out-$v"
    "$RELEASE" install-repo "$repo" "$v" "$BATS_TEST_TMPDIR/out-$v"
    for f in install.sh join.sh SHA256SUMS manifest.json NOTES.md; do
      cmp "$BATS_TEST_TMPDIR/out-$v/$f" "$repo/v$v/$f"
    done
  done
  [ "$(cat "$repo/LATEST")" = "0.5.0" ]
  cmp "$BATS_TEST_TMPDIR/out-0.5.0/install.sh" "$repo/install.sh"
  [ ! -e "$repo/manifest.json" ]
  jq -e '[.[].version] == ["0.6.0-rc.1", "0.5.0", "0.4.1"]
         and [.[].channel] == ["edge", "stable", "stable"]
         and all(.[]; keys == ["channel", "published", "version"])' "$repo/releases.json"
  [ "$(git -C "$repo" rev-list --count HEAD)" -eq 3 ]

  # Publishing the same release again changes nothing.
  run "$RELEASE" install-repo "$repo" 0.5.0 "$BATS_TEST_TMPDIR/out-0.5.0"
  [ "$status" -eq 0 ]
  [[ "$output" == *"already has v0.5.0"* ]] || false
  [ "$(jq length "$repo/releases.json")" -eq 3 ]

  # The stable release after its candidate sorts above it.
  release 0.6.0 "$BATS_TEST_TMPDIR/out-0.6.0"
  "$RELEASE" install-repo "$repo" 0.6.0 "$BATS_TEST_TMPDIR/out-0.6.0"
  jq -e '[.[].version] == ["0.6.0", "0.6.0-rc.1", "0.5.0", "0.4.1"]' "$repo/releases.json"
  [ "$(cat "$repo/LATEST")" = "0.6.0" ]
}

@test "install-repo: refuses a draft manifest or one of another version" {
  make_repo
  repo="$BATS_TEST_TMPDIR/kwerft-install"
  mkdir -p "$repo"
  git -C "$repo" -c init.defaultBranch=main init -q
  out="$BATS_TEST_TMPDIR/out"
  "$RELEASE" scripts 0.6.0 "$out"
  "$RELEASE" manifest 0.6.0 "$out" --draft
  "$RELEASE" notes 0.6.0 "$out" >"$out/NOTES.md"
  run "$RELEASE" install-repo "$repo" 0.6.0 "$out"
  [ "$status" -ne 0 ]
  [[ "$output" == *"not the final manifest"* ]] || false
  fake_push 0.6.0 "$out"
  "$RELEASE" manifest 0.6.0 "$out"
  run "$RELEASE" install-repo "$repo" 0.6.1 "$out"
  [ "$status" -ne 0 ]
  rm "$out/NOTES.md"
  run "$RELEASE" install-repo "$repo" 0.6.0 "$out"
  [ "$status" -ne 0 ]
  [[ "$output" == *"missing $out/NOTES.md"* ]] || false
}

@test "crdcompat: compares with the previous stable release and prerelease" {
  make_repo
  stub="$BATS_TEST_TMPDIR/crdcompat"
  printf '#!/usr/bin/env bash\necho "$@" >>"%s/calls"\nexit "${STUB_EXIT:-0}"\n' "$BATS_TEST_TMPDIR" >"$stub"
  chmod +x "$stub"
  export CRDCOMPAT="$stub"
  tag 0.4.0; tag 0.5.0; tag 0.6.0-rc.1
  run "$RELEASE" crdcompat 0.6.0
  [ "$status" -eq 0 ]
  [ "$(wc -l <"$BATS_TEST_TMPDIR/calls")" -eq 2 ]
  grep -q -- '-name v0.5.0 .*/0.5.0/charts/kwerft/crds charts/kwerft/crds$' "$BATS_TEST_TMPDIR/calls"
  grep -q -- '-name v0.6.0-rc.1 ' "$BATS_TEST_TMPDIR/calls"

  # Incompatible: the release stops, unless its tag says Rollback-Safe: no.
  run env STUB_EXIT=1 "$RELEASE" crdcompat 0.6.0
  [ "$status" -ne 0 ]
  [[ "$output" == *"Rollback-Safe: no"* ]] || false
  tag 0.6.0 "$(printf 'Kwerft v0.6.0\n\nRollback-Safe: no')"
  run env STUB_EXIT=1 "$RELEASE" crdcompat 0.6.0
  [ "$status" -eq 0 ]
  [[ "$output" == *"accepted"* ]] || false
  # A failure of the tool itself is never accepted.
  run env STUB_EXIT=2 "$RELEASE" crdcompat 0.6.0
  [ "$status" -ne 0 ]

  # The first release has nothing to compare with.
  rm "$BATS_TEST_TMPDIR/calls"
  run "$RELEASE" crdcompat 0.1.0
  [ "$status" -eq 0 ]
  [ ! -e "$BATS_TEST_TMPDIR/calls" ]
}
