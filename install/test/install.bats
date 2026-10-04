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

# setup_token_state reads the Secret through kc; these tests stub it.
setup_state_env() {
  KWERFT_SOURCED=1 source "$SCRIPT"
  SETUP_TOKEN_FILE="$BATS_TEST_TMPDIR/setup-token"
  CONFIG_FILE=""
}
secret_expires() { printf '%s' "$1" | base64; }

@test "setup_token_state: first install has no token" {
  setup_state_env
  kc() { return 1; }
  [ "$(setup_token_state)" = "missing" ]
}

@test "setup_token_state: valid token is pending" {
  setup_state_env
  echo kwft_setup_x >"$SETUP_TOKEN_FILE"
  kc() { secret_expires "2999-01-01T00:00:00Z"; }
  [ "$(setup_token_state)" = "pending" ]
}

@test "setup_token_state: expired token is replaced" {
  setup_state_env
  echo kwft_setup_x >"$SETUP_TOKEN_FILE"
  kc() { secret_expires "2000-01-01T00:00:00Z"; }
  [ "$(setup_token_state)" = "expired" ]
}

@test "setup_token_state: consumed token means setup is complete" {
  setup_state_env
  echo kwft_setup_x >"$SETUP_TOKEN_FILE"
  kc() { return 1; }
  [ "$(setup_token_state)" = "complete" ]
}

@test "setup_token_state: config file skips the token" {
  setup_state_env
  CONFIG_FILE=/root/kwerft.yaml
  [ "$(setup_token_state)" = "config" ]
}

@test "stage_handoff: a domain without a DNS record warns instead of failing" {
  setup_state_env
  mkdir() { :; }; chmod() { :; }   # CONF_DIR is readonly /etc/kwerft
  DOMAIN="console.kwerft.test"; PUBLIC_IP="203.0.113.24"
  getent() { return 2; }   # glibc: key not found
  setup_token_state() { echo config; }
  # Called directly, not via `run` or $(...): errexit must stay on (bash 3.2
  # drops it inside command substitution), since errexit is what killed the stage.
  stage_handoff >"$BATS_TEST_TMPDIR/out" 2>&1
  output=$(<"$BATS_TEST_TMPDIR/out")
  [[ "$output" == *"console.kwerft.test resolves to 'nothing', expected 203.0.113.24."* ]]
  [[ "$output" == *"DNS unresolved · owner from config"* ]]
}

@test "detect_addresses: a missing --private-iface is a preflight error, not a crash" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  PRIVATE_IFACE="eth9"
  ip() { [[ "$*" == *"dev eth9"* ]] && return 1; return 0; }   # iproute2: device does not exist
  # errexit has to be live inside the subshell, so no `run`, `||` or `!` here.
  set +e
  (set -e; detect_addresses) >"$BATS_TEST_TMPDIR/out" 2>&1
  status=$?
  set -e
  [ "$status" -eq 10 ]
  [[ "$(<"$BATS_TEST_TMPDIR/out")" == *"Interface eth9 has no IPv4 address"* ]]
}

@test "--version accepts a leading v" {
  run "$SCRIPT" --dry-run --platform cloud --domain ops.example.com --version v0.2.0
  [ "$status" -eq 0 ]
  [[ "$output" == *"Kwerft installer 0.2.0 "* ]]
}

@test "--version rejects anything that is not a release version" {
  run "$SCRIPT" --dry-run --platform cloud --version latest
  [ "$status" -eq 2 ]
  [[ "$output" == *"--version must look like 0.2.0"* ]]
}

# check_release asks the registry through oci_manifest_status; these tests stub it.
release_env() {
  KWERFT_SOURCED=1 source "$SCRIPT"
  KWERFT_CHART="oci://ghcr.io/ehilzinger/charts/kwerft"   # as when piped from curl
  KWERFT_VERSION="0.2.0"; IMAGE=""
  : >"$BATS_TEST_TMPDIR/lookups"
}
lookups() { cat "$BATS_TEST_TMPDIR/lookups"; }

@test "check_release: a published release passes; chart and image are both checked" {
  release_env
  oci_manifest_status() { echo "$*" >>"$BATS_TEST_TMPDIR/lookups"; echo 200; }
  check_release
  [ "$(lookups)" = "$(printf '%s\n' 'ghcr.io ehilzinger/charts/kwerft 0.2.0' 'ghcr.io ehilzinger/kwerft 0.2.0')" ]
}

@test "check_release: an unpublished version exits 50" {
  release_env
  oci_manifest_status() { echo 404; }
  run check_release
  [ "$status" -eq 50 ]
  [[ "$output" == *"Kwerft 0.2.0 is not published"* ]]
}

@test "check_release: a private package exits 50 and says so" {
  release_env
  oci_manifest_status() { if [[ "$2" == ehilzinger/kwerft ]]; then echo 403; else echo 200; fi; }
  run check_release
  [ "$status" -eq 50 ]
  [[ "$output" == *"Cannot pull ghcr.io/ehilzinger/kwerft:0.2.0 anonymously"* ]]
  [[ "$output" == *"still private"* ]]
}

@test "check_release: an unreachable registry is a network error" {
  release_env
  oci_manifest_status() { echo 000; }
  run check_release
  [ "$status" -eq 20 ]
}

@test "check_release: --image skips the image, a checkout skips everything" {
  release_env
  oci_manifest_status() { echo "$*" >>"$BATS_TEST_TMPDIR/lookups"; echo 200; }
  IMAGE="registry.local:5000/kwerft:dev-1"
  check_release
  [ "$(lookups)" = "ghcr.io ehilzinger/charts/kwerft 0.2.0" ]
  : >"$BATS_TEST_TMPDIR/lookups"; KWERFT_CHART=""   # chart_ref finds ../charts/kwerft
  check_release
  [ -z "$(lookups)" ]
}
