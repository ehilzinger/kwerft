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
  expected="Preflight System Firewall Kubernetes Registry Helm Network Ingress Observability Kwerft Handoff"
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
# must name the same registry and images.
@test "registry and build pins agree with the chart and internal/builds" {
  KWERFT_SOURCED=1 source "$SCRIPT"
  root="$BATS_TEST_DIRNAME/../.."
  values="$root/charts/kwerft/values.yaml"
  grep -qE "^    tag: $ZOT_VERSION( |$)" "$values"
  grep -qE "^  clusterIP: $REGISTRY_CLUSTER_IP( |$)" "$values"
  grep -qE "^  buildkitImage: docker.io/moby/buildkit:${BUILDKIT_VERSION}-rootless( |$)" "$values"
  grep -qE "^  railpackImage: ghcr.io/railwayapp/railpack-frontend:${RAILPACK_VERSION}( |$)" "$values"
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
  # The recording rules read it under kube-state-metrics' name for it.
  grep -q 'label_kwerft_dev_app' "$BATS_TEST_DIRNAME/../../charts/kwerft/templates/metrics-rules.yaml"
}
