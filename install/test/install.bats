#!/usr/bin/env bats
# SPDX-FileCopyrightText: 2026 Enzo Hilzinger
# SPDX-License-Identifier: AGPL-3.0-only

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
  expected="Preflight System Firewall Kubernetes Registry Helm Upgrades Network Hetzner Ingress Observability Backups Kwerft Handoff"
  actual=$(printf '%s\n' "$output" | sed -n 's/^→ \([A-Za-z]*\).*/\1/p' | tr '\n' ' ' | sed 's/ $//')
  [ "$actual" = "$expected" ]
}

@test "--dry-run in join mode stops after joining" {
  run "$SCRIPT" --dry-run --platform dedicated --join https://ops.example.com --token t
  [ "$status" -eq 0 ]
  [[ "$output" == *"Join cluster"* ]]
  [[ "$output" == *"Registry mirror"* ]]
  [[ "$output" != *"Kubernetes"* ]]
}

@test "node pools' cloud-init flags: --node-label and --node-taint are checked" {
  run "$SCRIPT" --dry-run --platform cloud --join https://ops.example.com --token t --role worker \
    --node-label kwerft.dev/pool=prod-builds --node-label tier=web --node-taint kwerft.dev/builds=true:NoSchedule --yes
  [ "$status" -eq 0 ]
  [[ "$output" == *"Join cluster"* ]]
  run "$SCRIPT" --dry-run --join https://ops.example.com --token t --node-label 'x=y: z'
  [ "$status" -eq 2 ]
  [[ "$output" == *"--node-label must look like key=value"* ]]
  run "$SCRIPT" --dry-run --join https://ops.example.com --token t --node-taint kwerft.dev/builds=true
  [ "$status" -eq 2 ]
  [[ "$output" == *"--node-taint must look like"* ]]
}

@test "node_settings writes the platform label, node labels and taints" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  PLATFORM=cloud
  [ "$(node_settings)" = $'node-label:\n  - kwerft.dev/platform=cloud' ]
  NODE_LABELS=(kwerft.dev/pool=prod-builds kwerft.dev/builds=true)
  NODE_TAINTS=(kwerft.dev/builds=true:NoSchedule)
  expected=$'node-label:\n  - kwerft.dev/platform=cloud\n  - kwerft.dev/pool=prod-builds\n  - kwerft.dev/builds=true\nnode-taint:\n  - kwerft.dev/builds=true:NoSchedule'
  [ "$(node_settings)" = "$expected" ]
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

@test "temp_domain_notice: an explicit sslip.io --domain is not 'No --domain given'" {
  run "$SCRIPT" --dry-run --platform cloud --domain 203.0.113.24.sslip.io
  [ "$status" -eq 0 ]
  [[ "$output" != *"No --domain given"* ]]
  [[ "$output" == *"203.0.113.24.sslip.io is a temporary hostname"* ]]
}

@test "temp_domain_notice: the fallback says no --domain was given" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  DOMAIN="203.0.113.24.sslip.io"; DOMAIN_EXPLICIT=0
  run temp_domain_notice
  [[ "$output" == *"No --domain given: using temporary hostname 203.0.113.24.sslip.io"* ]]
  DOMAIN="ops.example.com"
  run temp_domain_notice
  [ -z "$output" ]
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

@test "setup_token_state: the console saying setup is complete wins" {
  setup_state_env
  # No token file and no Secret: a re-run after a re-run once setup was done.
  kc() { if [[ "$*" == *"/proxy/api/v1/setup"* ]]; then echo '{"complete":true}'; else return 1; fi; }
  [ "$(setup_token_state)" = "complete" ]
}

@test "setup_token_state: a console without an owner falls back to the token" {
  setup_state_env
  kc() { if [[ "$*" == *"/proxy/api/v1/setup"* ]]; then echo '{"complete":false}'; else return 1; fi; }
  [ "$(setup_token_state)" = "missing" ]
}

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

@test "setup_token_state: --config without an owner still gets a setup token" {
  setup_state_env
  CONFIG_FILE=/root/kwerft.yaml
  kc() { return 1; }
  [ "$(setup_token_state)" = "missing" ]
}

@test "setup_token_state: an owner from --config waits in kwerft-bootstrap" {
  setup_state_env
  CONFIG_FILE=/root/kwerft.yaml
  kc() { if [[ "$*" == *"get secret kwerft-bootstrap"*"{.metadata.name}"* ]]; then echo kwerft-bootstrap; else return 1; fi; }
  [ "$(setup_token_state)" = "config" ]
}

@test "stage_handoff: a domain without a DNS record warns instead of failing" {
  setup_state_env
  mkdir() { :; }; chmod() { :; }   # CONF_DIR is readonly /etc/kwerft
  DOMAIN="console.kwerft.test"; PUBLIC_IP="203.0.113.24"
  getent() { return 2; }   # glibc: key not found
  kc() { return 0; }
  setup_token_state() { echo complete; }
  # Called directly, not via `run` or $(...): errexit must stay on (bash 3.2
  # drops it inside command substitution), since errexit is what killed the stage.
  stage_handoff >"$BATS_TEST_TMPDIR/out" 2>&1
  output=$(<"$BATS_TEST_TMPDIR/out")
  [[ "$output" == *"console.kwerft.test resolves to 'nothing', expected 203.0.113.24."* ]]
  [[ "$output" == *"DNS unresolved · setup complete"* ]]
}

# ---------------------------------------------------------------------------
# The owner from --config: the installer writes Secret kwerft-bootstrap, the
# console creates the owner and replaces the password with the outcome
# (internal/setup/owner.go), Handoff reports it and deletes the Secret.
# ---------------------------------------------------------------------------

owner_config() {
  local cfg="$BATS_TEST_TMPDIR/kwerft.yaml" pw="$BATS_TEST_TMPDIR/owner.pw"
  printf '%s\n' "${2-correct horse battery staple}" >"$pw"
  printf 'domain: ops.example.com\n%s\n' "${1//PW/$pw}" >"$cfg"
  echo "$cfg"
}

@test "--config owner: checked before anything is installed" {
  run "$SCRIPT" --dry-run --platform cloud --config "$(owner_config 'owner: { email: you@example.com, passwordFile: PW }')"
  [ "$status" -eq 0 ]
  run "$SCRIPT" --dry-run --platform cloud --config "$(owner_config $'owner:\n  email: you@example.com\n  name: Ada Lovelace\n  passwordFile: PW')"
  [ "$status" -eq 0 ]
  run "$SCRIPT" --dry-run --platform cloud --config "$(owner_config 'owner: { email: you@example.com }')"
  [ "$status" -eq 2 ]
  [[ "$output" == *"owner.passwordFile in"*"not readable"* ]]
  run "$SCRIPT" --dry-run --platform cloud --config "$(owner_config 'owner: { email: you, passwordFile: PW }')"
  [ "$status" -eq 2 ]
  [[ "$output" == *"owner.email in"*"must be an address like you@example.com, got 'you'"* ]]
  run "$SCRIPT" --dry-run --platform cloud --config "$(owner_config 'owner: { email: you@example.com, passwordFile: PW }' 'short')"
  [ "$status" -eq 2 ]
  [[ "$output" == *"must hold a password of 12 to 256 characters"* ]]
  [[ "$output" != *"short"*"short"* ]]
}

# kc stub recording its calls and keeping Secret kwerft-bootstrap as files in
# $SECRET_DIR (one per key); the console's answer is written there by a test.
owner_env() {
  setup_state_env
  LOG_FILE="$BATS_TEST_TMPDIR/install.log"
  KC_LOG="$BATS_TEST_TMPDIR/kc.log"; : >"$KC_LOG"
  SECRET_DIR="$BATS_TEST_TMPDIR/kwerft-bootstrap"
  OWNER_TIMEOUT=0; OWNER_POLL=0; COMPLETE=false
  kc() {
    printf 'kc %s\n' "$*" >>"$KC_LOG"
    case "$*" in
      *"/proxy/api/v1/setup"*) echo "{\"complete\":$COMPLETE}" ;;
      *"get secret kwerft-bootstrap"*"{.metadata.name}"*) [[ ! -d "$SECRET_DIR" ]] || echo kwerft-bootstrap ;;
      *"get secret kwerft-bootstrap"*"{.data."*)
        [[ "$*" =~ \{\.data\.([a-z]+)\} ]]
        [[ ! -f "$SECRET_DIR/${BASH_REMATCH[1]}" ]] || base64 <"$SECRET_DIR/${BASH_REMATCH[1]}" ;;
      *"delete secret kwerft-bootstrap"*) rm -rf "$SECRET_DIR" ;;
      *"apply"*) cat >>"$KC_LOG" ;;
    esac
    return 0
  }
}

# console_answers <key=value>...: what the console left in the Secret.
console_answers() {
  rm -rf "$SECRET_DIR"; command mkdir -p "$SECRET_DIR"
  local kv
  for kv in "$@"; do printf '%s' "${kv#*=}" >"$SECRET_DIR/${kv%%=*}"; done
}

handoff() {
  mkdir() { :; }; chmod() { :; }   # CONF_DIR is readonly /etc/kwerft
  DOMAIN="console.kwerft.test"; PUBLIC_IP="203.0.113.24"
  getent() { echo "203.0.113.24 STREAM console.kwerft.test"; }
  create_setup_token() { echo kwft_setup_x >"$SETUP_TOKEN_FILE"; }   # GNU date and sha256sum
  stage_handoff >"$BATS_TEST_TMPDIR/out" 2>&1
  output=$(<"$BATS_TEST_TMPDIR/out")
}

@test "write_owner_secret: the password goes from its file, by server-side apply" {
  owner_env
  OWNER_EMAIL=you@example.com; OWNER_NAME="Ada Lovelace"; OWNER_PASSWORD_FILE="$BATS_TEST_TMPDIR/owner.pw"
  printf 'correct horse battery staple\n' >"$OWNER_PASSWORD_FILE"
  write_owner_secret
  grep -q -- "create secret generic kwerft-bootstrap --from-literal=email=you@example.com --from-literal=name=Ada Lovelace --from-file=password=$OWNER_PASSWORD_FILE" "$KC_LOG"
  grep -q -- "apply --server-side --force-conflicts --field-manager=kwerft-installer -f -" "$KC_LOG"
  absent "correct horse" "$KC_LOG"
  absent "delete secret kwerft-bootstrap" "$KC_LOG"
}

@test "write_owner_secret: nothing to hand over once setup is complete, or without an owner" {
  owner_env
  OWNER_EMAIL=you@example.com; OWNER_PASSWORD_FILE="$BATS_TEST_TMPDIR/owner.pw"; COMPLETE=true
  write_owner_secret
  absent "create secret" "$KC_LOG"
  grep -q "delete secret kwerft-bootstrap --ignore-not-found" "$KC_LOG"
  : >"$KC_LOG"; COMPLETE=false; OWNER_EMAIL=""
  write_owner_secret
  absent "create secret" "$KC_LOG"
  grep -q "delete secret kwerft-bootstrap --ignore-not-found" "$KC_LOG"
  : >"$KC_LOG"; OWNER_EMAIL=you@example.com; RESTORE_FROM=latest
  write_owner_secret
  absent "create secret" "$KC_LOG"
}

@test "stage_handoff: the owner from --config was created, no setup token" {
  owner_env
  echo kwft_setup_old >"$SETUP_TOKEN_FILE"
  console_answers email=you@example.com status=created
  handoff
  [[ "$output" == *"DNS 203.0.113.24 · owner you@example.com from --config"* ]]
  [ ! -e "$SECRET_DIR" ]
  [ ! -e "$SETUP_TOKEN_FILE" ]
  grep -q "delete secret kwerft-setup-token" "$KC_LOG"
}

@test "stage_handoff: an owner the console rejected falls back to a setup token" {
  owner_env
  console_answers email=you@example.com status=rejected "reason=the password in owner.passwordFile does not qualify: use at least 12 characters"
  handoff
  [[ "$output" == *"did not create the owner from the config: the password in owner.passwordFile does not qualify: use at least 12 characters. Finish setup with the setup token"* ]]
  [[ "$output" == *"DNS 203.0.113.24 · owner: setup token ready"* ]]
  [ ! -e "$SECRET_DIR" ]
  [ -s "$SETUP_TOKEN_FILE" ]
}

@test "stage_handoff: a console that never takes the owner: the password goes, a setup token comes" {
  owner_env
  CONFIG_FILE=/root/kwerft.yaml
  console_answers email=you@example.com password=pw
  handoff
  [[ "$output" == *"did not create the owner from /root/kwerft.yaml within 0s"* ]]
  [[ "$output" == *"· owner: setup token ready"* ]]
  [ ! -e "$SECRET_DIR" ]
  [ -s "$SETUP_TOKEN_FILE" ]
}

@test "stage_handoff: setup already complete, the owner from --config is not created" {
  owner_env
  console_answers email=you@example.com status=exists
  handoff
  [[ "$output" == *"Setup was already complete"* ]]
  [[ "$output" == *"· setup complete"* ]]
  [ ! -e "$SECRET_DIR" ]
}

@test "print_summary: an owner from --config signs in, a setup token wins when there is one" {
  summary_env
  CONFIG_FILE=/root/kwerft.yaml; OWNER_EMAIL=you@example.com
  run print_summary
  [[ "$output" == *"Sign in with the owner account from /root/kwerft.yaml (you@example.com)"* ]]
  [[ "$output" != *"/setup"* ]]
  SETUP_TOKEN_FILE="$BATS_TEST_TMPDIR/setup-token"; echo kwft_setup_x >"$SETUP_TOKEN_FILE"
  run print_summary
  [[ "$output" == *"https://ops.example.com/setup"* ]]
  [[ "$output" != *"owner account"* ]]
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

@test "detect_addresses: never takes the cluster's own interfaces for the private network" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  PRIVATE_IFACE=""
  ADDRS=""
  ip() {
    case "$*" in
      *"route get"*) echo "1.1.1.1 via 172.31.1.1 dev eth0 src 203.0.113.7 uid 0" ;;
      *"route show default"*) echo "default via 172.31.1.1 dev eth0 proto dhcp" ;;
      *"addr show scope global"*) printf '%b' "$ADDRS" ;;
    esac
  }
  # A Cloud server without a Cloud Network, with the cluster running: none.
  ADDRS="2: eth0    inet 203.0.113.7/32 scope global dynamic eth0\n5: cilium_host    inet 10.42.0.199/32 scope global cilium_host\n7: lxc1234    inet 10.42.0.5/32 scope global lxc1234\n9: kube-ipvs0    inet 10.43.0.1/32 scope global kube-ipvs0\n"
  detect_addresses
  [ -z "$PRIVATE_CIDR" ] && [ -z "$PRIVATE_IFACE" ] && [ -z "$PRIVATE_IP" ]
  # With a Cloud Network, after Cilium's interfaces: that one.
  ADDRS="2: eth0    inet 203.0.113.7/32 scope global dynamic eth0\n5: cilium_host    inet 10.42.0.199/32 scope global cilium_host\n6: enp7s0    inet 10.0.0.2/32 scope global dynamic enp7s0\n"
  PRIVATE_IFACE=""
  detect_addresses
  [ "$PRIVATE_IFACE" = "enp7s0" ] && [ "$PRIVATE_IP" = "10.0.0.2" ]
  # A vSwitch VLAN interface on a dedicated server.
  ADDRS="2: enp0s31f6    inet 203.0.113.7/26 scope global enp0s31f6\n4: enp0s31f6.4000    inet 10.0.64.2/24 scope global enp0s31f6.4000\n"
  PRIVATE_IFACE=""
  detect_addresses
  [ "$PRIVATE_IFACE" = "enp0s31f6.4000" ] && [ "$PRIVATE_CIDR" = "10.0.64.2/24" ]
}

@test "private_network: the whole network the nodes share, never this server's /32" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  ROUTES=""
  ip() { [[ "$*" == *"route show dev enp7s0"* ]] && printf '%b' "$ROUTES"; return 0; }
  # Hetzner Cloud: the metadata service's network range wins.
  PRIVATE_CIDR="10.0.0.2/32"; PRIVATE_IFACE=enp7s0; HCLOUD_NETWORK_RANGE="10.0.0.0/16"
  private_network
  [ "$PRIVATE_NETWORK" = "10.0.0.0/16" ]
  # Without metadata: the route through the private interface.
  HCLOUD_NETWORK_RANGE=""; ROUTES="10.0.0.0/16 via 10.0.0.1 proto dhcp src 10.0.0.2 metric 1003\n10.0.0.1 proto dhcp scope link src 10.0.0.2 metric 1003\n"
  private_network
  [ "$PRIVATE_NETWORK" = "10.0.0.0/16" ]
  # A vSwitch VLAN interface: its own subnet.
  PRIVATE_CIDR="10.0.64.2/24"; PRIVATE_IFACE=vlan4000; ROUTES=""
  private_network
  [ "$PRIVATE_NETWORK" = "10.0.64.0/24" ]
  # None.
  PRIVATE_CIDR=""
  private_network
  [ -z "$PRIVATE_NETWORK" ]
  # The firewall admits the whole network.
  PRIVATE_CIDR="10.0.0.2/32"; PRIVATE_IFACE=enp7s0; HCLOUD_NETWORK_RANGE="10.0.0.0/16"
  private_network
  firewall_ruleset | grep -q "ip saddr 10.0.0.0/16 accept"
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

