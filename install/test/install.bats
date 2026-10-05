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
  expected="Preflight System Firewall Kubernetes Registry Helm Network Hetzner Ingress Observability Kwerft Handoff"
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
  PRIVATE_CIDR="10.0.1.3/16"
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
  expected="Preflight System Firewall Kubernetes Registry Helm Network Hetzner Ingress Observability Kwerft"
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
  STORED=""; OWNER=""
  kc() {
    printf 'kc %s\n' "$*" >>"$KC_LOG"
    case "$*" in
      *"get secret kwerft-hcloud-token"*) printf '%s' "$(printf '%s' "$STORED" | base64)" ;;
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
