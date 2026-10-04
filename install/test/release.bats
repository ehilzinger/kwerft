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
  grep -qx 'INSTALLER_URL="https://raw.githubusercontent.com/ehilzinger/kwerft-install/main/v0.2.0/install.sh"' "$out/join.sh"
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