@test "--acme-server staging selects Let's Encrypt staging and says so" {
  run "$SCRIPT" --dry-run --platform cloud --domain ops.example.com --acme-server staging
  [ "$status" -eq 0 ]
  [[ "$output" == *"Let's Encrypt staging"* ]]
  KWERFT_SOURCED=1 source "$SCRIPT"
  parse_args --acme-server staging
  [ "$ACME_SERVER" = "https://acme-staging-v02.api.letsencrypt.org/directory" ]
}

@test "KWERFT_ACME_SERVER works like --acme-server; without either the chart's default stays" {
  KWERFT_ACME_SERVER=staging KWERFT_SOURCED=1 source "$SCRIPT"
  parse_args
  [ "$ACME_SERVER" = "https://acme-staging-v02.api.letsencrypt.org/directory" ]
  run env -u KWERFT_ACME_SERVER "$SCRIPT" --dry-run --platform cloud --domain ops.example.com
  [ "$status" -eq 0 ]
  [[ "$output" != *"ACME"* && "$output" != *"staging"* ]]
}

@test "--acme-server must be an https URL" {
  run "$SCRIPT" --dry-run --platform cloud --acme-server http://acme.example.com/directory
  [ "$status" -eq 2 ]
  [[ "$output" == *"--acme-server must be an https:// ACME directory URL or staging"* ]]
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

# ---------------------------------------------------------------------------
# Console settings: the cluster's ConsoleSettings "kwerft" records the console
# hostname and apps domain; the Settings page edits the same object.
# ---------------------------------------------------------------------------

@test "config_get_in reads inline mappings" {
  cfg="$BATS_TEST_TMPDIR/kwerft.yaml"
  printf 'domain: ops.example.com\ndns: { solver: hetzner, tokenFile: "/root/dns.token" }   # wildcard\nowner: { email: a@example.com }\n' >"$cfg"
  KWERFT_SOURCED=1 source "$SCRIPT"
  CONFIG_FILE=$cfg
  [ "$(config_get_in dns solver)" = "hetzner" ]
  [ "$(config_get_in dns tokenFile)" = "/root/dns.token" ]
  [ "$(config_get_in owner solver)" = "" ]
  [ "$(config_get_in backups solver)" = "" ]
}

@test "config_get_in reads block mappings" {
  cfg="$BATS_TEST_TMPDIR/kwerft.yaml"
  printf 'dns:\n  solver: hetzner   # the only one\n  tokenFile: /root/dns.token\nappsDomain: apps.example.com\nother:\n  solver: nope\n' >"$cfg"
  KWERFT_SOURCED=1 source "$SCRIPT"
  CONFIG_FILE=$cfg
  [ "$(config_get_in dns solver)" = "hetzner" ]
  [ "$(config_get_in dns tokenFile)" = "/root/dns.token" ]
  [ "$(config_get appsDomain)" = "apps.example.com" ]
}

write_config() {
  token="$BATS_TEST_TMPDIR/dns.token"; printf 'hz-token\n' >"$token"
  cfg="$BATS_TEST_TMPDIR/kwerft.yaml"
  printf '%b' "$1" | sed "s|TOKEN|$token|" >"$cfg"
}

@test "--config with appsDomain and Hetzner DNS is accepted" {
  write_config 'domain: ops.example.com\nappsDomain: Apps.Example.com\ndns: { solver: hetzner, tokenFile: TOKEN }\n'
  run "$SCRIPT" --dry-run --platform cloud --config "$cfg"
  [ "$status" -eq 0 ]
}

@test "--config dns needs a solver Kwerft knows" {
  write_config 'appsDomain: apps.example.com\ndns: { solver: route53, tokenFile: TOKEN }\n'
  run "$SCRIPT" --dry-run --platform cloud --config "$cfg"
  [ "$status" -eq 2 ]
  [[ "$output" == *"dns.solver"*"must be hetzner"* ]]
}

@test "--config dns.records must be true or false" {
  write_config 'appsDomain: apps.example.com\ndns: { solver: hetzner, tokenFile: TOKEN, records: maybe }\n'
  run "$SCRIPT" --dry-run --platform cloud --config "$cfg"
  [ "$status" -eq 2 ]
  [[ "$output" == *"dns.records"*"true or false"* ]]
  write_config 'appsDomain: apps.example.com\ndns: { solver: hetzner, tokenFile: TOKEN, records: False }\n'
  run "$SCRIPT" --dry-run --platform cloud --config "$cfg"
  [ "$status" -eq 0 ]
}

@test "--config dns needs appsDomain" {
  write_config 'dns: { solver: hetzner, tokenFile: TOKEN }\n'
  run "$SCRIPT" --dry-run --platform cloud --config "$cfg"
  [ "$status" -eq 2 ]
  [[ "$output" == *"needs appsDomain"* ]]
}

@test "--config dns needs a readable token file" {
  write_config 'appsDomain: apps.example.com\ndns:\n  solver: hetzner\n  tokenFile: /nonexistent/dns.token\n'
  run "$SCRIPT" --dry-run --platform cloud --config "$cfg"
  [ "$status" -eq 2 ]
  [[ "$output" == *"dns.tokenFile not readable"* ]]
}

@test "--config appsDomain must be a domain" {
  write_config 'appsDomain: "apps example"\n'
  run "$SCRIPT" --dry-run --platform cloud --config "$cfg"
  [ "$status" -eq 2 ]
  [[ "$output" == *"appsDomain"*"must be a domain"* ]]
}

@test "--domain must be a hostname" {
  run "$SCRIPT" --dry-run --platform cloud --domain "ops example.com"
  [ "$status" -eq 2 ]
  [[ "$output" == *"--domain must be a hostname"* ]]
}

@test "resolve_domain: re-running without --domain keeps what Settings chose" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  DOMAIN_FILE="$BATS_TEST_TMPDIR/domain"; echo "203.0.113.24.sslip.io" >"$DOMAIN_FILE"
  kc() { [[ "$*" == *"{.spec.consoleDomain}"* ]] && printf 'ops.example.com'; return 0; }
  DOMAIN=""; DOMAIN_EXPLICIT=0; PUBLIC_IP="203.0.113.24"
  resolve_domain
  [ "$DOMAIN" = "ops.example.com" ]
}

@test "resolve_domain: --domain replaces the Settings choice and says so" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  kc() { [[ "$*" == *"{.spec.consoleDomain}"* ]] && printf 'ops.example.com'; return 0; }
  DOMAIN="console.example.net"; DOMAIN_EXPLICIT=1; PUBLIC_IP="203.0.113.24"
  resolve_domain 2>"$BATS_TEST_TMPDIR/err"
  [ "$DOMAIN" = "console.example.net" ]
  [[ "$(<"$BATS_TEST_TMPDIR/err")" == *"moves from ops.example.com (chosen in Settings) to console.example.net"* ]]
}

# absent PATTERN FILE: bats does not fail a test on `! grep`.
absent() { [ "$(grep -c -- "$1" "$2")" -eq 0 ]; }

# kc stub for apply_console_settings: logs every call (and what is piped into
# `apply -f -`) and answers `get` as if the settings exist when EXISTING is set.
settings_env() {
  KWERFT_SOURCED=1 source "$SCRIPT"
  LOG_FILE="$BATS_TEST_TMPDIR/install.log"; : >"$LOG_FILE"
  DNS_TOKEN_SUM_FILE="$BATS_TEST_TMPDIR/dns-token.sha256"
  KC_LOG="$BATS_TEST_TMPDIR/kc.log"; : >"$KC_LOG"
  EXISTING=""
  kc() {
    printf 'kc %s\n' "$*" >>"$KC_LOG"
    if [[ "$*" == *"apply -f -"* ]]; then cat >>"$KC_LOG"; fi
    if [[ "$*" == "get consolesettings"* ]]; then printf '%s' "$EXISTING"; fi
    return 0
  }
}

@test "apply_console_settings: the first run records the console hostname" {
  settings_env
  DOMAIN="203.0.113.24.sslip.io"; DOMAIN_EXPLICIT=0
  apply_console_settings
  grep -q "kind: ConsoleSettings" "$KC_LOG"
  grep -q "consoleDomain: 203.0.113.24.sslip.io" "$KC_LOG"
  absent "kc patch" "$KC_LOG"
}

@test "apply_console_settings: a re-run without --domain leaves the hostname alone" {
  settings_env
  EXISTING="kwerft"; DOMAIN="ops.example.com"; DOMAIN_EXPLICIT=0
  apply_console_settings
  absent "kc patch" "$KC_LOG"
  absent "kind: ConsoleSettings" "$KC_LOG"
}

@test "apply_console_settings: --domain on a re-run replaces the hostname" {
  settings_env
  EXISTING="kwerft"; DOMAIN="console.example.net"; DOMAIN_EXPLICIT=1
  apply_console_settings
  grep -q 'kc patch consolesettings.kwerft.dev kwerft --type merge -p {"spec":{"consoleDomain":"console.example.net"}}' "$KC_LOG"
}

@test "apply_console_settings: appsDomain alone keeps a certificate per hostname" {
  settings_env
  EXISTING="kwerft"; DOMAIN="ops.example.com"; APPS_DOMAIN="apps.example.com"
  apply_console_settings
  grep -q '"appsDomain":"apps.example.com","tls":"http01","dns":null' "$KC_LOG"
  absent "kwerft-dns-token" "$KC_LOG"
}

@test "apply_console_settings: Hetzner DNS stores the token and turns on the wildcard" {
  settings_env
  token="$BATS_TEST_TMPDIR/dns.token"; printf 'hz-token\n' >"$token"
  EXISTING="kwerft"; DOMAIN="ops.example.com"; APPS_DOMAIN="apps.example.com"; DNS_SOLVER="hetzner"; DNS_TOKEN_FILE=$token
  apply_console_settings
  grep -q "create secret generic kwerft-dns-token --from-file=token=$token" "$KC_LOG"
  grep -q '"appsDomain":"apps.example.com","tls":"dns01","dns":{"provider":"hetzner","manageRecords":true}' "$KC_LOG"
  [ "$(grep -c 'kwerft.dev/dns-token-updated-at' "$KC_LOG")" -eq 1 ]
  # The token itself never lands in the install log.
  absent "hz-token" "$LOG_FILE"

  # The same token again: stored again, but no retry is triggered.
  : >"$KC_LOG"
  apply_console_settings
  grep -q "kwerft-dns-token" "$KC_LOG"
  absent 'kwerft.dev/dns-token-updated-at' "$KC_LOG"

  # A new token triggers one.
  printf 'hz-token-2\n' >"$token"
  apply_console_settings
  grep -q 'kwerft.dev/dns-token-updated-at' "$KC_LOG"
}

@test "apply_console_settings: dns.records false keeps DNS records manual" {
  settings_env
  token="$BATS_TEST_TMPDIR/dns.token"; printf 'hz-token\n' >"$token"
  EXISTING="kwerft"; DOMAIN="ops.example.com"; APPS_DOMAIN="apps.example.com"; DNS_SOLVER="hetzner"; DNS_TOKEN_FILE=$token; DNS_RECORDS=false
  apply_console_settings
  grep -q '"dns":{"provider":"hetzner","manageRecords":false}' "$KC_LOG"
}

# kc stub answering ConsoleSettings fields from SETTING_<field> variables.
summary_env() {
  KWERFT_SOURCED=1 source "$SCRIPT"
  DOMAIN="ops.example.com"; PUBLIC_IP="203.0.113.24"; LOG_FILE=/var/log/kwerft-install.log
  SETUP_TOKEN_FILE="$BATS_TEST_TMPDIR/none"; CONFIG_FILE=""
  kc() {
    case "$*" in
      *"{.spec.appsDomain}"*) printf '%s' "${SETTING_APPS:-}" ;;
      *"{.spec.tls}"*) printf '%s' "${SETTING_TLS:-http01}" ;;
      *"{.spec.dns.manageRecords}"*) printf '%s' "${SETTING_RECORDS:-}" ;;
      *"{.status.consoleDomain}"*) printf '%s' "$DOMAIN" ;;
    esac
    return 0
  }
}

@test "print_summary: apps DNS by hand or kept by Kwerft" {
  summary_env
  SETTING_APPS="apps.example.com"
  run print_summary
  [ "$status" -eq 0 ]
  [[ "$output" == *"DNS: *.apps.example.com → 203.0.113.24"* ]]
  SETTING_TLS="dns01"; SETTING_RECORDS="true"
  run print_summary
  [ "$status" -eq 0 ]
  [[ "$output" == *"(wildcard certificate via Hetzner DNS) · DNS records kept by Kwerft"* ]]
}

# The registry mirror: REGISTRIES_FILE in the test directory, systemctl and kc
# stubbed; SYSTEMCTL_LOG records restarts, ACTIVE names the running k3s unit.
mirror_env() {
  KWERFT_SOURCED=1 source "$SCRIPT"
  REGISTRIES_FILE="$BATS_TEST_TMPDIR/rancher/k3s/registries.yaml"
  # The build AppArmor profile has tests of its own; keep it off the real
  # /etc/apparmor.d on CI runners that have AppArmor.
  BUILD_APPARMOR_FILE="$BATS_TEST_TMPDIR/no-apparmor/kwerft-buildkit"
  LOG_FILE="$BATS_TEST_TMPDIR/install.log"; : >"$LOG_FILE"
  SYSTEMCTL_LOG="$BATS_TEST_TMPDIR/systemctl.log"; : >"$SYSTEMCTL_LOG"
  ACTIVE=""
  systemctl() {
    case "$1" in
      is-active) [[ "$3" == "$ACTIVE" ]] ;;
      *) printf '%s\n' "$*" >>"$SYSTEMCTL_LOG" ;;
    esac
  }
  kc() { return 0; }
}

@test "write_registry_mirror: maps the registry name to zot's ClusterIP over plain HTTP" {
  mirror_env
  [ "$(write_registry_mirror)" = "changed" ]
  grep -qx '  "registry.kwerft.internal:5000":' "$REGISTRIES_FILE"
  grep -qx '      - "http://10.43.0.50:5000"' "$REGISTRIES_FILE"
  [ "$(write_registry_mirror)" = "unchanged" ]
  # An older version of Kwerft's own file is replaced.
  sed -i.bak 's/10.43.0.50/10.43.0.99/' "$REGISTRIES_FILE"
  [ "$(write_registry_mirror)" = "changed" ]
  grep -q '10.43.0.50' "$REGISTRIES_FILE"
}

@test "registries_yaml: the nodes pull without a credential, so upgrades keep the file and k3s running" {
  # docs/phase2.md › Registry credentials: pushes need a project's
  # credential, pulls none. Adding auth here would restart k3s on every node
  # and need the credential on joined nodes first.
  mirror_env
  run registries_yaml
  [ "$status" -eq 0 ]
  [[ "$output" != *configs:* && "$output" != *auth:* && "$output" != *password* ]]
  # The file the release before credentials wrote is this one's: the stage
  # leaves it, and k3s, alone.
  mkdir -p "$(dirname "$REGISTRIES_FILE")"
  cat >"$REGISTRIES_FILE" <<'EOF'
# Managed by Kwerft installer (registry mirror).
# Images built from Git (registry.kwerft.internal:5000) come from the in-cluster registry
# through its fixed ClusterIP. k3s reads this file only when it starts.
mirrors:
  "registry.kwerft.internal:5000":
    endpoint:
      - "http://10.43.0.50:5000"
EOF
  ACTIVE=k3s
  run stage_registry_mirror
  [ "$status" -eq 0 ]
  [[ "$output" != *restarted* ]]
  ! grep -q restart "$SYSTEMCTL_LOG"
}

@test "write_registry_mirror: never touches the operator's own registries.yaml" {
  mirror_env
  mkdir -p "$(dirname "$REGISTRIES_FILE")"
  printf 'mirrors:\n  docker.io:\n    endpoint: ["https://mirror.example.com"]\n' >"$REGISTRIES_FILE"
  cp "$REGISTRIES_FILE" "$BATS_TEST_TMPDIR/before"
  [ "$(write_registry_mirror)" = "foreign" ]
  cmp -s "$REGISTRIES_FILE" "$BATS_TEST_TMPDIR/before"
  printf '  "registry.kwerft.internal:5000":\n    endpoint: ["http://10.43.0.50:5000"]\n' >>"$REGISTRIES_FILE"
  [ "$(write_registry_mirror)" = "operator" ]
}

@test "stage_registry_mirror: restarts k3s only when the file changed" {
  mirror_env
  ACTIVE=k3s
  run stage_registry_mirror
  [ "$status" -eq 0 ]
  [ "$output" = "registry.kwerft.internal:5000 → zot at 10.43.0.50:5000 · k3s restarted" ]
  grep -qx "restart k3s" "$SYSTEMCTL_LOG"

  : >"$SYSTEMCTL_LOG"
  run stage_registry_mirror
  [ "$status" -eq 0 ]
  [ "$output" = "registry.kwerft.internal:5000 → zot at 10.43.0.50:5000" ]
  [ ! -s "$SYSTEMCTL_LOG" ]
}

