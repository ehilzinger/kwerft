#!/usr/bin/env bats
# Unit tests for install.sh that run anywhere (no root, no Ubuntu needed).
# Stage behaviour on real servers is covered by test/e2e.

setup() {
  SCRIPT="$BATS_TEST_DIRNAME/../install.sh"
}

@test "--help prints usage and exits 0" {
  run "$SCRIPT" --help
  [ "$status" -eq 0 ]
  [[ "$output" == *"Usage: install.sh"* ]]
}

@test "unknown option exits with usage code 2" {
  run "$SCRIPT" --bogus
  [ "$status" -eq 2 ]
  [[ "$output" == *"Unknown option: --bogus"* ]]
}

@test "flag without value exits 2" {
  run "$SCRIPT" --domain
  [ "$status" -eq 2 ]
  [[ "$output" == *"--domain needs a value"* ]]
}

@test "--join requires --token" {
  run "$SCRIPT" --join https://ops.example.com
  [ "$status" -eq 2 ]
  [[ "$output" == *"--join needs --token"* ]]
}

@test "invalid --platform is rejected" {
  run "$SCRIPT" --platform aws
  [ "$status" -eq 2 ]
}

@test "--dry-run lists every install stage in order" {
  run "$SCRIPT" --dry-run --platform cloud --domain ops.example.com
  [ "$status" -eq 0 ]
  expected="Preflight System Firewall Kubernetes Helm Network Ingress Observability Kwerft Handoff"
  actual=$(printf '%s\n' "$output" | sed -n 's/^→ \([A-Za-z]*\).*/\1/p' | tr '\n' ' ' | sed 's/ $//')
  [ "$actual" = "$expected" ]
}

@test "--dry-run in join mode stops after joining" {
  run "$SCRIPT" --dry-run --platform dedicated --join https://ops.example.com --token t
  [ "$status" -eq 0 ]
  [[ "$output" == *"Join cluster"* ]]
  [[ "$output" != *"Kubernetes"* ]]
}

@test "config_get reads top-level scalars and ignores comments and quotes" {
  cfg="$BATS_TEST_TMPDIR/kwerft.yaml"
  printf 'domain: "ops.example.com"   # console\nemail: ops@example.com\nowner:\n  email: nested@example.com\n' >"$cfg"
  KWERFT_SOURCED=1 source "$SCRIPT"
  CONFIG_FILE=$cfg
  [ "$(config_get domain)" = "ops.example.com" ]
  [ "$(config_get email)" = "ops@example.com" ]
}

@test "network_of masks host bits" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  [ "$(network_of 10.0.1.3/16)" = "10.0.0.0/16" ]
  [ "$(network_of 192.168.100.7/24)" = "192.168.100.0/24" ]
  [ "$(network_of 172.16.5.9/12)" = "172.16.0.0/12" ]
}

@test "resolve_domain: --domain wins over a saved hostname" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  DOMAIN_FILE="$BATS_TEST_TMPDIR/domain"; echo "old.example.com" >"$DOMAIN_FILE"
  DOMAIN="ops.example.com"; PUBLIC_IP="203.0.113.24"
  resolve_domain
  [ "$DOMAIN" = "ops.example.com" ]
  run is_temp_domain
  [ "$status" -ne 0 ]
}

@test "resolve_domain: re-running without --domain keeps the saved hostname" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  DOMAIN_FILE="$BATS_TEST_TMPDIR/domain"; echo "ops.example.com" >"$DOMAIN_FILE"
  DOMAIN=""; PUBLIC_IP="203.0.113.24"
  resolve_domain
  [ "$DOMAIN" = "ops.example.com" ]
}

@test "resolve_domain: falls back to a temporary sslip.io hostname" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  DOMAIN_FILE="$BATS_TEST_TMPDIR/missing"
  DOMAIN=""; PUBLIC_IP="203.0.113.24"
  resolve_domain
  [ "$DOMAIN" = "203.0.113.24.sslip.io" ]
  is_temp_domain
}

@test "--image needs a tag" {
  run "$SCRIPT" --image ghcr.io/ehilzinger/kwerft
  [ "$status" -eq 2 ]
  [[ "$output" == *"--image needs a tag"* ]]
}

@test "--image-archive needs --image" {
  archive="$BATS_TEST_TMPDIR/kwerft.tar"; touch "$archive"
  run "$SCRIPT" --image-archive "$archive"
  [ "$status" -eq 2 ]
  [[ "$output" == *"--image-archive needs --image"* ]]
}

@test "--image with a registry port keeps the port in the repository" {
  archive="$BATS_TEST_TMPDIR/kwerft.tar"; touch "$archive"
  run "$SCRIPT" --dry-run --platform cloud --image registry.local:5000/kwerft:dev-1 --image-archive "$archive"
  [ "$status" -eq 0 ]
}