@test "stage_registry_mirror: a joined worker restarts k3s-agent; nothing restarts before k3s runs" {
  mirror_env
  ACTIVE=k3s-agent
  run stage_registry_mirror
  [ "$status" -eq 0 ]
  [[ "$output" == *"· k3s-agent restarted" ]]
  grep -qx "restart k3s-agent" "$SYSTEMCTL_LOG"

  rm -f "$REGISTRIES_FILE"; : >"$SYSTEMCTL_LOG"; ACTIVE=""
  run stage_registry_mirror
  [ "$status" -eq 0 ]
  [[ "$output" != *"restarted"* ]]
  [ ! -s "$SYSTEMCTL_LOG" ]
}

@test "stage_registry_mirror: an operator's file is reported, not changed" {
  mirror_env
  ACTIVE=k3s
  mkdir -p "$(dirname "$REGISTRIES_FILE")"
  printf 'mirrors: {}\n' >"$REGISTRIES_FILE"
  run stage_registry_mirror
  [ "$status" -eq 0 ]
  [[ "$output" == *"was not written by Kwerft"* ]]
  [[ "$output" == *'"http://10.43.0.50:5000"'* ]]
  [[ "$output" == *"registry.kwerft.internal:5000 not configured (see warning)" ]]
  [ "$(cat "$REGISTRIES_FILE")" = "mirrors: {}" ]
  [ ! -s "$SYSTEMCTL_LOG" ]
}

@test "remove_registry_mirror: removes Kwerft's file only" {
  mirror_env
  write_registry_mirror >/dev/null
  remove_registry_mirror
  [ ! -e "$REGISTRIES_FILE" ]
  printf 'mirrors: {}\n' >"$REGISTRIES_FILE"
  remove_registry_mirror
  [ -e "$REGISTRIES_FILE" ]
}

@test "registry_address_taken: zot's own Service does not count" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  kc() { printf '%s\n' "${SERVICES[@]}"; }
  SERVICES=()
  [ -z "$(registry_address_taken)" ]
  SERVICES=(kwerft-system/kwerft-registry)
  [ -z "$(registry_address_taken)" ]
  SERVICES=(shop/db)
  [ "$(registry_address_taken)" = "shop/db" ]
}

@test "adopt_namespace: an existing kwerft-builds is handed to the Helm release" {
  settings_env
  kc() { printf 'kc %s\n' "$*" >>"$KC_LOG"; [[ "$*" != "get namespace"* || -n "$EXISTING" ]]; }
  adopt_namespace kwerft-builds
  absent "annotate" "$KC_LOG"
  EXISTING=yes
  adopt_namespace kwerft-builds
  grep -q "annotate namespace kwerft-builds --overwrite meta.helm.sh/release-name=kwerft meta.helm.sh/release-namespace=kwerft-system" "$KC_LOG"
  grep -q "label namespace kwerft-builds --overwrite app.kubernetes.io/managed-by=Helm" "$KC_LOG"
}

# The pins at the top of install.sh, the chart's defaults and internal/builds
# must name the same registry and images (and the SMART exporter's).
@test "registry and build pins agree with the chart and internal/builds" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  root="$BATS_TEST_DIRNAME/../.."
  values="$root/charts/kwerft/values.yaml"
  grep -qE "^    tag: $ZOT_VERSION( |$)" "$values"
  grep -qE "^  clusterIP: $REGISTRY_CLUSTER_IP( |$)" "$values"
  grep -qE "^  buildkitImage: docker.io/moby/buildkit:${BUILDKIT_VERSION}-rootless( |$)" "$values"
  grep -qE "^  railpackImage: ghcr.io/railwayapp/railpack-frontend:${RAILPACK_VERSION}( |$)" "$values"
  grep -qE "^    repository: quay.io/prometheuscommunity/smartctl-exporter( |$)" "$values"
  grep -qE "^    tag: $SMARTCTL_EXPORTER_VERSION( |$)" "$values"
  grep -qF "RegistryHost = \"$REGISTRY_HOST\"" "$root/internal/builds/builds.go"
  grep -qF "RegistryClusterIP = \"$REGISTRY_CLUSTER_IP\"" "$root/internal/builds/builds.go"
  # Inside the service CIDR (10.43.0.0/16), in its low range kept for fixed addresses.
  [[ "$REGISTRY_CLUSTER_IP" == 10.43.0.* ]]
}

@test "ensure_build_apparmor: writes and loads the build profile, idempotently" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  LOG_FILE="$BATS_TEST_TMPDIR/install.log"; : >"$LOG_FILE"
  mkdir -p "$BATS_TEST_TMPDIR/apparmor.d"
  BUILD_APPARMOR_FILE="$BATS_TEST_TMPDIR/apparmor.d/kwerft-buildkit"
  PARSER_LOG="$BATS_TEST_TMPDIR/parser.log"; : >"$PARSER_LOG"
  apparmor_parser() { printf '%s\n' "$*" >>"$PARSER_LOG"; }
  run ensure_build_apparmor
  [ "$status" -eq 0 ]
  [ "$output" = "AppArmor profile kwerft-buildkit" ]
  grep -q 'profile kwerft-buildkit flags=(unconfined)' "$BUILD_APPARMOR_FILE"
  grep -q '^  userns,$' "$BUILD_APPARMOR_FILE"
  grep -q -- "-r -W $BUILD_APPARMOR_FILE" "$PARSER_LOG"
  before=$(stat -c %Y "$BUILD_APPARMOR_FILE" 2>/dev/null || stat -f %m "$BUILD_APPARMOR_FILE")
  run ensure_build_apparmor
  [ "$status" -eq 0 ]
  [ "$(stat -c %Y "$BUILD_APPARMOR_FILE" 2>/dev/null || stat -f %m "$BUILD_APPARMOR_FILE")" = "$before" ]
}

@test "ensure_build_apparmor: nothing where AppArmor is not installed" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  BUILD_APPARMOR_FILE="$BATS_TEST_TMPDIR/nonexistent/kwerft-buildkit"
  PATH="/nonexistent" run ensure_build_apparmor
  [ "$status" -eq 0 ]
  [ -z "$output" ]
}

@test "write_vlogs_values: Vector sends the fields the console filters on" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  values="$BATS_TEST_TMPDIR/vlogs.yaml"
  write_vlogs_values >"$values"
  grep -qx '            VL-Stream-Fields: namespace,pod,container,stream,project,app,build,task' "$values"
  grep -qx '            VL-Msg-Field: message' "$values"
  grep -qx '            VL-Time-Field: timestamp' "$values"
  # Only Vector and the console may reach VictoriaLogs.
  grep -qx '              kubernetes.io/metadata.name: kwerft-observability' "$values"
  grep -qx '              kubernetes.io/metadata.name: kwerft-system' "$values"
  # The remap is embedded as the parser's source, indented under it.
  grep -qx '          .app = labels."kwerft.dev/app"' "$values"
  # Same output every run (the stage is idempotent).
  [ "$(write_vlogs_values | cksum)" = "$(cksum <"$values")" ]
}

@test "vector_remap: sets every field internal/logs relies on" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  remap=$(vector_remap)
  for f in namespace pod container node project app build task schedule job level log; do
    [[ "$remap" == *".$f = "* ]] || { echo "missing .$f"; return 1; }
  done
  # The Go side names the same fields.
  fields="$BATS_TEST_DIRNAME/../../internal/logs/fields.go"
  for f in namespace pod container stream project app build task level; do
    grep -q "= \"$f\"" "$fields" || { echo "internal/logs lacks $f"; return 1; }
  done
}

@test "vector_remap: flattens Kubernetes metadata and keeps a JSON line's fields under log" {
  command -v vector >/dev/null || skip "vector is not installed"
  KWERFT_SOURCED=1 source "$SCRIPT"
  vector_remap >"$BATS_TEST_TMPDIR/remap.vrl"
  cat >"$BATS_TEST_TMPDIR/vector.yaml" <<YAML
sources:
  in: {type: stdin, decoding: {codec: json}}
transforms:
  parser: {type: remap, inputs: [in], file: "$BATS_TEST_TMPDIR/remap.vrl"}
sinks:
  out: {type: console, inputs: [parser], encoding: {codec: json}}
YAML
  cat >"$BATS_TEST_TMPDIR/in.json" <<'JSON'
{"message":"{\"level\":\"WARN\",\"namespace\":\"other\"}","stream":"stderr","file":"/var/log/pods/x","source_type":"kubernetes_logs","kubernetes":{"pod_name":"web-1","pod_namespace":"shop","container_name":"app","pod_owner":"Job/nightly","pod_uid":"u","pod_labels":{"kwerft.dev/app":"web","kwerft.dev/project":"shop","kwerft.dev/task":"nightly","other":"x"}}}
JSON
  line=$(vector --quiet -c "$BATS_TEST_TMPDIR/vector.yaml" <"$BATS_TEST_TMPDIR/in.json" | grep '"pod"')
  for kv in '"namespace":"shop"' '"pod":"web-1"' '"container":"app"' '"app":"web"' '"project":"shop"' \
            '"task":"nightly"' '"job":"nightly"' '"level":"warn"' '"stream":"stderr"' '"log":{"level":"WARN","namespace":"other"}'; do
    [[ "$line" == *"$kv"* ]] || { echo "missing $kv in $line"; return 1; }
  done
  [[ "$line" != *'"kubernetes"'* && "$line" != *'"file"'* && "$line" != *'"build"'* ]]
}

@test "traefik_values: Prometheus metrics with router labels on the metrics port" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  out=$(traefik_values)
  grep -qx '  metrics:' <<<"$out"
  grep -qx '    port: 9101          # node-exporter owns 9100 on the host network' <<<"$out"
  grep -qx '    entryPoint: metrics' <<<"$out"
  grep -qx '    addRoutersLabels: true' <<<"$out"
  grep -qx '    addServicesLabels: false' <<<"$out"
  grep -q '^    buckets: "0.01,.*,10"$' <<<"$out"
  # The chart scrapes the port by that name, in Traefik's namespace.
  chart="$BATS_TEST_DIRNAME/../../charts/kwerft"
  grep -q '^    - port: metrics$' "$chart/templates/metrics-scrape.yaml"
  grep -q '^    namespace: traefik ' "$chart/values.yaml"
}

@test "vm_stack_values: kube-state-metrics exports Kwerft's pod labels" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  out=$(vm_stack_values)
  grep -qx '  metricLabelsAllowlist:' <<<"$out"
  grep -qx '    - pods=\[kwerft.dev/app,kwerft.dev/project\]' <<<"$out"
  # DiskReadingsMissing finds dedicated servers by the node label.
  grep -qx '    - nodes=\[kwerft.dev/platform\]' <<<"$out"
  grep -q 'label_kwerft_dev_platform="dedicated"' "$BATS_TEST_DIRNAME/../../internal/alerting/conditions.go"
  # The recording rules read it under kube-state-metrics' name for it.
  grep -q 'label_kwerft_dev_app' "$BATS_TEST_DIRNAME/../../charts/kwerft/templates/metrics-rules.yaml"
}

# Kwerft routes alerts with VMAlertmanagerConfigs in kwerft-observability;
# the operator's namespace matcher would drop project and node alerts.
@test "stage_observability: Alertmanager routes alerts from every namespace" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  LOG_FILE="$BATS_TEST_TMPDIR/install.log"; : >"$LOG_FILE"
  VALUES_DIR="$BATS_TEST_TMPDIR/values"   # the stage writes its Helm values there
  HELM_LOG="$BATS_TEST_TMPDIR/helm.log"; : >"$HELM_LOG"
  helmk() { printf 'helm %s\n' "$*" >>"$HELM_LOG"; }
  kc() { :; }
  run stage_observability
  [ "$status" -eq 0 ]
  grep -q "upgrade --install vm vm/victoria-metrics-k8s-stack .*--set alertmanager.enabled=true" "$HELM_LOG"
  grep -q "upgrade --install vm vm/victoria-metrics-k8s-stack .*--set alertmanager.spec.disableNamespaceMatcher=true" "$HELM_LOG"
  [[ "$output" == VictoriaMetrics* ]]
}

# The console reads traffic counts and dropped connections from the Hubble
# relay over plain gRPC; --lite installs neither.
@test "stage_network: Hubble and its relay unless --lite, relay without TLS" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  LOG_FILE="$BATS_TEST_TMPDIR/install.log"; : >"$LOG_FILE"
  HELM_LOG="$BATS_TEST_TMPDIR/helm.log"; : >"$HELM_LOG"
  PUBLIC_IP=203.0.113.10
  helmk() { printf 'helm %s\n' "$*" >>"$HELM_LOG"; }
  kc() { :; }
  LITE=0
  run stage_network
  [ "$status" -eq 0 ]
  grep -q "upgrade --install cilium .*--set hubble.enabled=true --set hubble.relay.enabled=true" "$HELM_LOG"
  grep -q "upgrade --install cilium .*--set hubble.relay.tls.server.enabled=false" "$HELM_LOG"
  [[ "$output" == *"· Hubble" ]]
  : >"$HELM_LOG"
  LITE=1
  run stage_network
  [ "$status" -eq 0 ]
  grep -q "upgrade --install cilium .*--set hubble.enabled=false --set hubble.relay.enabled=false" "$HELM_LOG"
  [[ "$output" != *"Hubble"* ]]
  # The Kwerft chart follows (stage_kwerft), and expects the relay where
  # Cilium's chart puts it.
  grep -q -- '--set hubble.enabled="$(hubble_enabled)"' "$SCRIPT"
  grep -q '^  relayAddress: hubble-relay.kube-system.svc:80 ' "$BATS_TEST_DIRNAME/../../charts/kwerft/values.yaml"
}

# The node agent (internal/firewall) fills managed_ssh and managed_open; the
# base chain decides everything else, in this order.
@test "firewall_ruleset: the base chain keeps the baseline and jumps to Kwerft's chains" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  PRIVATE_CIDR="10.0.1.3/16"; HCLOUD_NETWORK_RANGE=""
  private_network
  run firewall_ruleset
  [ "$status" -eq 0 ]
  grep -q '^  chain managed_ssh {$' <<<"$output"
  grep -q '^  chain managed_open {$' <<<"$output"
  grep -q '^flush chain inet kwerft input$' <<<"$output"
  ! grep -q 'delete table' <<<"$output" || false
  ! grep -q 'flush chain inet kwerft managed' <<<"$output" || false
  # Everything the cluster needs is accepted before public SSH may be narrowed …
  rules=$(sed -n '/^flush chain/,$p' <<<"$output" | sed -n 's/^    //p')
  line() { grep -n -F -x -- "$1" <<<"$rules" | cut -d: -f1; }
  ssh_jump=$(line "tcp dport 22 jump managed_ssh")
  [ -n "$ssh_jump" ]
  for before in "ct state established,related accept" "iif lo accept" "ip saddr 10.42.0.0/16 accept" \
                "ip saddr 10.0.0.0/16 accept" "udp dport 51871 accept" 'iifname { "cilium_*", "lxc*" } accept'; do
    n=$(line "$before"); [ -n "$n" ]; [ "$n" -lt "$ssh_jump" ]
  done
  # … HTTP(S) and SSH are accepted after it, and the console's ports come last.
  [ "$(line "tcp dport { 22, 80, 443 } accept")" -gt "$ssh_jump" ]
  [ "$(line "jump managed_open")" -eq "$(wc -l <<<"$rules" | tr -d ' ')" ]
}

@test "firewall_ruleset: without a private network" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  PRIVATE_CIDR=""
  run firewall_ruleset
  grep -q '# no private network detected' <<<"$output"
  grep -q 'tcp dport 22 jump managed_ssh' <<<"$output"
}

@test "reset_firewall: removes the table and pauses the node agent" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  NFT_LOG="$BATS_TEST_TMPDIR/nft.log"; : >"$NFT_LOG"
  nft() { printf '%s\n' "$*" >>"$NFT_LOG"; }
  FIREWALL_STATE_DIR="$BATS_TEST_TMPDIR/firewall"
  run reset_firewall
  [ "$status" -eq 0 ]
  grep -qx 'delete table inet kwerft' "$NFT_LOG"
  [ -e "$FIREWALL_STATE_DIR/paused" ]
}

# install.sh's table and ports are what internal/firewall manages and shows.
@test "firewall constants agree with internal/firewall and the chart" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  root="$BATS_TEST_DIRNAME/../.."
  grep -qF "PodCIDR       = \"$POD_CIDR\"" "$root/internal/firewall/firewall.go"
  grep -qF "WireGuardPort = $WG_PORT" "$root/internal/firewall/firewall.go"
  grep -qF 'ChainSSH   = "managed_ssh"' "$root/internal/firewall/nft.go"
  grep -qF 'ChainOpen  = "managed_open"' "$root/internal/firewall/nft.go"
  grep -qF 'PausedFile = "paused"' "$root/internal/firewall/agent.go"
  grep -qF "path: $FIREWALL_STATE_DIR," "$root/charts/kwerft/templates/node-agent.yaml"
}

# ---- agent mode (docs/phase5.md) ---------------------------------------------

AGENT_TOKEN="kwag_edge-1_abcdefghijklmnopqrstuvwxyzABCDEFGHIJ-_0123"

@test "--agent needs --console and --cluster-token" {
  run "$SCRIPT" --agent --console https://ops.example.com
  [ "$status" -eq 2 ]
  [[ "$output" == *"--agent needs --console and --cluster-token"* ]]
  run "$SCRIPT" --agent --cluster-token "$AGENT_TOKEN"
  [ "$status" -eq 2 ]
}

@test "--console and --cluster-token need --agent" {
  run "$SCRIPT" --console https://ops.example.com --cluster-token "$AGENT_TOKEN"
  [ "$status" -eq 2 ]
  [[ "$output" == *"need --agent"* ]]
}

@test "--agent refuses --domain, --config and --join" {
  run "$SCRIPT" --agent --console https://ops.example.com --cluster-token "$AGENT_TOKEN" --domain ops2.example.com
  [ "$status" -eq 2 ]
  [[ "$output" == *"--domain and --config do not apply"* ]]
  run "$SCRIPT" --agent --console https://ops.example.com --cluster-token "$AGENT_TOKEN" --join https://ops.example.com --token t
  [ "$status" -eq 2 ]
  [[ "$output" == *"exclude each other"* ]]
}

@test "--console must be an https URL of a hostname" {
  for bad in http://ops.example.com ops.example.com https://ops.example.com/path "https://user@ops.example.com" https://ops; do
    run "$SCRIPT" --dry-run --agent --console "$bad" --cluster-token "$AGENT_TOKEN"
    [ "$status" -eq 2 ]
    [[ "$output" == *"--console must look like https://"* ]]
  done
  run "$SCRIPT" --dry-run --platform cloud --agent --console https://ops.example.com:8443/ --cluster-token "$AGENT_TOKEN"
  [ "$status" -eq 0 ]
}

@test "--cluster-token must be an agent token" {
  for bad in kwft_abc "kwag_edge_short" "kwag_Edge_abcdefghijklmnopqrstuvwxyzABCDEFGHIJ" "kwag__abcdefghijklmnopqrstuvwxyzABCDEFGHIJ"; do
    run "$SCRIPT" --dry-run --agent --console https://ops.example.com --cluster-token "$bad"
    [ "$status" -eq 2 ]
    [[ "$output" == *"--cluster-token is not an agent token"* ]]
  done
}

@test "--dry-run in agent mode installs the cluster without the console's handoff" {
  run "$SCRIPT" --dry-run --platform cloud --agent --console https://ops.example.com --cluster-token "$AGENT_TOKEN"
  [ "$status" -eq 0 ]
  expected="Preflight System Firewall Kubernetes Registry Helm Upgrades Network Hetzner Ingress Observability Kwerft"
  actual=$(printf '%s\n' "$output" | sed -n 's/^→ \([A-Za-z]*\).*/\1/p' | tr '\n' ' ' | sed 's/ $//')
  [ "$actual" = "$expected" ]
  [[ "$output" == *"Kwerft agent"* ]]
  [[ "$output" == *"mode=agent"* ]]
  [[ "$output" != *"Handoff"* ]]
  [[ "$output" != *"sslip.io"* ]]
}

@test "agent options come from the environment too" {
  KWERFT_CONSOLE=https://ops.example.com KWERFT_CLUSTER_TOKEN="$AGENT_TOKEN" run "$SCRIPT" --dry-run --platform cloud --agent
  [ "$status" -eq 0 ]
}

@test "cluster_of_token names the token's cluster" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  [ "$(cluster_of_token "$AGENT_TOKEN")" = "edge-1" ]
}

# The token goes into the Secret the chart mounts, never into Helm values or
# command-line arguments; the chart runs in agent mode against the console.
@test "stage_kwerft_agent: chart in agent mode, token only in the Secret" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  LOG_FILE="$BATS_TEST_TMPDIR/install.log"; : >"$LOG_FILE"
  HELM_LOG="$BATS_TEST_TMPDIR/helm.log"; : >"$HELM_LOG"
  KC_LOG="$BATS_TEST_TMPDIR/kc.log"; : >"$KC_LOG"
  STDIN_LOG="$BATS_TEST_TMPDIR/stdin.log"; : >"$STDIN_LOG"
  helmk() { printf 'helm %s\n' "$*" >>"$HELM_LOG"; }
  kc() {
    printf 'kc %s\n' "$*" >>"$KC_LOG"
    if [[ "$*" == *"--from-file=token=/dev/stdin"* ]]; then cat >>"$STDIN_LOG"; fi
    return 0
  }
  chart_ref() { echo "oci://ghcr.io/ehilzinger/charts/kwerft"; }
  MODE=agent CONSOLE_URL=https://ops.example.com CLUSTER_TOKEN="$AGENT_TOKEN" PLATFORM=cloud
  run stage_kwerft_agent
  [ "$status" -eq 0 ]
  grep -q "upgrade --install kwerft oci://ghcr.io/ehilzinger/charts/kwerft .*--set mode=agent --set agent.consoleURL=https://ops.example.com" "$HELM_LOG"
  ! grep -q "console.domain" "$HELM_LOG"
  ! grep -qF "$AGENT_TOKEN" "$HELM_LOG"
  ! grep -qF "$AGENT_TOKEN" "$KC_LOG"
  grep -q "create secret generic kwerft-agent --from-file=token=/dev/stdin" "$KC_LOG"
  [ "$(cat "$STDIN_LOG")" = "$AGENT_TOKEN" ]
  [[ "$output" == "agent 0.1.0-dev · cluster edge-1 → https://ops.example.com" ]]
  # The Secret's name and key are the chart's.
  grep -q '^  tokenSecret: kwerft-agent' "$BATS_TEST_DIRNAME/../../charts/kwerft/values.yaml"
  grep -q 'items: \[{ key: token, path: token }\]' "$BATS_TEST_DIRNAME/../../charts/kwerft/templates/deployment.yaml"
}

@test "write_join_secret: the k3s server's join material, or nothing on an agent node" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  LOG_FILE="$BATS_TEST_TMPDIR/install.log"; : >"$LOG_FILE"
  KC_LOG="$BATS_TEST_TMPDIR/kc.log"; : >"$KC_LOG"
  kc() { printf 'kc %s\n' "$*" >>"$KC_LOG"; }
  PRIVATE_IP=10.0.0.2 PUBLIC_IP=203.0.113.10
  run write_join_secret     # no /var/lib/rancher/k3s/server/token here
  [ "$status" -eq 0 ]
  [ ! -s "$KC_LOG" ]
  # Both installs write it; the console's code reads the same name.
  [ "$(sed -n '/^stage_kwerft() {/,/^}/p' "$SCRIPT" | grep -c '^  write_join_secret$')" -eq 1 ]
  [ "$(sed -n '/^stage_kwerft_agent() {/,/^}/p' "$SCRIPT" | grep -c '^  write_join_secret$')" -eq 1 ]
  grep -qF 'LocalJoinSecret = "cluster-local-join"' "$BATS_TEST_DIRNAME/../../internal/clusters/tunnel.go"
  grep -qF 'kwerft-system create secret generic cluster-local-join' "$SCRIPT"
}

# ---- Hetzner Cloud (Phase 5) ------------------------------------------------

write_hcloud_config() {
  htoken="$BATS_TEST_TMPDIR/hcloud.token"; printf 'hcloud-token-0123456789\n' >"$htoken"
  cfg="$BATS_TEST_TMPDIR/kwerft.yaml"
  printf '%b' "$1" | sed "s|HTOKEN|$htoken|" >"$cfg"
}

@test "--config hcloud keys are checked" {
  write_hcloud_config 'domain: ops.example.com\nhcloud:\n  tokenFile: HTOKEN\n  cloudControllerManager: true\n  loadBalancer: true\n'
  run "$SCRIPT" --dry-run --platform cloud --config "$cfg"
  [ "$status" -eq 0 ]
  write_hcloud_config 'hcloud: { tokenFile: HTOKEN, cloudControllerManager: maybe }\n'
  run "$SCRIPT" --dry-run --platform cloud --config "$cfg"
  [ "$status" -eq 2 ]
  [[ "$output" == *"hcloud.cloudControllerManager"*"true or false"* ]]
  write_hcloud_config 'hcloud: { tokenFile: HTOKEN, loadBalancer: yes }\n'
  run "$SCRIPT" --dry-run --platform cloud --config "$cfg"
  [ "$status" -eq 2 ]
  [[ "$output" == *"hcloud.loadBalancer"* ]]
  write_hcloud_config 'hcloud: { tokenFile: /nonexistent/hcloud.token }\n'
  run "$SCRIPT" --dry-run --platform cloud --config "$cfg"
  [ "$status" -eq 2 ]
  [[ "$output" == *"hcloud.tokenFile not readable"* ]]
  write_hcloud_config 'hcloud: { cloudControllerManager: true }\n'
  run "$SCRIPT" --dry-run --platform cloud --config "$cfg"
  [ "$status" -eq 2 ]
  [[ "$output" == *"needs hcloud.tokenFile"* ]]
}

@test "--dry-run runs the Hetzner Cloud stage after the network" {
  run "$SCRIPT" --dry-run --platform cloud --domain ops.example.com
  [ "$status" -eq 0 ]
  [[ "$output" == *"Hetzner Cloud"*"(would run stage_hcloud)"* ]]
}

@test "write_k3s_config: the hcloud CCM turns off k3s's cloud controller, only when chosen" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  K3S_CONFIG_FILE="$BATS_TEST_TMPDIR/k3s/config.yaml"
  PUBLIC_IP=203.0.113.10; PRIVATE_IP=10.0.0.2; DOMAIN=ops.example.com; PLATFORM=cloud
  HCLOUD_CCM=""
  write_k3s_config
  absent "cloud-provider=external" "$K3S_CONFIG_FILE"
  run ccm_active
  [ "$status" -ne 0 ]
  HCLOUD_CCM=true
  write_k3s_config
  grep -qx "  - max-pods=200" "$K3S_CONFIG_FILE"
  grep -qx "  - cloud-provider=external" "$K3S_CONFIG_FILE"
  grep -qx "disable-cloud-controller: true" "$K3S_CONFIG_FILE"
  grep -qx "node-ip: 10.0.0.2" "$K3S_CONFIG_FILE"
  ccm_active
  # Still valid YAML: kubelet-arg is one list.
  [ "$(sed -n '/^kubelet-arg:/,/^[a-z]/p' "$K3S_CONFIG_FILE" | grep -c '^  - ')" -eq 2 ]
}

@test "hcloud_network_field reads the metadata service's private networks" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  meta='- ip: 10.0.0.2
  alias_ips: [10.0.0.3, 10.0.0.4]
  interface_num: 1
  mac_address: 86:00:00:2a:7d:e0
  network_id: 1234
  network_name: nw-test1
  network: 10.0.0.0/16
  subnet: 10.0.0.0/24
  gateway: 10.0.0.1
- ip: 192.168.0.2
  alias_ips: []
  network_id: 4321
  network: 192.168.0.0/16'
  [ "$(hcloud_network_field "$meta" 10.0.0.2 network_id)" = "1234" ]
  [ "$(hcloud_network_field "$meta" 10.0.0.2 network)" = "10.0.0.0/16" ]
  [ "$(hcloud_network_field "$meta" 192.168.0.2 network_id)" = "4321" ]
  [ -z "$(hcloud_network_field "$meta" 10.9.9.9 network_id)" ]
}

@test "traefik_proxy_args: PROXY protocol from the Cloud Network only" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  HCLOUD_NETWORK_RANGE=""
  [ -z "$(traefik_proxy_args)" ]
  HCLOUD_NETWORK_RANGE=10.0.0.0/16
  run traefik_proxy_args
  [[ "$output" == *"--set ports.web.proxyProtocol.trustedIPs={10.0.0.0/16}"* ]]
  [[ "$output" == *"--set ports.websecure.proxyProtocol.trustedIPs={10.0.0.0/16}"* ]]
}

@test "hcloud_storage_class: not the default, only where the CSI driver runs" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  run hcloud_storage_class
  [[ "$output" == *"name: hcloud-volumes"* ]]
  [[ "$output" == *'is-default-class: "false"'* ]]
  [[ "$output" == *"provisioner: csi.hetzner.cloud"* ]]
  [[ "$output" == *"key: csi.hetzner.cloud/location"* ]]
  [[ "$output" == *"values: [fsn1, nbg1, hel1, ash, hil, sin]"* ]]
  [[ "$output" == *"allowVolumeExpansion: true"* ]]
  run hcloud_csi_values
  [[ "$output" == *"storageClasses: []"* ]]
  [[ "$output" == *"key: kwerft.dev/platform"* ]]
  [[ "$output" == *"values: [dedicated]"* ]]
}

@test "hcloud_ccm_values: private addresses with a Cloud Network, never routes" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  HCLOUD_NETWORK_ID=1234
  run hcloud_ccm_values
  [[ "$output" == *"enabled: true"* ]]
  [[ "$output" == *"clusterCIDR: 10.42.0.0/16"* ]]
  [[ "$output" == *'HCLOUD_NETWORK_ROUTES_ENABLED:'*'value: "false"'* ]]
  HCLOUD_NETWORK_ID=""
  run hcloud_ccm_values
  [[ "$output" == *"enabled: false"* ]]
}

# kc/helm stubs for stage_hcloud: kc logs calls and piped input, answers the
# stored token from STORED and the hcloud Secret's owner from OWNER.
hcloud_env() {
  KWERFT_SOURCED=1 source "$SCRIPT"
  HCLOUD_TMP_DIR="$BATS_TEST_TMPDIR/state"; mkdir -p "$HCLOUD_TMP_DIR"
  VALUES_DIR="$BATS_TEST_TMPDIR/values"
  K3S_CONFIG_FILE="$BATS_TEST_TMPDIR/k3s.yaml"; : >"$K3S_CONFIG_FILE"
  LOG_FILE="$BATS_TEST_TMPDIR/install.log"; : >"$LOG_FILE"
  KC_LOG="$BATS_TEST_TMPDIR/kc.log"; : >"$KC_LOG"
  HELM_LOG="$BATS_TEST_TMPDIR/helm.log"; : >"$HELM_LOG"
  PLATFORM=cloud; PRIVATE_IP=10.0.0.2; HCLOUD_NETWORK_ID=1234; HCLOUD_TOKEN_FILE=""; HCLOUD_CCM=""
  STORED=""; OWNER=""; CONSOLE_TOKEN=""
  kc() {
    printf 'kc %s\n' "$*" >>"$KC_LOG"
    case "$*" in
      *"get secret kwerft-hcloud-token"*) printf '%s' "$(printf '%s' "$STORED" | base64)" ;;
      *"get secret hcloud -o jsonpath={.data.token}"*) printf '%s' "$(printf '%s' "$CONSOLE_TOKEN" | base64)" ;;
      *"get secret hcloud -o jsonpath"*) printf '%s' "$OWNER" ;;
      *"get secret hcloud"*) [[ -n "$OWNER" ]] || return 1 ;;
      *"get storageclass"*) return 1 ;;
      *"get nodes"*) printf '' ;;
      *"create secret generic hcloud"*) printf 'kind: Secret\n' ;;
      *"label --local"*) cat ;;
      *"apply -f -"*) cat >>"$KC_LOG" ;;
    esac
    return 0
  }
  helmk() { printf 'helm %s\n' "$*" >>"$HELM_LOG"; }
}

@test "stage_hcloud: dedicated servers and clusters without a token get no Cloud parts" {
  hcloud_env
  PLATFORM=dedicated
  run stage_hcloud
  [ "$status" -eq 0 ]
  [[ "$output" == *"not a Hetzner Cloud server"* ]]
  PLATFORM=cloud
  run stage_hcloud
  [ "$status" -eq 0 ]
  [[ "$output" == *"no Cloud API token"* ]]
  [ ! -s "$HELM_LOG" ]
  [ -z "$(ls "$HCLOUD_TMP_DIR")" ]
}

@test "stage_hcloud: the CSI driver with the token from --config or Settings" {
  hcloud_env
  printf ' hcloud-token-from-file \n' >"$BATS_TEST_TMPDIR/t"; HCLOUD_TOKEN_FILE="$BATS_TEST_TMPDIR/t"
  run stage_hcloud
  [ "$status" -eq 0 ]
  [[ "$output" == *"CSI $HCLOUD_CSI_CHART_VERSION"* ]]
  [[ "$output" != *"cloud-controller-manager"* ]]
  grep -q "upgrade --install hcloud-csi hcloud/hcloud-csi --version $HCLOUD_CSI_CHART_VERSION" "$HELM_LOG"
  absent "hcloud-cloud-controller-manager" "$HELM_LOG"
  grep -q "create secret generic hcloud --from-file=token=" "$KC_LOG"
  grep -q "from-literal=network=1234" "$KC_LOG"
  grep -q "label --local -f - app.kubernetes.io/managed-by=kwerft" "$KC_LOG"
  grep -q "name: hcloud-volumes" "$KC_LOG"
  # The temporary token file is gone, and the token never reached the log.
  [ -z "$(ls "$HCLOUD_TMP_DIR")" ]
  absent "hcloud-token-from-file" "$LOG_FILE"

  # Without --config, the token stored in Settings.
  : >"$HELM_LOG"; HCLOUD_TOKEN_FILE=""; STORED="stored-token"
  run stage_hcloud
  [ "$status" -eq 0 ]
  grep -q "hcloud-csi" "$HELM_LOG"

  # An operator's own kube-system/hcloud is left alone.
  : >"$KC_LOG"; OWNER="someone"
  run stage_hcloud
  [ "$status" -eq 0 ]
  absent "create secret generic hcloud" "$KC_LOG"
}

@test "stage_hcloud: the CCM where the cluster was installed with it" {
  hcloud_env
  printf 'disable-cloud-controller: true\n' >"$K3S_CONFIG_FILE"
  retry() { "${@:3}"; }
  run stage_hcloud
  [ "$status" -eq 40 ]
  [[ "$output" == *"needs the Cloud API token"* ]]
  STORED="stored-token"
  run stage_hcloud
  [ "$status" -eq 0 ]
  [[ "$output" == "cloud-controller-manager $HCLOUD_CCM_CHART_VERSION · CSI"* ]]
  grep -q "upgrade --install hcloud-cloud-controller-manager hcloud/hcloud-cloud-controller-manager --version $HCLOUD_CCM_CHART_VERSION" "$HELM_LOG"
  grep -q "HCLOUD_NETWORK_ROUTES_ENABLED" "$VALUES_DIR/hcloud-ccm.yaml"
  # No Cloud Network found for the private address: refuse.
  HCLOUD_NETWORK_ID=""
  run stage_hcloud
  [ "$status" -eq 40 ]
  [[ "$output" == *"Cloud Network"* ]]
}

@test "stage_hcloud: hcloud.cloudControllerManager on an existing cluster only warns" {
  hcloud_env
  HCLOUD_CCM=true; STORED="stored-token"
  run stage_hcloud
  [ "$status" -eq 0 ]
  [[ "$output" == *"only takes effect when a cluster is first installed"* ]]
  absent "hcloud-cloud-controller-manager" "$HELM_LOG"
}

@test "stage_network: no --wait while the node waits for the hcloud CCM" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  LOG_FILE="$BATS_TEST_TMPDIR/install.log"; : >"$LOG_FILE"
  HELM_LOG="$BATS_TEST_TMPDIR/helm.log"; : >"$HELM_LOG"
  K3S_CONFIG_FILE="$BATS_TEST_TMPDIR/k3s.yaml"; printf 'disable-cloud-controller: true\n' >"$K3S_CONFIG_FILE"
  PUBLIC_IP=203.0.113.10
  helmk() { printf 'helm %s\n' "$*" >>"$HELM_LOG"; }
  kc() { if [[ "$*" == "get nodes"* ]]; then printf 'node.cloudprovider.kubernetes.io/uninitialized'; fi; return 0; }
  run stage_network
  [ "$status" -eq 0 ]
  absent "upgrade --install cilium .*--wait" "$HELM_LOG"
  : >"$HELM_LOG"
  kc() { return 0; }
  run stage_network
  grep -q "upgrade --install cilium .*--wait" "$HELM_LOG"
}

@test "apply_hcloud_settings: stores the token once and the Load Balancer choice" {
  settings_env
  HCLOUD_TOKEN_SUM_FILE="$BATS_TEST_TMPDIR/hcloud.sha256"; HCLOUD_TMP_DIR="$BATS_TEST_TMPDIR"
  printf 'hc-token\n' >"$BATS_TEST_TMPDIR/hc"; HCLOUD_TOKEN_FILE="$BATS_TEST_TMPDIR/hc"; HCLOUD_LB=""
  apply_hcloud_settings
  grep -q "create secret generic kwerft-hcloud-token --from-file=token=" "$KC_LOG"
  [ "$(grep -c 'kwerft.dev/hcloud-token-updated-at' "$KC_LOG")" -eq 1 ]
  absent "hetznerCloud" "$KC_LOG"
  absent "hc-token" "$LOG_FILE"
  : >"$KC_LOG"
  HCLOUD_LB=true
  apply_hcloud_settings
  absent 'kwerft.dev/hcloud-token-updated-at' "$KC_LOG"
  grep -q '{"spec":{"hetznerCloud":{"loadBalancer":{"enabled":true}}}}' "$KC_LOG"
}

@test "hcloud chart pins are versions, and the chart takes the hcloud flags" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  [[ "$HCLOUD_CCM_CHART_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]
  [[ "$HCLOUD_CSI_CHART_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]
  grep -q -- '--hcloud-ccm=' "$BATS_TEST_DIRNAME/../../charts/kwerft/templates/deployment.yaml"
  grep -q -- '--hcloud-proxy-network=' "$BATS_TEST_DIRNAME/../../charts/kwerft/templates/deployment.yaml"
}

@test "agent mode: stage_hcloud uses the token the console handed over" {
  hcloud_env
  MODE=agent
  run stage_hcloud
  [ "$status" -eq 0 ]
  [[ "$output" == *"no Cloud API token: Cloud Volumes off (Hetzner Cloud clusters get the console's"* ]]
  AWAIT_CLOUD_TOKEN=1
  run stage_hcloud
  [[ "$output" == *"the console hands it over once this cluster's agent connects"* ]]
  [ ! -s "$HELM_LOG" ]
  # The Settings token of the cluster itself does not count in agent mode.
  STORED="stored-token"
  run stage_hcloud
  [ ! -s "$HELM_LOG" ]
  # The console's, in kube-system/hcloud labelled as Kwerft's.
  OWNER=kwerft; CONSOLE_TOKEN="console-token"
  run stage_hcloud
  [ "$status" -eq 0 ]
  [[ "$output" == *"CSI $HCLOUD_CSI_CHART_VERSION"* ]]
  grep -q "upgrade --install hcloud-csi" "$HELM_LOG"
  # Not when the Secret is an operator's own.
  : >"$HELM_LOG"; OWNER=someone
  run stage_hcloud
  [ ! -s "$HELM_LOG" ]
}

@test "await_cloud_volumes waits for the console's token, then adds the CSI driver" {
  hcloud_env
  MODE=agent
  helmk() { printf 'helm %s\n' "$*" >>"$HELM_LOG"; [[ "$*" != *"status hcloud-csi"* ]]; }
  SLEEPS=0
  sleep() { SLEEPS=$((SLEEPS + 1)); (( SLEEPS < 3 )) || { OWNER=kwerft; CONSOLE_TOKEN="console-token"; }; }
  # Only with --await-cloud-token, only on Cloud servers.
  run await_cloud_volumes
  [ -z "$output" ]
  AWAIT_CLOUD_TOKEN=1; PLATFORM=dedicated
  run await_cloud_volumes
  [ -z "$output" ]
  PLATFORM=cloud
  # The token never comes: a warning, not a failure.
  CLOUD_TOKEN_WAIT=0
  run await_cloud_volumes
  [ "$status" -eq 0 ]
  [[ "$output" == *"has not handed over its Hetzner Cloud token"* ]]
  absent "upgrade --install" "$HELM_LOG"
  # It comes after a few polls.
  CLOUD_TOKEN_WAIT=60
  run await_cloud_volumes
  [ "$status" -eq 0 ]
  [ "$output" = " · Cloud Volumes (CSI $HCLOUD_CSI_CHART_VERSION)" ]
  grep -q "upgrade --install hcloud-csi" "$HELM_LOG"
  # Installed already: nothing to wait for.
  helmk() { printf 'helm %s\n' "$*" >>"$HELM_LOG"; }
  : >"$HELM_LOG"; OWNER=""; CONSOLE_TOKEN=""
  run await_cloud_volumes
  [ -z "$output" ]
  absent "upgrade --install" "$HELM_LOG"
}

@test "--await-cloud-token needs --agent; agent mode passes the Cloud network to the chart" {
  run "$SCRIPT" --dry-run --await-cloud-token
  [ "$status" -eq 2 ]
  [[ "$output" == *"need --agent"* ]]
  run "$SCRIPT" --dry-run --platform cloud --agent --console https://ops.example.com --cluster-token "$AGENT_TOKEN" --await-cloud-token
  [ "$status" -eq 0 ]
  sed -n '/^stage_kwerft_agent()/,/^}/p' "$SCRIPT" | grep -qF '"${hcloud_args[@]}"'
}

# ---- upgrades (docs/phase6-upgrades.md, installer changes W2) ---------------

# effective [flags...] parses the flags in a fresh copy of the installer, with
# install.env at $BATS_TEST_TMPDIR/install.env, and prints the settings.
# STUBS is evaluated after sourcing (stub functions).
effective() {
  (
    KWERFT_SOURCED=1 source "$SCRIPT"
    INSTALL_ENV_FILE="$BATS_TEST_TMPDIR/install.env"
    eval "${STUBS:-}"
    parse_args "$@"
    echo "mode=$MODE email=$ACME_EMAIL acme=$ACME_SERVER platform=$PLATFORM iface=$PRIVATE_IFACE lite=$LITE ssh=$HARDEN_SSH channel=$CHANNEL console=$CONSOLE_URL token=$CLUSTER_TOKEN"
  )
}
write_install_env() { printf '%b' "$1" >"$BATS_TEST_TMPDIR/install.env"; }
STAGING="https://acme-staging-v02.api.letsencrypt.org/directory"

@test "remember_settings: install.env holds the effective settings, 0600, no hostname or tokens" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  INSTALL_ENV_FILE="$BATS_TEST_TMPDIR/install.env"
  MODE=install ACME_EMAIL=ops@example.com ACME_SERVER=$ACME_STAGING_URL PLATFORM=cloud PRIVATE_IFACE=enp7s0 PRIVATE_IFACE_GIVEN=""
  LITE=1 HARDEN_SSH=0 CHANNEL=edge DOMAIN=ops.example.com JOIN_TOKEN=kwft_join_secret CONSOLE_URL=""
  remember_settings
  [[ "$(ls -l "$INSTALL_ENV_FILE")" == -rw-------* ]]
  expected="KWERFT_MODE=install
KWERFT_EMAIL=ops@example.com
KWERFT_ACME_SERVER=https://acme-staging-v02.api.letsencrypt.org/directory
KWERFT_PLATFORM=cloud
KWERFT_PRIVATE_IFACE=
KWERFT_LITE=1
KWERFT_HARDEN_SSH=0
KWERFT_CHANNEL=edge"
  [ "$(grep -v '^#' "$INSTALL_ENV_FILE")" = "$expected" ]
  ! grep -q 'ops.example.com\|kwft_join\|DOMAIN' "$INSTALL_ENV_FILE"
  [ ! -e "$INSTALL_ENV_FILE.kwerft-new" ]
  # Agent mode adds the console, never the agent token; a dry run writes nothing.
  MODE=agent CONSOLE_URL=https://ops.example.com CLUSTER_TOKEN="$AGENT_TOKEN" PRIVATE_IFACE_GIVEN=enp7s0
  remember_settings
  grep -qx 'KWERFT_MODE=agent' "$INSTALL_ENV_FILE"
  grep -qx 'KWERFT_CONSOLE=https://ops.example.com' "$INSTALL_ENV_FILE"
  grep -qx 'KWERFT_PRIVATE_IFACE=enp7s0' "$INSTALL_ENV_FILE"
  ! grep -qF "$AGENT_TOKEN" "$INSTALL_ENV_FILE"
  rm "$INSTALL_ENV_FILE"
  DRY_RUN=1 remember_settings
  [ ! -e "$INSTALL_ENV_FILE" ]
}

@test "install.env: what a run remembered is read back by the next" {
  (
    KWERFT_SOURCED=1 source "$SCRIPT"
    INSTALL_ENV_FILE="$BATS_TEST_TMPDIR/install.env"
    parse_args --email ops@example.com --acme-server staging --platform dedicated --private-iface enp7s0 --lite --harden-ssh --channel edge
    remember_settings
  )
  run effective
  [ "$output" = "mode=install email=ops@example.com acme=$STAGING platform=dedicated iface=enp7s0 lite=1 ssh=1 channel=edge console= token=" ]
}

@test "install.env: precedence is flag, then KWERFT_*, then install.env, then the default" {
  write_install_env 'KWERFT_MODE=install\nKWERFT_EMAIL=file@example.com\nKWERFT_ACME_SERVER=https://acme.example.com/directory\nKWERFT_PLATFORM=dedicated\nKWERFT_LITE=1\nKWERFT_CHANNEL=edge\n'
  run effective
  [ "$output" = "mode=install email=file@example.com acme=https://acme.example.com/directory platform=dedicated iface= lite=1 ssh=0 channel=edge console= token=" ]
  KWERFT_EMAIL=env@example.com KWERFT_LITE=0 KWERFT_CHANNEL=stable run effective
  [ "$output" = "mode=install email=env@example.com acme=https://acme.example.com/directory platform=dedicated iface= lite=0 ssh=0 channel=stable console= token=" ]
  KWERFT_EMAIL=env@example.com run effective --email flag@example.com --platform cloud --acme-server staging
  [ "$output" = "mode=install email=flag@example.com acme=$STAGING platform=cloud iface= lite=1 ssh=0 channel=edge console= token=" ]
  # Without the file: the defaults.
  rm "$BATS_TEST_TMPDIR/install.env"
  run effective
  [ "$output" = "mode=install email= acme= platform=auto iface= lite=0 ssh=0 channel=stable console= token=" ]
}

@test "install.env: an email in this run's --config wins over the remembered one" {
  write_install_env 'KWERFT_EMAIL=file@example.com\n'
  cfg="$BATS_TEST_TMPDIR/kwerft.yaml"
  printf 'email: config@example.com\n' >"$cfg"
  run effective --config "$cfg"
  [[ "$output" == *"email=config@example.com "* ]]
  printf 'domain: ops.example.com\n' >"$cfg"
  run effective --config "$cfg"
  [[ "$output" == *"email=file@example.com "* ]]
}

@test "install.env: values are checked like flags, and the file is never sourced" {
  write_install_env 'KWERFT_CHANNEL=nightly\n'
  run effective
  [ "$status" -eq 2 ]
  [[ "$output" == *"--channel must be stable or edge"* ]]
  write_install_env 'KWERFT_EMAIL=$(touch '"$BATS_TEST_TMPDIR"'/pwned)\nrm -rf /tmp/nothing\nKWERFT_UNKNOWN=x\r\nKWERFT_PLATFORM=cloud\r\n'
  run effective
  [ "$status" -eq 0 ]
  [ ! -e "$BATS_TEST_TMPDIR/pwned" ]
  [[ "$output" == *'email=$(touch '*"platform=cloud "* ]]
}

@test "install.env: an agent re-run needs no flags; its token comes back from the Secret" {
  write_install_env 'KWERFT_MODE=agent\nKWERFT_CONSOLE=https://ops.example.com\nKWERFT_PLATFORM=cloud\n'
  STUBS='k3s() { :; }; kc() { [[ "$*" == "-n kwerft-system get secret kwerft-agent -o jsonpath={.data.token}" ]] && printf %s "$AGENT_TOKEN" | base64; }'
  run effective --version 0.6.0 --yes
  [ "$status" -eq 0 ]
  [ "$output" = "mode=agent email= acme= platform=cloud iface= lite=0 ssh=0 channel=stable console=https://ops.example.com token=$AGENT_TOKEN" ]
  # Before k3s runs there is no Secret to read: the console's command is needed.
  STUBS='kc() { return 1; }'
  run effective
  [ "$status" -eq 2 ]
  [[ "$output" == *"--agent needs --console and --cluster-token"* ]]
}

@test "install.env: a flag that chooses the mode wins over the remembered one" {
  write_install_env 'KWERFT_MODE=agent\nKWERFT_CONSOLE=https://ops.example.com\n'
  run effective --join https://ops.example.com --token t
  [ "$status" -eq 0 ]
  [[ "$output" == "mode=join "*"console= "* ]]
  run effective --uninstall
  [[ "$output" == "mode=uninstall "* ]]
}

@test "install.env: a joined node re-runs in join mode without the join command" {
  write_install_env 'KWERFT_MODE=join\nKWERFT_PLATFORM=cloud\n'
  STUBS='stage_done() { [[ $1 == join ]]; }'
  run effective --yes
  [ "$status" -eq 0 ]
  [[ "$output" == "mode=join "* ]]
  # Not joined yet: the join command is needed again.
  STUBS='stage_done() { return 1; }'
  run effective --yes
  [ "$status" -eq 2 ]
  [[ "$output" == *"has not joined its cluster yet"* ]]
}

@test "install.env: installed before it existed, the stages tell the mode" {
  STUBS='stage_done() { [[ $1 == join ]]; }'
  run effective
  [[ "$output" == "mode=join "* ]]
  STUBS='stage_done() { [[ $1 == kubernetes ]]; }'
  run effective
  [[ "$output" == "mode=install "* ]]
}

@test "main remembers the settings once preflight passed, never in a dry run" {
  [ "$(sed -n '/^main() {/,/^}/p' "$SCRIPT" | grep -A1 'run_stage preflight' | tail -n1 | tr -d ' ')" = "remember_settings" ]
  grep -qF '  if (( DRY_RUN )) || [[ ! -w ' "$SCRIPT"
}

# ---- --progress -------------------------------------------------------------

progress_env() {
  KWERFT_SOURCED=1 source "$SCRIPT"
  PROGRESS_FILE="$BATS_TEST_TMPDIR/progress.jsonl"; : >"$PROGRESS_FILE"
  LOG_FILE="$BATS_TEST_TMPDIR/install.log"; : >"$LOG_FILE"
  DONE=""
  stage_done() { [[ " $DONE " == *" $1 "* ]]; }
  mark_done() { :; }
}
# line <n> prints line n of the progress file without its timestamp.
line() { sed -n "${1}p" "$PROGRESS_FILE" | sed -E 's/,"at":"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z"}$/}/'; }

@test "progress: a JSON line per stage with its summary" {
  progress_env
  network() { echo "noise"; echo 'Cilium 1.20.2 · "WireGuard" \ Hubble'; }
  run_stage network "Network" network force >/dev/null
  DONE="registry"
  run_stage registry "Registry mirror" network >/dev/null
  [ "$(line 1)" = '{"id":"network","label":"Network","state":"ok","detail":"Cilium 1.20.2 · \"WireGuard\" \\ Hubble"}' ]
  [ "$(line 2)" = '{"id":"registry","label":"Registry mirror","state":"skip","detail":"already done"}' ]
  grep -qE '"at":"[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:]{8}Z"}$' "$PROGRESS_FILE"
  [ "$(wc -l <"$PROGRESS_FILE" | tr -d ' ')" -eq 2 ]
  # Without --progress nothing is written anywhere.
  PROGRESS_FILE=""
  run_stage network "Network" network force >/dev/null
}

@test "progress: a failed stage gets one line, with die's message" {
  progress_env
  CURRENT_STAGE=Helm CURRENT_STAGE_ID=helm
  (die 40 "Helm checksum mismatch") 2>/dev/null || true
  on_error 40 123 2>/dev/null   # the top-level shell reports it again
  [ "$(line 1)" = '{"id":"helm","label":"Helm","state":"fail","detail":"Helm checksum mismatch"}' ]
  [ "$(wc -l <"$PROGRESS_FILE" | tr -d ' ')" -eq 1 ]
  # A command that fails without die: the line says where.
  CURRENT_STAGE=Network CURRENT_STAGE_ID=network
  on_error 1 77 2>/dev/null
  [ "$(line 2)" = "{\"id\":\"network\",\"label\":\"Network\",\"state\":\"fail\",\"detail\":\"exit 1 at line 77; see $LOG_FILE\"}" ]
  # Outside a stage (arguments, mode checks) only the exit line is written.
  CURRENT_STAGE_ID=""
  (die 2 "Unknown option") 2>/dev/null || true
  [ "$(wc -l <"$PROGRESS_FILE" | tr -d ' ')" -eq 2 ]
}

@test "progress: the installer ends the file with its exit code" {
  [[ $EUID -ne 0 ]] || skip "would install: runs as root"
  f="$BATS_TEST_TMPDIR/progress.jsonl"
  echo stale >"$f"
  run "$SCRIPT" --dry-run --platform cloud --domain ops.example.com --progress "$f"
  [ "$status" -eq 0 ]
  [ "$(cat "$f")" = '{"exit":0}' ]
  # A usage error before anything ran.
  rm "$f"
  run "$SCRIPT" --progress "$f" --platform aws
  [ "$status" -eq 2 ]
  [ "$(cat "$f")" = '{"exit":2}' ]
  # Preflight refuses to run without root: one fail line, then the exit code.
  run "$SCRIPT" --platform dedicated --domain ops.example.com --yes --progress "$f"
  [ "$status" -eq 10 ]
  [ "$(sed -n 1p "$f" | sed -E 's/,"at":"[^"]*"}$/}/')" = '{"id":"preflight","label":"Preflight","state":"fail","detail":"Run as root (sudo)."}' ]
  [ "$(sed -n 2p "$f")" = '{"exit":10}' ]
  [ "$(wc -l <"$f" | tr -d ' ')" -eq 2 ]
}

@test "--progress needs an existing directory" {
  run "$SCRIPT" --dry-run --platform cloud --progress "$BATS_TEST_TMPDIR/missing/progress.jsonl"
  [ "$status" -eq 2 ]
  [[ "$output" == *"--progress: the directory"* ]]
}

@test "json_str escapes what JSON needs and drops terminal colours" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  [ "$(json_str $'a\\b"c\td\ne\033[31mf · ›')" = '"a\\b\"c\td\ne[31mf · ›"' ]
  [ "$(json_str "")" = '""' ]
}

# ---- converging stages, k3s versions ----------------------------------------

@test "system and helm converge on every run; kubernetes and join run once" {
  body=$(sed -n '/^main() {/,/^}/p' "$SCRIPT")
  grep -qE 'run_stage system +"System" +stage_system force$' <<<"$body"
  grep -qE 'run_stage helm +"Helm" +stage_helm force$' <<<"$body"
  grep -qE 'run_stage upgrades +"Upgrades" +stage_upgrades force$' <<<"$body"
  grep -qE 'run_stage kubernetes +"Kubernetes" +stage_kubernetes$' <<<"$body"
  grep -qE 'run_stage join "Join cluster" stage_join$' <<<"$body"
}

@test "k3s_older compares k3s releases" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  k3s_older v1.37.1+k3s1 v1.37.2+k3s1
  k3s_older v1.36.9+k3s3 v1.37.0+k3s1
  k3s_older v1.37.1+k3s1 v1.37.1+k3s2
  k3s_older v1.9.0+k3s1 v1.10.0+k3s1
  ! k3s_older v1.37.1+k3s1 v1.37.1+k3s1
  ! k3s_older v1.38.0+k3s1 v1.37.9+k3s1
  ! k3s_older garbage v1.37.1+k3s1
}

@test "a skipped Kubernetes stage says when k3s is older than the pin" {
  progress_env
  DONE="kubernetes"
  k3s() { echo "k3s version v1.36.4+k3s1 (0123abcd)"; echo "go version go1.25"; }
  run_stage kubernetes "Kubernetes" stage_kubernetes >"$BATS_TEST_TMPDIR/out"
  [[ "$(<"$BATS_TEST_TMPDIR/out")" == *"k3s v1.36.4+k3s1 is older than this release's $K3S_VERSION: upgrade it in Settings › Updates"* ]]
  [[ "$(line 1)" == *'"state":"skip","detail":"k3s v1.36.4+k3s1 is older than this release'* ]]
  k3s() { echo "k3s version $K3S_VERSION (0123abcd)"; }
  run_stage kubernetes "Kubernetes" stage_kubernetes >"$BATS_TEST_TMPDIR/out"
  [[ "$(<"$BATS_TEST_TMPDIR/out")" == *"k3s $K3S_VERSION · installed"* ]]
  # A joined node says the same about its own k3s.
  DONE="join"
  k3s() { echo "k3s version v1.36.4+k3s1 (0123abcd)"; }
  run_stage join "Join cluster" stage_join >"$BATS_TEST_TMPDIR/out"
  [[ "$(<"$BATS_TEST_TMPDIR/out")" == *"is older than this release's"* ]]
}

@test "--k3s-version: a k3s release, used by new servers and joins" {
  run "$SCRIPT" --dry-run --platform cloud --join https://ops.example.com --token t --k3s-version v1.36.4+k3s1
  [ "$status" -eq 0 ]
  run "$SCRIPT" --dry-run --platform cloud --agent --console https://ops.example.com --cluster-token "$AGENT_TOKEN" --k3s-version v1.38.0-rc1+k3s1
  [ "$status" -eq 0 ]
  for bad in 1.36.4 v1.36.4 latest "v1.36.4+k3s1;x"; do
    run "$SCRIPT" --dry-run --platform cloud --k3s-version "$bad"
    [ "$status" -eq 2 ]
    [[ "$output" == *"--k3s-version must be a k3s release"* ]]
  done
  # KWERFT_K3S_VERSION (e2e) works like the flag.
  [ "$(KWERFT_K3S_VERSION=v1.36.1+k3s2 effective_k3s)" = "v1.36.1+k3s2" ]
  KWERFT_SOURCED=1 source "$SCRIPT"
  [ "$(k3s_target)" = "$K3S_VERSION" ]
  parse_args --k3s-version v1.36.4+k3s1
  [ "$(k3s_target)" = "v1.36.4+k3s1" ]
  # Both k3s installs take it.
  [ "$(sed -n '/^stage_kubernetes() {/,/^}/p' "$SCRIPT" | grep -c 'version=$(k3s_target)\|INSTALL_K3S_VERSION="$version"')" -eq 2 ]
  [ "$(sed -n '/^stage_join() {/,/^}/p' "$SCRIPT" | grep -c 'version=$(k3s_target)\|INSTALL_K3S_VERSION="$version"')" -eq 2 ]
  ! grep -qF 'INSTALL_K3S_VERSION="$K3S_VERSION"' "$SCRIPT"
}
effective_k3s() { ( KWERFT_SOURCED=1 source "$SCRIPT"; k3s_target ); }

@test "node pools pass the installer's --k3s-version in its format" {
  root="$BATS_TEST_DIRNAME/../.."
  grep -qF '"--k3s-version", j.K3sVersion' "$root/internal/controllers/nodepool_cloudinit.go"
  # The Go pattern and the installer's agree.
  re=$(sed -n 's/^var k3sVersionRE = regexp.MustCompile(`\(.*\)`)$/\1/p' "$root/internal/controllers/nodepool_cloudinit.go")
  [ -n "$re" ]
  [ "$re" = "$(sed -n '/^valid_k3s_version() {/,/^}/p' "$SCRIPT" | sed -n 's/.*=~ \(.*\) \]\]$/\1/p')" ]
}

# ---- stage upgrades ---------------------------------------------------------

upgrades_env() {
  KWERFT_SOURCED=1 source "$SCRIPT"
  LOG_FILE="$BATS_TEST_TMPDIR/install.log"; : >"$LOG_FILE"
  KC_LOG="$BATS_TEST_TMPDIR/kc.log"; : >"$KC_LOG"
  READY=True LABELLED="kwerft-1"
  hostname() { echo "Kwerft-1"; }
  kc() {
    printf 'kc %s\n' "$*" >>"$KC_LOG"
    case "$*" in
      "get node kwerft-1") return 0 ;;
      "get node "*" -o jsonpath={.status.conditions"*) echo "$READY" ;;
      "get nodes -l kwerft.dev/installer=true"*) echo "$LABELLED" ;;
    esac
    return 0
  }
}

@test "stage_upgrades: system-upgrade-controller at its pin, and the installer node's label" {
  upgrades_env
  LABELLED="kwerft-1 kwerft-old"
  run stage_upgrades
  [ "$status" -eq 0 ]
  [ "$output" = "system-upgrade-controller $SYSTEM_UPGRADE_CONTROLLER_VERSION (ready) · installer node kwerft-1" ]
  base="https://github.com/rancher/system-upgrade-controller/releases/download/$SYSTEM_UPGRADE_CONTROLLER_VERSION"
  # The CRDs first, established, then the controller.
  [ "$(grep -n "apply --server-side --force-conflicts -f $base/crd.yaml" "$KC_LOG" | cut -d: -f1)" -lt \
    "$(grep -n "apply --server-side --force-conflicts -f $base/system-upgrade-controller.yaml" "$KC_LOG" | cut -d: -f1)" ]
  grep -q "wait --for=condition=Established crd/plans.upgrade.cattle.io" "$KC_LOG"
  grep -q "rollout status deployment/system-upgrade-controller" "$KC_LOG"
  grep -qx "kc label node kwerft-1 kwerft.dev/installer=true --overwrite" "$KC_LOG"
  # Only one node is the installer's.
  grep -qx "kc label node kwerft-old kwerft.dev/installer-" "$KC_LOG"
  ! grep -q "label node kwerft-1 kwerft.dev/installer-" "$KC_LOG"
}

@test "stage_upgrades: on a first install the controller starts with the network" {
  upgrades_env
  READY=False
  run stage_upgrades
  [ "$status" -eq 0 ]
  [ "$output" = "system-upgrade-controller $SYSTEM_UPGRADE_CONTROLLER_VERSION (starts with the network) · installer node kwerft-1" ]
  ! grep -q "rollout status" "$KC_LOG"
}

@test "installer_node: by host name, else by this server's address" {
  upgrades_env
  [ "$(installer_node)" = "kwerft-1" ]
  hostname() { echo "srv-renamed"; }
  PRIVATE_IP=10.0.0.2
  kc() {
    case "$*" in
      "get nodes -o jsonpath="*) printf 'kwerft-1 10.0.0.2\nkwerft-2 10.0.0.3\n' ;;
      *) return 1 ;;
    esac
  }
  [ "$(installer_node)" = "kwerft-1" ]
  PRIVATE_IP=10.0.0.9
  [ -z "$(installer_node)" ]
  run label_installer_node
  [ "$status" -eq 30 ]
}

@test "system-upgrade-controller's pin is a release, dated with the others" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  [[ "$SYSTEM_UPGRADE_CONTROLLER_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]
  grep -B2 '^SYSTEM_UPGRADE_CONTROLLER_VERSION=' "$SCRIPT" | grep -q '20[0-9][0-9]-[0-9][0-9]-[0-9][0-9]'
}

@test "--help documents the new flags and what a re-run converges" {
  run "$SCRIPT" --help
  [[ "$output" == *"--progress FILE"* ]]
  [[ "$output" == *"--k3s-version V"* ]]
  [[ "$output" == *"/var/lib/kwerft/install.env"* ]]
  [[ "$output" != *"Give it on every run"* ]]
  ! grep -q 'Re-running repairs a broken install or upgrades it' "$SCRIPT"
}

# ---------------------------------------------------------------------------
# Backups (docs/phase6.md): etcd snapshots, Velero, --restore
# ---------------------------------------------------------------------------

# A valid recovery key as the console shows it (52 base32 characters in
# groups of four), lower case and with dashes to test normalization.
RECOVERY_KEY_SHOWN="abcd-efgh-ijkl-mnop-qrst-uvwx-yz23-4567-abcd-efgh-ijkl-mnop-qrst"
RECOVERY_KEY_PASSWORD="ABCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRST"

# restore_config [extra backups lines…] writes a --config with a backups block
# and its key files; prints its path.
restore_config() {
  local d="$BATS_TEST_TMPDIR/restore" l
  mkdir -p "$d"
  printf 'AKIA-ACCESS\n' >"$d/s3.access"
  printf 'SECRET-KEY-VALUE\n' >"$d/s3.secret"
  printf '%s\n' "$RECOVERY_KEY_SHOWN" >"$d/recovery.key"
  {
    echo "backups:"
    echo "  endpoint: ${ENDPOINT:-https://fsn1.your-objectstorage.com}"
    echo "  bucket: acme-kwerft"
    echo "  accessKeyFile: $d/s3.access"
    echo "  secretKeyFile: $d/s3.secret"
    echo "  recoveryKeyFile: $d/recovery.key"
    for l in "$@"; do echo "  $l"; done
  } >"$d/kwerft.yaml"
  printf '%s' "$d/kwerft.yaml"
}

@test "velero pins are versions, dated with the other pins" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  [[ "$VELERO_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]
  [[ "$VELERO_CHART_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]
  [[ "$VELERO_PLUGIN_AWS_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]
  grep -q "^# Backups (docs/phase6.md), latest stable as of 20[0-9-]*\." "$SCRIPT"
}

@test "--help documents --restore and exit code 60" {
  run "$SCRIPT" --help
  [ "$status" -eq 0 ]
  [[ "$output" == *"--restore B"* ]]
  [[ "$output" == *"60 restore"* ]]
  grep -q '^readonly EXIT_RESTORE=60' "$SCRIPT"
}

@test "--dry-run --restore restores before Kwerft is installed" {
  cfg=$(restore_config "prefix: ops.example.com")
  run "$SCRIPT" --dry-run --platform cloud --config "$cfg" --restore latest
  [ "$status" -eq 0 ]
  expected="Preflight System Firewall Kubernetes Registry Helm Upgrades Network Hetzner Ingress Observability Backups Restore Kwerft Handoff"
  actual=$(printf '%s\n' "$output" | sed -n 's/^→ \([A-Za-z]*\).*/\1/p' | tr '\n' ' ' | sed 's/ $//')
  [ "$actual" = "$expected" ]
  [[ "$output" == *"Restoring backup latest from s3://acme-kwerft/ops.example.com/velero at https://fsn1.your-objectstorage.com; the console hostname comes from the backup."* ]]
  [[ "$output" != *"temporary hostname"* ]]
}

@test "--restore: what it needs and what it refuses" {
  cfg=$(restore_config "prefix: ops.example.com")
  run "$SCRIPT" --dry-run --restore latest
  [ "$status" -eq 2 ]
  [[ "$output" == *"--restore needs --config with a backups block"* ]]
  run "$SCRIPT" --dry-run --config "$cfg" --restore latest --lite
  [ "$status" -eq 2 ]
  [[ "$output" == *"--lite leaves out"* ]]
  run "$SCRIPT" --dry-run --config "$cfg" --restore 'Kwerft Cluster!'
  [ "$status" -eq 2 ]
  [[ "$output" == *"--restore takes latest or a backup's name"* ]]
  run "$SCRIPT" --dry-run --join https://ops.example.com --token t --restore latest
  [ "$status" -eq 2 ]
  [[ "$output" == *"does not combine with --agent, --join"* ]]
  # A named backup is fine.
  run "$SCRIPT" --dry-run --config "$cfg" --restore kwerft-cluster-20261005030000
  [ "$status" -eq 0 ]
}

@test "--restore: the backups block is checked" {
  cfg=$(restore_config)
  # No prefix and no --domain: the folder is unknown.
  run "$SCRIPT" --dry-run --config "$cfg" --restore latest
  [ "$status" -eq 2 ]
  [[ "$output" == *"backups.prefix in $cfg is needed"* ]]
  # --domain is the default prefix.
  run "$SCRIPT" --dry-run --config "$cfg" --restore latest --domain ops.example.com
  [ "$status" -eq 0 ]
  [[ "$output" == *"s3://acme-kwerft/ops.example.com/velero"* ]]
  [[ "$output" != *"comes from the backup"* ]]

  cfg=$(ENDPOINT=http://insecure.example.com restore_config "prefix: ops.example.com")
  run "$SCRIPT" --dry-run --config "$cfg" --restore latest
  [ "$status" -eq 2 ]
  [[ "$output" == *"backups.endpoint"* ]]

  cfg=$(restore_config "prefix: ../escape")
  run "$SCRIPT" --dry-run --config "$cfg" --restore latest
  [ "$status" -eq 2 ]
  [[ "$output" == *"backups.prefix"* ]]

  cfg=$(restore_config "prefix: ops.example.com")
  rm "$BATS_TEST_TMPDIR/restore/s3.secret"
  run "$SCRIPT" --dry-run --config "$cfg" --restore latest
  [ "$status" -eq 2 ]
  [[ "$output" == *"backups.secretKeyFile in $cfg not readable"* ]]

  cfg=$(restore_config "prefix: ops.example.com")
  printf 'not-a-key\n' >"$BATS_TEST_TMPDIR/restore/recovery.key"
  run "$SCRIPT" --dry-run --config "$cfg" --restore latest
  [ "$status" -eq 2 ]
  [[ "$output" == *"does not hold a recovery key"* ]]
  # The key itself is never printed.
  [[ "$output" != *"not-a-key"* ]]
}

@test "parse_restore_args: region from a Hetzner endpoint, prefix without slashes" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  CONFIG_FILE=$(restore_config "prefix: /ops.example.com/")
  RESTORE_FROM=latest; MODE=install
  parse_restore_args
  [ "$BACKUP_REGION" = "fsn1" ]
  [ "$BACKUP_PREFIX" = "ops.example.com" ]
  [ "$BACKUP_ENDPOINT" = "https://fsn1.your-objectstorage.com" ]
  CONFIG_FILE=$(restore_config "prefix: p" "region: nbg1")
  parse_restore_args
  [ "$BACKUP_REGION" = "nbg1" ]
  # Without --restore the block is not read.
  RESTORE_FROM=""; BACKUP_BUCKET=""
  parse_restore_args
  [ -z "$BACKUP_BUCKET" ]
}

@test "recovery_key: the console's grouping becomes Velero's repository password" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  [ "$(printf '%s\n' "$RECOVERY_KEY_SHOWN" | recovery_key)" = "$RECOVERY_KEY_PASSWORD" ]
  [ "$(printf 'abcd efgh\nijkl\n' | recovery_key)" = "ABCDEFGHIJKL" ]
  # The file Settings › Backups offers for download (web/src/backups.ts
  # recoveryKeyFile): the key on its own line among others.
  printf 'Kwerft recovery key\n\n%s\n\nConsole: ops.example.com\nCreated: 2026-10-05T12:00:00.000Z\n\nBackups of this console cannot be read without this key.\n' \
    "$(tr '[:lower:]' '[:upper:]' <<<"$RECOVERY_KEY_SHOWN")" >"$BATS_TEST_TMPDIR/download.txt"
  [ "$(recovery_key <"$BATS_TEST_TMPDIR/download.txt")" = "$RECOVERY_KEY_PASSWORD" ]
  [ "$(recovery_key <"$BATS_TEST_TMPDIR/download.txt" | sse_customer_key)" = "be0a7fb9dc10ee8cdbe36ee51a34a723ea6b6ab9c6d87aa824d051b73e4d7e50" ]
  # No line break at the end: it is the repository password as it is.
  [ "$(printf '%s\n' "$RECOVERY_KEY_SHOWN" | recovery_key | wc -c | tr -d ' ')" = "52" ]
}

# The SSE-C key of RECOVERY_KEY_SHOWN: the same vector as the console's
# TestSSECustomerKey (internal/backups), so --restore reads what the
# console's backups were encrypted with.
SSE_KEY_HEX="be0a7fb9dc10ee8cdbe36ee51a34a723ea6b6ab9c6d87aa824d051b73e4d7e50"

@test "sse_customer_key: the console's vector, from the key file as the owner keeps it" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  [ "$(printf '%s\n' "$RECOVERY_KEY_SHOWN" | recovery_key | sse_customer_key)" = "$SSE_KEY_HEX" ]
  [ "$(printf '%s' "$RECOVERY_KEY_PASSWORD" | sse_customer_key)" = "$SSE_KEY_HEX" ]
  # Another key, another SSE-C key.
  [ "$(printf 'BBCDEFGHIJKLMNOPQRSTUVWXYZ234567ABCDEFGHIJKLMNOPQRST' | sse_customer_key)" != "$SSE_KEY_HEX" ]
}

@test "hmac_sha256 and HKDF: RFC 4231 and RFC 5869 test vectors" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  # RFC 4231 test cases 1 and 2.
  [ "$(hmac_sha256 0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b "$(str_hex "Hi There")")" = \
    "b0344c61d8db38535ca8afceaf0bf12b881dc200c9833da726e9376c2e32cff7" ]
  [ "$(hmac_sha256 "$(str_hex Jefe)" "$(str_hex "what do ya want for nothing?")")" = \
    "5bdcc146bf60754e6a042426089575c75a003f089d2739839dec58b964ec3843" ]
  # RFC 5869 test case 1: extract, then the first block of expand. The PRK
  # holds a byte 0x36, which makes a NUL in the inner pad.
  prk=$(hmac_sha256 000102030405060708090a0b0c 0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b)
  [ "$prk" = "077709362c2e32df0ddc3f0dc47bba6390b6c73bb50f9c3122ec844ad7c2b3e5" ]
  [ "$(hmac_sha256 "$prk" f0f1f2f3f4f5f6f7f8f901)" = "3cb25f25faacd57a90434f64d0362f2a2d2d0a90cf1a5a4c5db02d56ecc4c5bf" ]
  # Not hex, or a key over 64 bytes: refused.
  run hmac_sha256 xyz 00
  [ "$status" -ne 0 ]
  run hmac_sha256 "$(printf '%0130d' 0)" 00
  [ "$status" -ne 0 ]
}

@test "sse_customer_key agrees with openssl's HKDF" {
  command -v openssl >/dev/null && openssl kdf -help >/dev/null 2>&1 || skip "no openssl 3"
  KWERFT_SOURCED=1 source "$SCRIPT"
  want=$(openssl kdf -keylen 32 -kdfopt digest:SHA256 -kdfopt "key:$RECOVERY_KEY_PASSWORD" \
    -kdfopt salt:kwerft.dev/recovery-key -kdfopt info:kwerft.dev/backups/sse-c/v1 HKDF | tr -d ':\n' | tr '[:upper:]' '[:lower:]')
  [ "$want" = "$SSE_KEY_HEX" ]
}

# etcd snapshots: the k3s config and its drop-in in the test directory,
# systemctl stubbed (SYSTEMCTL_LOG, ACTIVE as for the registry mirror) and
# ConsoleSettings answered from SCHEDULE and RETENTION.
etcd_env() {
  KWERFT_SOURCED=1 source "$SCRIPT"
  K3S_CONFIG_FILE="$BATS_TEST_TMPDIR/k3s/config.yaml"
  K3S_ETCD_CONFIG_FILE="$BATS_TEST_TMPDIR/k3s/config.yaml.d/50-kwerft-etcd-snapshots.yaml"
  mkdir -p "$BATS_TEST_TMPDIR/k3s"
  printf '# Managed by Kwerft installer.\ncluster-init: true\n' >"$K3S_CONFIG_FILE"
  LOG_FILE="$BATS_TEST_TMPDIR/install.log"; : >"$LOG_FILE"
  SYSTEMCTL_LOG="$BATS_TEST_TMPDIR/systemctl.log"; : >"$SYSTEMCTL_LOG"
  ACTIVE=k3s; SCHEDULE=""; RETENTION=""
  systemctl() {
    case "$1" in
      is-active) [[ "$3" == "$ACTIVE" ]] ;;
      *) printf '%s\n' "$*" >>"$SYSTEMCTL_LOG" ;;
    esac
  }
  kc() {
    case "$*" in
      *"{.spec.backups.etcdSnapshots.schedule}"*) printf '%s' "$SCHEDULE" ;;
      *"{.spec.backups.etcdSnapshots.retention}"*) printf '%s' "$RETENTION" ;;
    esac
    return 0
  }
}

@test "etcd_snapshot_config: local snapshots only, never k3s's unencrypted S3 upload" {
  etcd_env
  run etcd_snapshot_config "0 */6 * * *" 28
  [ "$status" -eq 0 ]
  [[ "$output" == *'etcd-snapshot-schedule-cron: "0 */6 * * *"'* ]]
  [[ "$output" == *"etcd-snapshot-retention: 28"* ]]
  [[ "$output" == *"etcd-snapshot-compress: true"* ]]
  # Kwerft's etcd snapshot agent uploads them, encrypted; k3s cannot.
  ! printf '%s\n' "$output" | grep -q '^etcd-s3'
}

@test "ensure_etcd_snapshots: a drop-in with k3s's S3 upload is rewritten, k3s restarted" {
  etcd_env
  mkdir -p "$(dirname "$K3S_ETCD_CONFIG_FILE")"
  # As install.sh wrote it before Kwerft uploaded the snapshots itself.
  cat >"$K3S_ETCD_CONFIG_FILE" <<'EOF'
# Managed by Kwerft installer (etcd snapshots).
# k3s reads this file when it starts; the installer restarts k3s when it changes.
etcd-snapshot-schedule-cron: "0 */6 * * *"
etcd-snapshot-retention: 28
etcd-snapshot-compress: true
etcd-s3: true
etcd-s3-config-secret: kwerft-etcd-s3
EOF
  run ensure_etcd_snapshots
  [ "$status" -eq 0 ]
  [ "$output" = "etcd snapshots (0 */6 * * *, 28 kept) · k3s restarted" ]
  grep -qx "restart k3s" "$SYSTEMCTL_LOG"
  absent 'etcd-s3' "$K3S_ETCD_CONFIG_FILE"
  grep -qx 'etcd-snapshot-compress: true' "$K3S_ETCD_CONFIG_FILE"
  [ "$(stat -c %a "$K3S_ETCD_CONFIG_FILE" 2>/dev/null || stat -f %Lp "$K3S_ETCD_CONFIG_FILE")" = "600" ]
  # The next run leaves it alone.
  : >"$SYSTEMCTL_LOG"
  run ensure_etcd_snapshots
  [ "$output" = "etcd snapshots (0 */6 * * *, 28 kept)" ]
  [ ! -s "$SYSTEMCTL_LOG" ]
}

@test "ensure_etcd_snapshots: defaults, restart only when the drop-in changes" {
  etcd_env
  run ensure_etcd_snapshots
  [ "$status" -eq 0 ]
  [ "$output" = "etcd snapshots (0 */6 * * *, 28 kept) · k3s restarted" ]
  grep -qx "restart k3s" "$SYSTEMCTL_LOG"
  grep -qx 'etcd-snapshot-schedule-cron: "0 \*/6 \* \* \*"' "$K3S_ETCD_CONFIG_FILE"
  [ "$(stat -c %a "$K3S_ETCD_CONFIG_FILE" 2>/dev/null || stat -f %Lp "$K3S_ETCD_CONFIG_FILE")" = "600" ]

  : >"$SYSTEMCTL_LOG"
  run ensure_etcd_snapshots
  [ "$output" = "etcd snapshots (0 */6 * * *, 28 kept)" ]
  [ ! -s "$SYSTEMCTL_LOG" ]

  # Settings › Backups changed the schedule: the next run applies it.
  SCHEDULE="30 2 * * *"; RETENTION="7"
  run ensure_etcd_snapshots
  [ "$output" = "etcd snapshots (30 2 * * *, 7 kept) · k3s restarted" ]
  grep -qx 'etcd-snapshot-retention: 7' "$K3S_ETCD_CONFIG_FILE"
}

@test "ensure_etcd_snapshots: invalid settings fall back to the defaults with a warning" {
  etcd_env
  SCHEDULE='0 0 * * * "; rm -rf /'; RETENTION="9999"
  run ensure_etcd_snapshots
  [ "$status" -eq 0 ]
  [[ "$output" == *"is not a cron schedule"* ]]
  [[ "$output" == *"is not between 1 and 500"* ]]
  [[ "$output" == *"etcd snapshots (0 */6 * * *, 28 kept)"* ]]
  absent 'rm -rf' "$K3S_ETCD_CONFIG_FILE"
  SCHEDULE="@daily"; RETENTION="0028"
  run ensure_etcd_snapshots
  [[ "$output" == *"etcd snapshots (@daily, 28 kept)"* ]]
  [[ "$output" == *"is not between 1 and 500"* ]]
}

@test "write_etcd_snapshot_config: nothing without Kwerft's embedded etcd" {
  etcd_env
  printf 'server: https://10.0.0.2:6443\n' >"$K3S_CONFIG_FILE"
  [ "$(write_etcd_snapshot_config "0 */6 * * *" 28)" = "none" ]
  [ ! -e "$K3S_ETCD_CONFIG_FILE" ]
  run ensure_etcd_snapshots
  [[ "$output" == "etcd snapshots as k3s has them"* ]]
  [ ! -s "$SYSTEMCTL_LOG" ]
}

@test "stage_kubernetes writes the etcd drop-in before k3s first starts" {
  sed -n '/^stage_kubernetes()/,/^}/p' "$SCRIPT" >"$BATS_TEST_TMPDIR/fn"
  line_drop_in=$(grep -n 'write_etcd_snapshot_config' "$BATS_TEST_TMPDIR/fn" | cut -d: -f1)
  line_k3s=$(grep -n 'get.k3s.io' "$BATS_TEST_TMPDIR/fn" | cut -d: -f1)
  [ -n "$line_drop_in" ]
  [ "$line_drop_in" -lt "$line_k3s" ]
}

@test "velero_values: Kopia file-system backups, the AWS plugin, no storage location" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  run velero_values
  [ "$status" -eq 0 ]
  [[ "$output" == *"tag: $VELERO_VERSION"* ]]
  [[ "$output" == *"image: docker.io/velero/velero-plugin-for-aws:$VELERO_PLUGIN_AWS_VERSION"* ]]
  [[ "$output" == *"uploaderType: kopia"* ]]
  [[ "$output" == *"defaultVolumesToFsBackup: true"* ]]
  [[ "$output" == *"deployNodeAgent: true"* ]]
  [[ "$output" == *"backupStorageLocation: []"* ]]
  [[ "$output" == *"volumeSnapshotLocation: []"* ]]
  [[ "$output" == *"snapshotsEnabled: false"* ]]
  [[ "$output" == *"useSecret: false"* ]]
  [[ "$output" == *"defaultBackupStorageLocation: kwerft"* ]]
  # The node agent runs on tainted build nodes too.
  [[ "$output" == *"- operator: Exists"* ]]
  # The AWS plugin reads the SSE-C key Secret with Velero's own account;
  # nothing is mounted for it.
  [[ "$output" == *$'rbac:\n  create: true\n  clusterAdministrator: true'* ]]
  [[ "$output" != *"customerKeyEncryptionFile"* ]]
  [[ "$output" != *"extraVolumes"* ]]
}

backups_env() {
  etcd_env
  VALUES_DIR="$BATS_TEST_TMPDIR/values"
  HELM_LOG="$BATS_TEST_TMPDIR/helm.log"; : >"$HELM_LOG"
  helmk() { printf 'helm %s\n' "$*" >>"$HELM_LOG"; }
}

@test "stage_backups: Velero at its pins; --lite keeps only the etcd snapshots" {
  backups_env
  run stage_backups
  [ "$status" -eq 0 ]
  [[ "$output" == "Velero $VELERO_VERSION · volume backups with Kopia · etcd snapshots (0 */6 * * *, 28 kept)"* ]]
  grep -q "upgrade --install velero vmware-tanzu/velero --version $VELERO_CHART_VERSION --namespace velero --create-namespace --wait" "$HELM_LOG"
  grep -q "uploaderType: kopia" "$VALUES_DIR/velero.yaml"

  : >"$HELM_LOG"; LITE=1
  run stage_backups
  [ "$status" -eq 0 ]
  [[ "$output" == "Velero off (--lite) · etcd snapshots"* ]]
  [ ! -s "$HELM_LOG" ]
}

# The restore against a stubbed cluster. kc logs every call (and what is
# piped into apply/create) to KC_LOG and answers from variables and files:
#   BSL_PHASE BSL_SYNCED BSL_MESSAGE   the BackupStorageLocation's status
#   BACKUPS                            "<completed-at> <name>" of Completed Cluster backups
#   BACKUP_PHASE BACKUP_SCOPE          a backup named on --restore (missing when no phase)
#   $RESTORE_PHASES                    one phase per read; the last one stays
#   PV_DIR REGISTRY_IP RESTORE_EXISTS
# Secrets land in SECRETS_LOG as "<secret> <key>=<content>".
restore_env() {
  KWERFT_SOURCED=1 source "$SCRIPT"
  LOG_FILE="$BATS_TEST_TMPDIR/install.log"; : >"$LOG_FILE"
  KC_LOG="$BATS_TEST_TMPDIR/kc.log"; : >"$KC_LOG"
  HELM_LOG="$BATS_TEST_TMPDIR/helm.log"; : >"$HELM_LOG"
  SECRETS_LOG="$BATS_TEST_TMPDIR/secrets.log"; : >"$SECRETS_LOG"
  HCLOUD_TMP_DIR="$BATS_TEST_TMPDIR/state"; mkdir -p "$HCLOUD_TMP_DIR"
  RESTORE_PHASES="$BATS_TEST_TMPDIR/phases"; printf 'InProgress\nCompleted\n' >"$RESTORE_PHASES"
  RESTORE_POLL=0; RESTORE_SYNC_TIMEOUT=3; RESTORE_TIMEOUT=30
  PLATFORM=dedicated; MODE=install; RESTORE_FROM=latest
  CONFIG_FILE=$(restore_config "prefix: ops.example.com")
  parse_restore_args
  BSL_PHASE=Available; BSL_SYNCED=2026-10-05T10:00:00Z; BSL_MESSAGE=""
  BACKUPS=$'2026-10-04T03:04:00Z kwerft-cluster-20261004030000\n2026-10-05T03:05:00Z kwerft-cluster-20261005030000\n2026-10-03T03:03:00Z kwerft-cluster-20261003030000'
  BACKUP_PHASE=""; BACKUP_SCOPE=Cluster; RESTORE_EXISTS=""
  PV_DIR="$BATS_TEST_TMPDIR/pv"; mkdir -p "$PV_DIR/backup"; printf 'SQLite format 3\0' >"$PV_DIR/backup/kwerft.db"
  REGISTRY_IP=10.43.7.7
  kc() {
    local a="$*" arg
    printf 'kc %s\n' "$a" >>"$KC_LOG"
    case "$a" in
      *"create secret generic"*)
        for arg in "$@"; do
          if [[ "$arg" == --from-file=sse-c-key=* ]]; then
            # 32 raw bytes: logged in hex.
            printf '%s sse-c-key=%s (%s bytes)\n' "$(sed -n 's/.*generic \([^ ]*\).*/\1/p' <<<"$a")" \
              "$(od -An -tx1 "${arg#*=*=}" | tr -d ' \n')" "$(wc -c <"${arg#*=*=}" | tr -d ' ')" >>"$SECRETS_LOG"
          elif [[ "$arg" == --from-file=*=* ]]; then
            arg=${arg#--from-file=}
            printf '%s %s=%s\n' "$(sed -n 's/.*generic \([^ ]*\).*/\1/p' <<<"$a")" "${arg%%=*}" "$(cat "${arg#*=}")" >>"$SECRETS_LOG"
          fi
        done
        printf 'kind: Secret\n' ;;
      *"apply"*"-f -"*|*"create -f -"*) cat >>"$KC_LOG" ;;
      *"get backupstoragelocations"*"{.status.phase}"*) printf '%s' "$BSL_PHASE" ;;
      *"get backupstoragelocations"*"{.status.lastSyncedTime}"*) printf '%s' "$BSL_SYNCED" ;;
      *"get backupstoragelocations"*"{.status.message}"*) printf '%s' "$BSL_MESSAGE" ;;
      *"get backups.velero.io -l kwerft.dev/backup-scope=Cluster"*) printf '%s\n' "$BACKUPS" ;;
      *"get backups.velero.io -o name"*) printf 'backup.velero.io/other\n' ;;
      *"get backups.velero.io"*"{.status.phase}"*) [[ -n "$BACKUP_PHASE" ]] || return 1; printf '%s' "$BACKUP_PHASE" ;;
      *"get backups.velero.io"*"backup-scope}"*) printf '%s' "$BACKUP_SCOPE" ;;
      *"get restores.velero.io"*"{.status.phase}"*)
        head -n1 "$RESTORE_PHASES"
        if [[ $(wc -l <"$RESTORE_PHASES") -gt 1 ]]; then
          tail -n +2 "$RESTORE_PHASES" >"$RESTORE_PHASES.next"; mv "$RESTORE_PHASES.next" "$RESTORE_PHASES"
        fi ;;
      *"get restores.velero.io"*"itemsRestored}"*) printf '412' ;;
      *"get restores.velero.io"*"totalItems}"*) printf '412' ;;
      *"get restores.velero.io"*"{.status.errors}"*) printf '3' ;;
      *"get restores.velero.io"*"failureReason}"*) printf 'error downloading backup' ;;
      *"get restores.velero.io"*) [[ -n "$RESTORE_EXISTS" ]] || return 1 ;;
      *"get pvc kwerft-data"*) printf 'pvc-1234' ;;
      *"get pv pvc-1234"*) printf '%s' "$PV_DIR" ;;
      *"get service kwerft-registry"*) printf '%s' "$REGISTRY_IP" ;;
      *"get pods"*) printf '' ;;
    esac
    return 0
  }
  helmk() { printf 'helm %s\n' "$*" >>"$HELM_LOG"; if [[ "$1" == show ]]; then printf 'kind: CustomResourceDefinition\n'; fi; }
  chown() { printf 'chown %s\n' "$*" >>"$KC_LOG"; }
  retry() { "${@:3}"; }
}

@test "stage_restore: the newest Cluster backup comes back and the database is marked" {
  restore_env
  run stage_restore
  [ "$status" -eq 0 ]
  [ "${lines[${#lines[@]}-1]}" = "backup kwerft-cluster-20261005030000 · 412 objects · console database from the backup" ]

  # Kwerft's CRDs first, for the restored kwerft.dev objects.
  grep -q "helm show crds" "$HELM_LOG"
  grep -q "kind: CustomResourceDefinition" "$KC_LOG"
  # The repository password and the S3 credentials, as the console writes them.
  grep -qx "velero-repo-credentials repository-password=$RECOVERY_KEY_PASSWORD" "$SECRETS_LOG"
  grep -q "kwerft-bsl-credentials cloud=\[default\]" "$SECRETS_LOG"
  grep -qx "aws_access_key_id=AKIA-ACCESS" "$SECRETS_LOG"
  grep -qx "aws_secret_access_key=SECRET-KEY-VALUE" "$SECRETS_LOG"
  [ "$(grep -n velero-repo-credentials "$SECRETS_LOG" | cut -d: -f1)" -lt "$(grep -n kwerft-bsl-credentials "$SECRETS_LOG" | cut -d: -f1)" ]
  # The SSE-C key the console derives (the same vector), as 32 raw bytes,
  # before the location exists.
  grep -qx "kwerft-bsl-encryption sse-c-key=$SSE_KEY_HEX (32 bytes)" "$SECRETS_LOG"
  [ "$(grep -n kwerft-bsl-encryption "$SECRETS_LOG" | cut -d: -f1)" -lt "$(grep -n kwerft-bsl-credentials "$SECRETS_LOG" | cut -d: -f1)" ]
  [ "$(grep -n 'create secret generic kwerft-bsl-encryption' "$KC_LOG" | cut -d: -f1)" -lt "$(grep -n 'kind: BackupStorageLocation' "$KC_LOG" | cut -d: -f1)" ]
  # Keys never in the log or kubectl's arguments; the temporary files are gone.
  absent "SECRET-KEY-VALUE" "$LOG_FILE"
  absent "SECRET-KEY-VALUE" "$KC_LOG"
  absent "$RECOVERY_KEY_PASSWORD" "$KC_LOG"
  absent "$SSE_KEY_HEX" "$KC_LOG"
  absent "$SSE_KEY_HEX" "$LOG_FILE"
  [ -z "$(ls "$HCLOUD_TMP_DIR")" ]

  # The location: read-only while restoring, then the console's.
  grep -q "accessMode: ReadOnly" "$KC_LOG"
  grep -q 'prefix: "ops.example.com/velero"' "$KC_LOG"
  grep -q 's3Url: "https://fsn1.your-objectstorage.com"' "$KC_LOG"
  grep -q 'region: "fsn1"' "$KC_LOG"
  grep -qF 'customerKeyEncryptionSecret: "kwerft-bsl-encryption/sse-c-key"' "$KC_LOG"
  grep -qF 'patch backupstoragelocations.velero.io kwerft --type merge -p {"spec":{"accessMode":"ReadWrite"}}' "$KC_LOG"

  # The restore.
  grep -q "name: restore-kwerft-cluster-20261005030000" "$KC_LOG"
  grep -q "backupName: kwerft-cluster-20261005030000" "$KC_LOG"
  grep -q "existingResourcePolicy: none" "$KC_LOG"
  grep -q "includeClusterResources: true" "$KC_LOG"
  grep -q 'excludedNamespaces: \["kube-system", .*"velero", .*"kwerft-observability"\]' "$KC_LOG"
  grep -q 'excludedResources: \["nodes", .*"jobs.batch", .*"customresourcedefinitions.apiextensions.k8s.io"' "$KC_LOG"
  grep -qF 'includedResources: ["builds.kwerft.dev", "tasks.kwerft.dev", "upgrades.kwerft.dev", "restores.kwerft.dev"]' "$KC_LOG"

  # Fixed up for Helm: the registry's Service gets its fixed address back,
  # a pending release revision goes.
  grep -q "delete service kwerft-registry" "$KC_LOG"
  grep -qF "delete secret -l owner=helm,name=kwerft,status in (pending-install,pending-upgrade,pending-rollback)" "$KC_LOG"

  # The marker, written while the console is stopped; the volume is the console's.
  [ -f "$PV_DIR/backup/RESTORE" ]
  grep -q "restored from backup kwerft-cluster-20261005030000" "$PV_DIR/backup/RESTORE"
  scale0=$(grep -n "scale deployment kwerft --replicas=0" "$KC_LOG" | cut -d: -f1)
  chown_line=$(grep -n "chown -R 65532:65532 $PV_DIR" "$KC_LOG" | cut -d: -f1)
  scale1=$(grep -n "scale deployment kwerft --replicas=1" "$KC_LOG" | cut -d: -f1)
  [ "$scale0" -lt "$chown_line" ]
  [ "$chown_line" -lt "$scale1" ]
}

@test "stage_restore: a re-run waits for the same restore; a correct registry address stays" {
  restore_env
  RESTORE_EXISTS=1; REGISTRY_IP=10.43.0.50
  run stage_restore
  [ "$status" -eq 0 ]
  absent "kind: Restore" "$KC_LOG"
  absent "delete service kwerft-registry" "$KC_LOG"
}

@test "pick_backup: a named backup must exist, cover the cluster and be complete" {
  restore_env
  RESTORE_FROM=kwerft-cluster-20261004030000
  run pick_backup
  [ "$status" -eq 60 ]
  [[ "$output" == *"no backup named kwerft-cluster-20261004030000 in s3://acme-kwerft/ops.example.com/velero"* ]]
  BACKUP_PHASE=Completed
  run pick_backup
  [ "$status" -eq 0 ]
  [ "$output" = "kwerft-cluster-20261004030000" ]
  BACKUP_SCOPE=Projects
  run pick_backup
  [ "$status" -eq 60 ]
  [[ "$output" == *"not a backup of the whole cluster (scope: Projects)"* ]]
  BACKUP_SCOPE=Cluster; BACKUP_PHASE=Failed
  run pick_backup
  [ "$status" -eq 60 ]
  [[ "$output" == *"did not complete (Failed)"* ]]
  BACKUP_PHASE=PartiallyFailed
  run pick_backup
  [ "$status" -eq 0 ]
  [[ "$output" == *"is incomplete"* ]]
}

@test "pick_backup: latest without any complete Cluster backup exits 60" {
  restore_env
  BACKUPS=""
  run pick_backup
  [ "$status" -eq 60 ]
  [[ "$output" == *"no complete Cluster backup in s3://acme-kwerft/ops.example.com/velero at https://fsn1.your-objectstorage.com (1 backups of any kind there)"* ]]
  [[ "$output" == *"encrypted with a key derived from the recovery key"* ]]
}

@test "wait_backup_sync: an unreadable bucket exits 60 with Velero's reason" {
  restore_env
  BSL_PHASE=Unavailable; BSL_MESSAGE="BackupStorageLocation is unavailable: AccessDenied"
  run wait_backup_sync
  [ "$status" -eq 60 ]
  [[ "$output" == *"Cannot read the backups in s3://acme-kwerft/ops.example.com/velero at https://fsn1.your-objectstorage.com: BackupStorageLocation is unavailable: AccessDenied."* ]]
  [[ "$output" == *"Check backups.endpoint, region, bucket, prefix and the access keys"* ]]
  BSL_PHASE=Available; BSL_SYNCED=""
  run wait_backup_sync
  [ "$status" -eq 60 ]
  [[ "$output" == *"has not listed its backups"* ]]
}

@test "wait_restore: failures exit 60, partial restores warn, slow ones time out" {
  restore_env
  printf 'InProgress\nFailed\n' >"$RESTORE_PHASES"
  run wait_restore restore-x
  [ "$status" -eq 60 ]
  [[ "$output" == *"failed: error downloading backup"* ]]
  printf 'PartiallyFailed\n' >"$RESTORE_PHASES"
  run wait_restore restore-x
  [ "$status" -eq 0 ]
  [[ "$output" == *"finished with 3 errors"* ]]
  [ "${lines[${#lines[@]}-1]}" = "PartiallyFailed" ]
  printf 'InProgress\n' >"$RESTORE_PHASES"; RESTORE_TIMEOUT=2
  run wait_restore restore-x
  [ "$status" -eq 60 ]
  [[ "$output" == *"did not finish within"* ]]
}

@test "mark_database_restore: a backup without the hook's copy exits 60" {
  restore_env
  rm "$PV_DIR/backup/kwerft.db"
  run mark_database_restore kwerft-cluster-1
  [ "$status" -eq 60 ]
  [[ "$output" == *"has no copy of the console's database"* ]]
  [ ! -e "$PV_DIR/backup/RESTORE" ]
  absent "scale deployment" "$KC_LOG"
  PV_DIR="$BATS_TEST_TMPDIR/nowhere"
  run mark_database_restore kwerft-cluster-1
  [ "$status" -eq 60 ]
  [[ "$output" == *"not a directory on this server"* ]]
}

@test "restore_cloud_volumes: the CSI driver once the Cloud token is back" {
  restore_env
  stage_hcloud() { echo "hcloud stage ran" >>"$KC_LOG"; }
  stored_hcloud_token() { printf ''; }
  restore_cloud_volumes
  absent "hcloud stage ran" "$KC_LOG"
  PLATFORM=cloud
  restore_cloud_volumes
  absent "hcloud stage ran" "$KC_LOG"
  stored_hcloud_token() { printf 'token'; }
  helmk() { [[ "$*" != *"status hcloud-csi"* ]]; }
  restore_cloud_volumes
  grep -q "hcloud stage ran" "$KC_LOG"
  : >"$KC_LOG"
  helmk() { return 0; }   # installed already
  restore_cloud_volumes
  absent "hcloud stage ran" "$KC_LOG"
}

@test "restore_refused: a server running Kwerft refuses --restore, a restored one resumes" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  DONE=""
  stage_done() { [[ " $DONE " == *" $1 "* ]]; }
  run restore_refused
  [ "$status" -ne 0 ]
  DONE="kubernetes backups"
  run restore_refused
  [ "$status" -ne 0 ]
  DONE="kwerft handoff"
  run restore_refused
  [ "$status" -eq 0 ]
  DONE="kwerft-agent"
  run restore_refused
  [ "$status" -eq 0 ]
  DONE="restore kwerft handoff"
  run restore_refused
  [ "$status" -ne 0 ]
}

@test "Handoff and summary after a restore: accounts from the backup" {
  summary_env
  RESTORE_FROM=latest; CONFIG_FILE=/root/kwerft.yaml
  [ "$(setup_token_state)" = "restored" ]
  run print_summary
  [ "$status" -eq 0 ]
  [[ "$output" == *"Sign in with your accounts"* ]]
  [[ "$output" != *"owner account"* ]]
  [[ "$output" == *"point ops.example.com and the apps hostnames at this server: 203.0.113.24"* ]]
  SETTING_RECORDS=true
  run print_summary
  [[ "$output" == *"Kwerft points its records at this server (203.0.113.24) by itself"* ]]
}
