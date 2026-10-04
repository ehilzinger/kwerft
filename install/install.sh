#!/usr/bin/env bash
# Kwerft installer — turns a fresh Ubuntu server on Hetzner (Cloud or dedicated)
# into a single-node Kubernetes cluster with the Kwerft console on top.
#
#   curl -fsSL https://raw.githubusercontent.com/ehilzinger/kwerft-install/main/install.sh | sudo bash -s -- --domain ops.example.com --email ops@example.com --yes
#
# Released copies come from the public ehilzinger/kwerft-install repository:
# main holds the latest stable release, v<version>/install.sh every release.
#
# The script is idempotent: every stage records completion in $STATE_DIR and is
# skipped on the next run. Re-running repairs a broken install or upgrades it.
#
# Everything lives inside functions and `main` is called on the last line, so a
# truncated download (curl | bash) never executes a partial script.

# Every step appends to the install log on purpose, one redirect per command.
# shellcheck disable=SC2129

set -Eeuo pipefail
shopt -s inherit_errexit 2>/dev/null || true   # bash >= 4.4: fail inside $(...) too

# ---------------------------------------------------------------------------
# Pinned versions. hack/release.sh stamps KWERFT_VERSION_DEFAULT with the
# release version; keep this block self-contained because the script is
# usually piped from curl with no sibling files.
# Latest upstream releases as of 2026-10-04. TODO(phase-0): verify chart values
# against these versions on a real server and add SHA-256 checksums.
# ---------------------------------------------------------------------------
KWERFT_VERSION_DEFAULT="0.1.0-dev"
K3S_VERSION="v1.37.1+k3s1"
HELM_VERSION="v4.3.0"
CILIUM_VERSION="1.20.2"
CERT_MANAGER_VERSION="v1.21.2"
GATEWAY_API_VERSION="v1.6.2"            # only used when k3s does not ship the CRDs
TRAEFIK_CHART_VERSION="41.6.1"
VM_STACK_CHART_VERSION="0.95.0"
VLOGS_CHART_VERSION="0.13.10"
HETZNER_WEBHOOK_CHART_VERSION="0.9.0"   # cert-manager DNS-01 for Hetzner DNS (Cloud API), 2026-08-19
# Builds from Git, latest stable as of 2026-10-04. The chart's values.yaml has
# the same defaults (install/test/install.bats checks that they agree).
ZOT_VERSION="v2.1.21"                   # in-cluster registry, ghcr.io/project-zot/zot-minimal
BUILDKIT_VERSION="v0.33.1"              # docker.io/moby/buildkit:<version>-rootless
RAILPACK_VERSION="v0.40.1"              # ghcr.io/railwayapp/railpack-frontend (contains the railpack CLI)
KWERFT_CHART_REPO="oci://ghcr.io/ehilzinger/charts/kwerft"
KWERFT_IMAGE_REPO="ghcr.io/ehilzinger/kwerft"   # the chart's image.repository; checked before installing

# ---------------------------------------------------------------------------
# Exit codes are part of the automation contract — do not renumber.
# ---------------------------------------------------------------------------
readonly EXIT_OK=0 EXIT_USAGE=2 EXIT_PREFLIGHT=10 EXIT_NETWORK=20 EXIT_K8S=30 EXIT_PLATFORM=40 EXIT_KWERFT=50

readonly STATE_DIR="/var/lib/kwerft"
VALUES_DIR="$STATE_DIR/values"            # Helm values the installer writes; not readonly so tests can point it elsewhere
readonly CONF_DIR="/etc/kwerft"
SETUP_TOKEN_FILE="$CONF_DIR/setup-token"  # not readonly so tests can point it elsewhere
readonly LOG_DIR="/var/log/kwerft"
LOG_FILE="$LOG_DIR/install.log"           # not readonly so tests can point it elsewhere
readonly KUBECONFIG_PATH="/etc/rancher/k3s/k3s.yaml"
readonly WG_PORT=51871
readonly POD_CIDR="10.42.0.0/16"
readonly SERVICE_CIDR="10.43.0.0/16"
readonly TEMP_DOMAIN_SUFFIX=".sslip.io"   # wildcard DNS: <ip>.sslip.io resolves to <ip>
readonly REGISTRY_HOST="registry.kwerft.internal:5000"   # internal/builds.RegistryHost
readonly REGISTRY_CLUSTER_IP="10.43.0.50"  # zot's fixed ClusterIP (chart: registry.clusterIP), inside SERVICE_CIDR
readonly REGISTRY_MARKER="# Managed by Kwerft installer (registry mirror)."
REGISTRIES_FILE="/etc/rancher/k3s/registries.yaml"   # not readonly so tests can point it elsewhere
BUILD_APPARMOR_FILE="/etc/apparmor.d/kwerft-buildkit"  # likewise
DOMAIN_FILE="$STATE_DIR/domain"           # not readonly so tests can point it elsewhere
DNS_TOKEN_SUM_FILE="$STATE_DIR/dns-token.sha256"  # likewise; tells a changed DNS token from the same one

# Options (flags override KWERFT_* environment variables).
DOMAIN="${KWERFT_DOMAIN:-}"
ACME_EMAIL="${KWERFT_EMAIL:-}"
CONFIG_FILE="${KWERFT_CONFIG:-}"
PLATFORM="${KWERFT_PLATFORM:-auto}"
PRIVATE_IFACE="${KWERFT_PRIVATE_IFACE:-}"
KWERFT_VERSION="${KWERFT_VERSION:-$KWERFT_VERSION_DEFAULT}"
CHANNEL="${KWERFT_CHANNEL:-stable}"
JOIN_URL="${KWERFT_JOIN_URL:-}"
JOIN_TOKEN="${KWERFT_JOIN_TOKEN:-}"
JOIN_ROLE="${KWERFT_JOIN_ROLE:-worker}"
KWERFT_CHART="${KWERFT_CHART:-}"
IMAGE="${KWERFT_IMAGE:-}"
IMAGE_ARCHIVE="${KWERFT_IMAGE_ARCHIVE:-}"
DOMAIN_EXPLICIT=0       # 1: the console hostname came from --domain, KWERFT_DOMAIN or --config
APPS_DOMAIN=""          # --config appsDomain
DNS_SOLVER=""           # --config dns.solver (hetzner)
DNS_TOKEN_FILE=""       # --config dns.tokenFile
DNS_RECORDS="true"      # --config dns.records: Kwerft keeps the console's and *.<appsDomain>'s records
MODE="install"
DRY_RUN=0
ASSUME_YES=0
HARDEN_SSH=0
LITE=0

# Discovered facts.
PUBLIC_IP=""
PRIVATE_IP=""
PRIVATE_CIDR=""
CURRENT_STAGE="startup"
START_TS=$(date +%s)

# ---------------------------------------------------------------------------
# Output
# ---------------------------------------------------------------------------
if [[ -t 1 ]]; then
  C_DIM=$'\033[2m' C_OK=$'\033[32m' C_WARN=$'\033[33m' C_ERR=$'\033[31m' C_ACC=$'\033[34m' C_B=$'\033[1m' C_0=$'\033[0m'
else
  C_DIM="" C_OK="" C_WARN="" C_ERR="" C_ACC="" C_B="" C_0=""
fi

log()  { [[ -w "$LOG_FILE" ]] && printf '%s %s\n' "$(date -u +%FT%TZ)" "$*" >>"$LOG_FILE"; return 0; }
say()  { printf '%s\n' "$*"; log "$*"; }
ok()   { printf '%s✓%s %-15s %s\n' "$C_OK" "$C_0" "$1" "${2:-}"; log "OK $1 $2"; }
skip() { printf '%s·%s %-15s %s%s%s\n' "$C_DIM" "$C_0" "$1" "$C_DIM" "${2:-already done}" "$C_0"; log "SKIP $1"; }
warn() { printf '%s!%s %s\n' "$C_WARN" "$C_0" "$*" >&2; log "WARN $*"; }
die()  { local code=$1; shift; printf '%s✗ %s%s\n' "$C_ERR" "$*" "$C_0" >&2; log "FAIL($code) $*"; exit "$code"; }

on_error() {
  local rc=$1 line=$2
  (( BASH_SUBSHELL == 0 )) || return 0   # report once, from the top-level shell
  printf '\n%s✗ Stage "%s" failed (exit %s, line %s).%s\n' "$C_ERR" "$CURRENT_STAGE" "$rc" "$line" "$C_0" >&2
  printf '  Full log: %s · re-run the same command to resume.\n' "$LOG_FILE" >&2
  log "ERROR stage=$CURRENT_STAGE rc=$rc line=$line"
}

usage() {
  cat <<EOF
Kwerft installer ${KWERFT_VERSION_DEFAULT}

Usage: install.sh [options]

Install:
  --domain HOST          Console hostname. Without it, a temporary <public-ip>.sslip.io
                         name is used so you can try Kwerft before setting up DNS
  --email ADDR           Let's Encrypt account contact (optional)
  --config FILE          Pre-seed owner, DNS, Hetzner tokens; skips the setup wizard
  --platform P           auto | cloud | dedicated (default: auto)
  --private-iface IF     Interface for node-to-node and API traffic
  --version V            Kwerft release to install (default: ${KWERFT_VERSION_DEFAULT})
  --channel C            stable | edge
  --lite                 Smaller footprint for 4 GB servers (no Hubble, short retention)
  --harden-ssh           Disable SSH password login and root password login

Join an existing cluster:
  --join URL --token T   Join the cluster whose console runs at URL
  --role R               worker | control-plane (default: worker)

Development:
  --image REF            Run this console image (repository:tag) instead of the release
  --image-archive FILE   Import FILE (docker/OCI tarball) into k3s first; needs --image.
                         hack/dev-server.sh uses both to test unreleased builds

Maintenance:
  --reset-firewall       Remove Kwerft's host firewall rules (rescue)
  --uninstall            Remove Kwerft and k3s from this server

General:
  --dry-run              Print the plan without changing anything
  --yes, -y              Never prompt
  --help, -h             Show this help

Every option can also be set as KWERFT_<NAME> in the environment, e.g. KWERFT_DOMAIN.
Exit codes: 0 ok · 2 usage · 10 preflight · 20 network/DNS · 30 Kubernetes · 40 platform · 50 Kwerft
EOF
}

# ---------------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------------
need_arg() { [[ $# -ge 2 && -n "$2" && "$2" != --* ]] || die $EXIT_USAGE "$1 needs a value"; }

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --domain)         need_arg "$@"; DOMAIN=$2; shift 2 ;;
      --email)          need_arg "$@"; ACME_EMAIL=$2; shift 2 ;;
      --config)         need_arg "$@"; CONFIG_FILE=$2; shift 2 ;;
      --platform)       need_arg "$@"; PLATFORM=$2; shift 2 ;;
      --private-iface)  need_arg "$@"; PRIVATE_IFACE=$2; shift 2 ;;
      --version)        need_arg "$@"; KWERFT_VERSION=$2; shift 2 ;;
      --channel)        need_arg "$@"; CHANNEL=$2; shift 2 ;;
      --join)           need_arg "$@"; JOIN_URL=$2; MODE="join"; shift 2 ;;
      --token)          need_arg "$@"; JOIN_TOKEN=$2; shift 2 ;;
      --role)           need_arg "$@"; JOIN_ROLE=$2; shift 2 ;;
      --image)          need_arg "$@"; IMAGE=$2; shift 2 ;;
      --image-archive)  need_arg "$@"; IMAGE_ARCHIVE=$2; shift 2 ;;
      --lite)           LITE=1; shift ;;
      --harden-ssh)     HARDEN_SSH=1; shift ;;
      --reset-firewall) MODE="reset-firewall"; shift ;;
      --uninstall)      MODE="uninstall"; shift ;;
      --dry-run)        DRY_RUN=1; shift ;;
      --yes|-y)         ASSUME_YES=1; shift ;;
      --help|-h)        usage; exit $EXIT_OK ;;
      *)                usage >&2; die $EXIT_USAGE "Unknown option: $1" ;;
    esac
  done

  [[ -n "$JOIN_URL" ]] && MODE="join"
  # Releases are tagged v0.2.0; chart versions and image tags drop the "v".
  KWERFT_VERSION=${KWERFT_VERSION#v}
  [[ "$KWERFT_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] \
    || die $EXIT_USAGE "--version must look like 0.2.0, v0.2.0 or v0.2.0-rc.1"
  case "$PLATFORM" in auto|cloud|dedicated) ;; *) die $EXIT_USAGE "--platform must be auto, cloud or dedicated" ;; esac
  case "$CHANNEL" in stable|edge) ;; *) die $EXIT_USAGE "--channel must be stable or edge" ;; esac
  case "$JOIN_ROLE" in worker|control-plane) ;; *) die $EXIT_USAGE "--role must be worker or control-plane" ;; esac
  if [[ "$MODE" == "join" && -z "$JOIN_TOKEN" ]]; then die $EXIT_USAGE "--join needs --token"; fi
  if [[ -n "$CONFIG_FILE" && ! -r "$CONFIG_FILE" ]]; then die $EXIT_USAGE "Config file not readable: $CONFIG_FILE"; fi
  if [[ -n "$IMAGE" && "${IMAGE##*/}" != *:* ]]; then die $EXIT_USAGE "--image needs a tag, e.g. ghcr.io/ehilzinger/kwerft:dev-abc123"; fi
  if [[ -n "$IMAGE_ARCHIVE" && -z "$IMAGE" ]]; then die $EXIT_USAGE "--image-archive needs --image to say which image it contains"; fi
  if [[ -n "$IMAGE_ARCHIVE" && ! -r "$IMAGE_ARCHIVE" ]]; then die $EXIT_USAGE "Image archive not readable: $IMAGE_ARCHIVE"; fi

  # A config file may provide domain and email; flags still win.
  if [[ -n "$CONFIG_FILE" ]]; then
    [[ -z "$DOMAIN" ]] && DOMAIN=$(config_get domain)
    [[ -z "$ACME_EMAIL" ]] && ACME_EMAIL=$(config_get email)
    APPS_DOMAIN=$(lower "$(config_get appsDomain)")
    DNS_SOLVER=$(config_get_in dns solver)
    DNS_TOKEN_FILE=$(config_get_in dns tokenFile)
    DNS_RECORDS=$(lower "$(config_get_in dns records)")
    DNS_RECORDS=${DNS_RECORDS:-true}
  fi
  # A hostname given now is explicit: it replaces what Settings chose. Without
  # one, resolve_domain keeps the cluster's setting.
  if [[ -n "$DOMAIN" ]]; then
    DOMAIN_EXPLICIT=1
    DOMAIN=$(lower "${DOMAIN%.}")
    valid_hostname "$DOMAIN" || die $EXIT_USAGE "--domain must be a hostname like ops.example.com, got '$DOMAIN'"
  fi
  if [[ -n "$APPS_DOMAIN" ]] && ! valid_hostname "$APPS_DOMAIN"; then
    die $EXIT_USAGE "appsDomain in $CONFIG_FILE must be a domain like apps.example.com, got '$APPS_DOMAIN'"
  fi
  case "$DNS_SOLVER" in
    ""|hetzner) ;;
    *) die $EXIT_USAGE "dns.solver in $CONFIG_FILE must be hetzner (Hetzner DNS through the Cloud API), got '$DNS_SOLVER'" ;;
  esac
  if [[ -n "$DNS_SOLVER" ]]; then
    [[ -n "$APPS_DOMAIN" ]] || die $EXIT_USAGE "dns in $CONFIG_FILE needs appsDomain: the DNS-01 certificate is the wildcard *.<appsDomain>"
    [[ -n "$DNS_TOKEN_FILE" && -r "$DNS_TOKEN_FILE" ]] || die $EXIT_USAGE "dns.tokenFile not readable: '$DNS_TOKEN_FILE'"
  fi
  case "$DNS_RECORDS" in
    true|false) ;;
    *) die $EXIT_USAGE "dns.records in $CONFIG_FILE must be true or false, got '$DNS_RECORDS'" ;;
  esac
  return 0
}

# Reads a top-level scalar `key: value` from the config file. The full file is
# handed to Kwerft as a Secret; the installer only needs a few top-level keys.
config_get() {
  sed -n -E "s/^$1:[[:space:]]*['\"]?([^'\"#]*)['\"]?[[:space:]]*(#.*)?$/\1/p" "$CONFIG_FILE" | head -n1 | sed -E 's/[[:space:]]+$//'
}

# Reads `key` of the top-level mapping `parent`, written either inline
# (dns: { solver: hetzner, tokenFile: /root/dns.token }) or as a block:
#   dns:
#     solver: hetzner
config_get_in() {
  awk -v parent="$1" -v key="$2" -v q="'" '
    function clean(v) {
      sub(/[[:space:]]+#.*$/, "", v)
      gsub("^[[:space:]\"" q "]+|[[:space:]\"" q "]+$", "", v)
      return v
    }
    $0 ~ "^" parent ":[[:space:]]*[{]" {
      line = $0
      sub("^" parent ":[[:space:]]*[{]", "", line)
      sub(/[}][[:space:]]*(#.*)?$/, "", line)
      n = split(line, parts, ",")
      for (i = 1; i <= n; i++) {
        if (match(parts[i], "^[[:space:]]*" key "[[:space:]]*:")) { print clean(substr(parts[i], RLENGTH + 1)); exit }
      }
      exit
    }
    $0 ~ "^" parent ":[[:space:]]*(#.*)?$" { inside = 1; next }
    inside && /^[^[:space:]#]/ { exit }
    inside && $0 ~ "^[[:space:]]+" key "[[:space:]]*:" {
      v = $0
      sub("^[[:space:]]+" key "[[:space:]]*:", "", v)
      print clean(v)
      exit
    }
  ' "$CONFIG_FILE"
}

lower() { printf '%s' "$1" | tr '[:upper:]' '[:lower:]'; }

valid_hostname() {
  [[ ${#1} -le 253 && "$1" =~ ^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]([-a-z0-9]*[a-z0-9])?$ ]]
}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
kc()   { k3s kubectl --kubeconfig "$KUBECONFIG_PATH" "$@"; }
helmk() { KUBECONFIG="$KUBECONFIG_PATH" helm "$@"; }

# retry <attempts> <sleep-seconds> <command...>
retry() {
  local n=$1 s=$2 i=1; shift 2
  until "$@"; do
    (( i >= n )) && return 1
    i=$((i + 1)); sleep "$s"
  done
}

stage_done() { [[ -f "$STATE_DIR/stages/$1.done" ]]; }
mark_done()  { mkdir -p "$STATE_DIR/stages"; date -u +%FT%TZ >"$STATE_DIR/stages/$1.done"; }

# run_stage <id> <label> <function> [force]
# Stages marked done are skipped unless "force" is given (used for stages that
# must always converge, such as the Helm releases on upgrade).
run_stage() {
  local id=$1 label=$2 fn=$3 force=${4:-}
  CURRENT_STAGE=$label
  if (( DRY_RUN )); then
    printf '%s→%s %-15s %s(would run %s)%s\n' "$C_ACC" "$C_0" "$label" "$C_DIM" "$fn" "$C_0"
    return 0
  fi
  if [[ -z "$force" ]] && stage_done "$id"; then skip "$label"; return 0; fi
  local detail
  detail=$("$fn")              # a failure here exits via errexit with the stage's code
  mark_done "$id"
  ok "$label" "${detail##*$'\n'}"
}

confirm() {
  (( ASSUME_YES )) && return 0
  [[ -r /dev/tty ]] || die $EXIT_USAGE "$1 — pass --yes to confirm non-interactively"
  local answer
  read -r -p "$1 [y/N] " answer </dev/tty
  [[ "$answer" == "y" || "$answer" == "Y" ]]
}

# ---------------------------------------------------------------------------
# Discovery
# ---------------------------------------------------------------------------
detect_platform() {
  [[ "$PLATFORM" != "auto" ]] && return 0
  if curl -fsS --max-time 2 http://169.254.169.254/hetzner/v1/metadata/instance-id >/dev/null 2>&1; then
    PLATFORM="cloud"
  else
    PLATFORM="dedicated"
  fi
}

detect_addresses() {
  command -v ip >/dev/null || return 0   # not Linux (e.g. --dry-run on a laptop)
  PUBLIC_IP=$(ip -4 route get 1.1.1.1 2>/dev/null | sed -n 's/.* src \([0-9.]*\).*/\1/p' | head -n1 || true)
  local default_if
  default_if=$(ip -4 route show default 2>/dev/null | awk '{print $5; exit}' || true)

  if [[ -n "$PRIVATE_IFACE" ]]; then
    PRIVATE_CIDR=$(ip -4 -o addr show dev "$PRIVATE_IFACE" 2>/dev/null | awk '{print $4; exit}' || true)
    [[ -n "$PRIVATE_CIDR" ]] || die $EXIT_PREFLIGHT "Interface $PRIVATE_IFACE has no IPv4 address"
  else
    # First RFC 1918 address that is not on the default-route interface:
    # a Hetzner Cloud Network (cloud) or a vSwitch VLAN interface (dedicated).
    PRIVATE_CIDR=$(ip -4 -o addr show scope global 2>/dev/null \
      | awk -v d="$default_if" '$2 != d {print $2, $4}' \
      | awk '$2 ~ /^(10\.|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.)/ {print $2; exit}' || true)
    PRIVATE_IFACE=$(ip -4 -o addr show scope global 2>/dev/null | awk -v c="$PRIVATE_CIDR" '$4 == c {print $2; exit}' || true)
  fi
  PRIVATE_IP=${PRIVATE_CIDR%/*}
}

# Picks the console hostname: --domain (or KWERFT_DOMAIN, or the config file)
# first; then the cluster's console setting, which the Settings page changes,
# so re-running without flags never undoes a change made there; then the file
# a run before ConsoleSettings existed saved; and finally a temporary
# <ip>.sslip.io name for trying Kwerft before DNS exists.
resolve_domain() {
  local current
  current=$(cluster_setting '{.spec.consoleDomain}')
  if (( DOMAIN_EXPLICIT )) && [[ -n "$current" && "$current" != "$DOMAIN" ]]; then
    warn "The console moves from $current (chosen in Settings) to $DOMAIN (--domain)."
  fi
  if [[ -z "$DOMAIN" ]]; then
    DOMAIN=$current
  fi
  if [[ -z "$DOMAIN" && -s "$DOMAIN_FILE" ]]; then
    DOMAIN=$(<"$DOMAIN_FILE")
  fi
  if [[ -z "$DOMAIN" && -n "$PUBLIC_IP" ]]; then
    DOMAIN="${PUBLIC_IP}${TEMP_DOMAIN_SUFFIX}"
  fi
  return 0
}

# cluster_setting prints a field (jsonpath) of the ConsoleSettings "kwerft",
# or nothing before Kubernetes or Kwerft exist.
cluster_setting() {
  kc get consolesettings.kwerft.dev kwerft -o jsonpath="$1" 2>/dev/null || true
}

is_temp_domain() { [[ "$DOMAIN" == *"$TEMP_DOMAIN_SUFFIX" ]]; }

# ---------------------------------------------------------------------------
# Stage: preflight
# ---------------------------------------------------------------------------
stage_preflight() {
  [[ $EUID -eq 0 ]] || die $EXIT_PREFLIGHT "Run as root (sudo)."

  # shellcheck disable=SC1091
  . /etc/os-release 2>/dev/null || die $EXIT_PREFLIGHT "Cannot read /etc/os-release"
  [[ "${ID:-}" == "ubuntu" ]] || die $EXIT_PREFLIGHT "Ubuntu required, found ${PRETTY_NAME:-unknown}"
  case "${VERSION_ID:-}" in 22.04|24.04|26.04) ;; *) die $EXIT_PREFLIGHT "Ubuntu 22.04, 24.04 or 26.04 LTS required, found $VERSION_ID" ;; esac

  local arch; arch=$(uname -m)
  case "$arch" in x86_64|aarch64) ;; *) die $EXIT_PREFLIGHT "Unsupported architecture: $arch" ;; esac

  local mem_mb; mem_mb=$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo)
  (( mem_mb >= 3500 )) || die $EXIT_PREFLIGHT "At least 4 GB RAM required, found ${mem_mb} MB"
  # The platform itself uses about 2.5 GB (measured on an idle 8 GB Cloud server).
  (( mem_mb >= 7000 || LITE )) || warn "Less than 8 GB RAM: the platform uses about 2.5 GB, leaving little for apps and Git builds. Consider --lite or a larger server."

  local disk_gb; disk_gb=$(df -BG --output=avail / | tail -n1 | tr -dc '0-9')
  (( disk_gb >= 30 )) || die $EXIT_PREFLIGHT "At least 30 GB free disk on / required, found ${disk_gb} GB"

  [[ -f /sys/fs/cgroup/cgroup.controllers ]] || die $EXIT_PREFLIGHT "cgroup v2 required"

  local port
  for port in 80 443; do
    if [[ -n "$(ss -Htln "sport = :$port")" ]] && ! stage_done kubernetes; then
      die $EXIT_PREFLIGHT "Port $port is already in use. Stop the service using it (e.g. nginx, apache2) and retry."
    fi
  done

  curl -fsS --max-time 10 -o /dev/null https://get.k3s.io || die $EXIT_NETWORK "No outbound HTTPS to get.k3s.io"
  [[ -n "$PUBLIC_IP" ]] || die $EXIT_NETWORK "Could not determine the public IPv4 address"
  if [[ "$MODE" == "install" ]]; then check_release; fi

  local priv="no private network"
  [[ -n "$PRIVATE_IP" ]] && priv="private $PRIVATE_IP on $PRIVATE_IFACE"
  echo "Ubuntu $VERSION_ID · $arch · $((mem_mb / 1024)) GiB RAM · ${disk_gb} GB free · $PLATFORM · $priv"
}

# ---------------------------------------------------------------------------
# Stage: system preparation
# ---------------------------------------------------------------------------
stage_system() {
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq >>"$LOG_FILE" 2>&1
  apt-get install -y -qq curl ca-certificates jq nftables chrony unattended-upgrades open-iscsi >>"$LOG_FILE" 2>&1

  swapoff -a
  sed -i -E 's@^([^#].*[[:space:]]swap[[:space:]].*)$@# disabled by kwerft: \1@' /etc/fstab

  cat >/etc/modules-load.d/kwerft.conf <<'EOF'
overlay
br_netfilter
wireguard
EOF
  modprobe overlay; modprobe br_netfilter; modprobe wireguard 2>/dev/null || true

  cat >/etc/sysctl.d/90-kwerft.conf <<'EOF'
net.ipv4.ip_forward = 1
net.ipv6.conf.all.forwarding = 1
net.bridge.bridge-nf-call-iptables = 1
fs.inotify.max_user_instances = 8192
fs.inotify.max_user_watches = 1048576
vm.max_map_count = 262144
EOF
  sysctl --system >>"$LOG_FILE" 2>&1

  systemctl enable --now chrony >>"$LOG_FILE" 2>&1
  systemctl enable --now unattended-upgrades >>"$LOG_FILE" 2>&1

  if (( HARDEN_SSH )); then
    cat >/etc/ssh/sshd_config.d/90-kwerft.conf <<'EOF'
PasswordAuthentication no
PermitRootLogin prohibit-password
KbdInteractiveAuthentication no
EOF
    systemctl reload ssh 2>/dev/null || systemctl reload sshd
  fi
  echo "packages · sysctl · chrony · unattended-upgrades · swap off$( (( HARDEN_SSH )) && echo ' · ssh hardened')"
}

# ---------------------------------------------------------------------------
# Stage: host firewall baseline
# Separate nftables table so we never clobber the operator's own rules.
# Cilium host policies take over fine-grained control once Kwerft runs.
# ---------------------------------------------------------------------------
stage_firewall() {
  mkdir -p /etc/nftables.d
  local priv_rule="# no private network detected"
  [[ -n "$PRIVATE_CIDR" ]] && priv_rule="ip saddr $(network_of "$PRIVATE_CIDR") accept"

  cat >/etc/nftables.d/kwerft.nft <<EOF
# Managed by Kwerft — edit rules in the console (Network → Server firewall).
table inet kwerft {
  chain input {
    type filter hook input priority 0; policy drop;
    ct state established,related accept
    iif lo accept
    meta l4proto { icmp, ipv6-icmp } accept
    tcp dport { 22, 80, 443 } accept
    udp dport $WG_PORT accept
    iifname { "cilium_*", "lxc*" } accept
    ip saddr $POD_CIDR accept
    $priv_rule
  }
}
EOF
  if ! grep -q 'include "/etc/nftables.d/\*.nft"' /etc/nftables.conf 2>/dev/null; then
    printf '\ninclude "/etc/nftables.d/*.nft"\n' >>/etc/nftables.conf
  fi
  nft delete table inet kwerft 2>/dev/null || true
  nft -f /etc/nftables.d/kwerft.nft
  systemctl enable nftables >>"$LOG_FILE" 2>&1
  echo "nftables: 22, 80, 443 public$([[ -n "$PRIVATE_CIDR" ]] && echo " · cluster ports on $PRIVATE_IFACE only")"
}

# 10.0.1.3/16 -> 10.0.0.0/16 (nft accepts host bits only with a warning on some versions)
network_of() {
  local ip=${1%/*} bits=${1#*/} a b c d mask
  IFS=. read -r a b c d <<<"$ip"
  mask=$(( 0xFFFFFFFF << (32 - bits) & 0xFFFFFFFF ))
  printf '%d.%d.%d.%d/%d\n' $(( a & (mask >> 24 & 255) )) $(( b & (mask >> 16 & 255) )) $(( c & (mask >> 8 & 255) )) $(( d & (mask & 255) )) "$bits"
}

# ---------------------------------------------------------------------------
# Stage: Kubernetes (k3s server)
# ---------------------------------------------------------------------------
write_k3s_config() {
  local node_ip=${PRIVATE_IP:-$PUBLIC_IP}
  mkdir -p /etc/rancher/k3s
  cat >/etc/rancher/k3s/config.yaml <<EOF
# Managed by Kwerft installer.
cluster-init: true
node-ip: $node_ip
node-external-ip: $PUBLIC_IP
advertise-address: $node_ip
tls-san:
  - $DOMAIN
  - $PUBLIC_IP
cluster-cidr: $POD_CIDR
service-cidr: $SERVICE_CIDR
flannel-backend: none
disable-network-policy: true
disable-kube-proxy: true
disable:
  - traefik
  - servicelb
secrets-encryption: true
write-kubeconfig-mode: "0600"
node-label:
  - kwerft.dev/platform=$PLATFORM
kubelet-arg:
  - max-pods=200
EOF
}

stage_kubernetes() {
  write_k3s_config
  write_registry_mirror >/dev/null   # before k3s first starts: no restart needed
  curl -fsSL https://get.k3s.io \
    | INSTALL_K3S_VERSION="$K3S_VERSION" INSTALL_K3S_SKIP_ENABLE=false sh -s - server >>"$LOG_FILE" 2>&1 \
    || die $EXIT_K8S "k3s installation failed"
  retry 60 2 kc get --raw /readyz >/dev/null 2>&1 || die $EXIT_K8S "Kubernetes API did not become ready"
  echo "k3s $K3S_VERSION · server · embedded etcd · secrets encryption on"
}

# ---------------------------------------------------------------------------
# Stage: registry mirror
# Images built from Git are named registry.kwerft.internal:5000/<project>/<app>
# and live in the in-cluster registry (zot, Service kwerft-registry with a
# fixed ClusterIP). containerd runs on the host and cannot resolve cluster DNS,
# so k3s's registries.yaml maps the name to that ClusterIP, which Cilium's
# kube-proxy replacement serves to host processes as well (socket load
# balancing), on this node and every joined one. Plain HTTP: the traffic
# stays inside the cluster (WireGuard between nodes) and zot admits only the
# nodes, build pods and the console. Deliberately no NodePort: Cilium answers
# NodePorts in eBPF before the nftables host firewall sees the packet, which
# would publish an unauthenticated registry on the public address.
# ---------------------------------------------------------------------------
registries_yaml() {
  cat <<EOF
$REGISTRY_MARKER
# Images built from Git ($REGISTRY_HOST) come from the in-cluster registry
# through its fixed ClusterIP. k3s reads this file only when it starts.
mirrors:
  "$REGISTRY_HOST":
    endpoint:
      - "http://$REGISTRY_CLUSTER_IP:5000"
EOF
}

# write_registry_mirror brings registries.yaml up to date and prints what it
# found: "changed" (written), "unchanged", "operator" (a file of the
# operator's own that already maps the registry) or "foreign" (a file of the
# operator's own without it). Files Kwerft did not write are never changed.
write_registry_mirror() {
  local want
  want=$(registries_yaml)
  if [[ -f "$REGISTRIES_FILE" ]]; then
    if [[ "$(<"$REGISTRIES_FILE")" == "$want" ]]; then echo unchanged; return 0; fi
    if [[ "$(head -n1 "$REGISTRIES_FILE")" != "$REGISTRY_MARKER" ]]; then
      if grep -qF "$REGISTRY_HOST" "$REGISTRIES_FILE"; then echo operator; else echo foreign; fi
      return 0
    fi
  fi
  mkdir -p "$(dirname "$REGISTRIES_FILE")"
  (umask 077; printf '%s\n' "$want" >"$REGISTRIES_FILE.kwerft-new")
  mv -f "$REGISTRIES_FILE.kwerft-new" "$REGISTRIES_FILE"
  echo changed
}

# restart_k3s applies a changed registries.yaml. Restarting k3s leaves running
# containers alone (the unit's KillMode=process); the API is back in seconds.
# Prints the unit it restarted, nothing when k3s is not running yet.
restart_k3s() {
  local unit
  for unit in k3s k3s-agent; do
    systemctl is-active --quiet "$unit" || continue
    systemctl restart "$unit" >>"$LOG_FILE" 2>&1 || die $EXIT_K8S "Could not restart $unit to apply $REGISTRIES_FILE"
    if [[ "$unit" == "k3s" ]]; then
      retry 60 2 kc get --raw /readyz >/dev/null 2>&1 || die $EXIT_K8S "Kubernetes API did not come back after restarting k3s"
    fi
    echo "$unit"
    return 0
  done
}

stage_registry_mirror() {
  local state restarted=""
  state=$(write_registry_mirror)
  case "$state" in
    changed) restarted=$(restart_k3s) ;;
    operator) echo "$REGISTRY_HOST as configured in $REGISTRIES_FILE (not managed by Kwerft)"; return 0 ;;
    foreign)
      warn "$REGISTRIES_FILE was not written by Kwerft, so it is left alone. Add this mirror to it and restart k3s, or images built from Git cannot be pulled:"
      registries_yaml | sed -n '/^mirrors:/,$p' >&2
      echo "$REGISTRY_HOST not configured (see warning)"
      return 0 ;;
  esac
  local apparmor
  apparmor=$(ensure_build_apparmor)
  echo "$REGISTRY_HOST → zot at $REGISTRY_CLUSTER_IP:5000${restarted:+ · $restarted restarted}${apparmor:+ · $apparmor}"
}

# Rootless BuildKit creates user namespaces. Ubuntu (24.04 on) lets only
# processes with an AppArmor profile that allows it do that
# (kernel.apparmor_restrict_unprivileged_userns), and an unconfined container
# has none. Rather than turning the restriction off for the whole host, this
# profile allows user namespaces and nothing else beyond "unconfined", like
# Ubuntu's own profile for rootlesskit; only build containers use it
# (Kubernetes appArmorProfile Localhost "kwerft-buildkit"). Prints a summary
# word, or nothing where AppArmor is off.
build_apparmor_profile() {
  cat <<'PROFILE'
# Written by the Kwerft installer: rootless BuildKit in build containers.
abi <abi/4.0>,
include <tunables/global>

profile kwerft-buildkit flags=(unconfined) {
  userns,

  include if exists <local/kwerft-buildkit>
}
PROFILE
}

ensure_build_apparmor() {
  command -v apparmor_parser >/dev/null 2>&1 || return 0
  [[ -d "$(dirname "$BUILD_APPARMOR_FILE")" ]] || return 0
  local want
  want=$(build_apparmor_profile)
  if [[ "$(cat "$BUILD_APPARMOR_FILE" 2>/dev/null)" != "$want" ]]; then
    printf '%s\n' "$want" >"$BUILD_APPARMOR_FILE"
  fi
  # Loading is cheap and idempotent; after a reboot the profile is loaded by
  # apparmor.service from the file.
  apparmor_parser -r -W "$BUILD_APPARMOR_FILE" >>"$LOG_FILE" 2>&1 \
    || die $EXIT_PLATFORM "Could not load the AppArmor profile $BUILD_APPARMOR_FILE (see $LOG_FILE)"
  echo "AppArmor profile kwerft-buildkit"
}

# Uninstall: k3s's own uninstall removes /etc/rancher/k3s; this covers a
# server where k3s is already gone. Only Kwerft's own file is removed.
remove_registry_mirror() {
  if [[ -f "$BUILD_APPARMOR_FILE" ]]; then
    apparmor_parser -R "$BUILD_APPARMOR_FILE" >/dev/null 2>&1 || true
    rm -f "$BUILD_APPARMOR_FILE"
  fi
  [[ -f "$REGISTRIES_FILE" && "$(head -n1 "$REGISTRIES_FILE")" == "$REGISTRY_MARKER" ]] || return 0
  rm -f "$REGISTRIES_FILE"
}

stage_helm() {
  if command -v helm >/dev/null && helm version --short 2>/dev/null | grep -q "${HELM_VERSION}"; then
    echo "helm $HELM_VERSION (present)"; return 0
  fi
  local arch tmp
  case "$(uname -m)" in x86_64) arch=amd64 ;; aarch64) arch=arm64 ;; esac
  tmp=$(mktemp -d)
  curl -fsSL -o "$tmp/helm.tgz" "https://get.helm.sh/helm-${HELM_VERSION}-linux-${arch}.tar.gz"
  curl -fsSL -o "$tmp/helm.tgz.sha256" "https://get.helm.sh/helm-${HELM_VERSION}-linux-${arch}.tar.gz.sha256sum"
  (cd "$tmp" && echo "$(awk '{print $1}' helm.tgz.sha256)  helm.tgz" | sha256sum -c - >/dev/null) \
    || die $EXIT_PLATFORM "Helm checksum mismatch"
  tar -xzf "$tmp/helm.tgz" -C "$tmp"
  install -m 0755 "$tmp/linux-${arch}/helm" /usr/local/bin/helm
  rm -rf "$tmp"
  echo "helm $HELM_VERSION"
}

# ---------------------------------------------------------------------------
# Stage: networking (Cilium)
# ---------------------------------------------------------------------------
stage_network() {
  local api_ip=${PRIVATE_IP:-$PUBLIC_IP} hubble=true
  (( LITE )) && hubble=false
  helmk repo add cilium https://helm.cilium.io --force-update >>"$LOG_FILE" 2>&1
  helmk upgrade --install cilium cilium/cilium --version "$CILIUM_VERSION" \
    --namespace kube-system --wait --timeout 10m \
    --set kubeProxyReplacement=true \
    --set k8sServiceHost="$api_ip" --set k8sServicePort=6443 \
    --set ipam.mode=kubernetes \
    --set operator.replicas=1 \
    --set encryption.enabled=true --set encryption.type=wireguard \
    --set encryption.wireguard.persistentKeepalive=25s \
    --set hubble.enabled="$hubble" --set hubble.relay.enabled="$hubble" \
    --set bpf.masquerade=true \
    >>"$LOG_FILE" 2>&1 || die $EXIT_PLATFORM "Cilium installation failed"
  retry 90 2 kc wait --for=condition=Ready nodes --all --timeout=5s >/dev/null 2>&1 \
    || die $EXIT_K8S "Node did not become Ready after installing Cilium"
  echo "Cilium $CILIUM_VERSION · kube-proxy replacement · WireGuard$([[ $hubble == true ]] && echo ' · Hubble')"
}

# Pods in namespaces labelled kwerft.dev/system may reach every app (each App's
# NetworkPolicy allows them): ingress, monitoring and the console itself.
mark_system_namespace() {
  kc label namespace "$1" kwerft.dev/system=true --overwrite >/dev/null
}

# ---------------------------------------------------------------------------
# Stage: platform services
# ---------------------------------------------------------------------------
# k3s >= 1.37 ships the Gateway API CRDs as its own packaged component
# (gateway-api-crd) and upgrades them together with Kubernetes. Taking them
# over would make k3s and Kwerft fight, so use k3s's copy when it is there and
# install the pinned release only on k3s versions without it.
gateway_api_from_k3s() {
  [[ "$(kc get crd gateways.gateway.networking.k8s.io \
    -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null)" == "gateway-api-crd" ]]
}

ensure_gateway_api() {
  # k3s deploys its packaged components shortly after the API is up.
  retry 15 2 gateway_api_from_k3s || true
  if gateway_api_from_k3s; then
    kc get crd gateways.gateway.networking.k8s.io \
      -o jsonpath='{.metadata.annotations.gateway\.networking\.k8s\.io/bundle-version}'
    echo " (k3s)"
    return 0
  fi
  kc apply --server-side -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml" \
    >>"$LOG_FILE" 2>&1 || die $EXIT_PLATFORM "Gateway API CRDs failed to install"
  echo "$GATEWAY_API_VERSION"
}

stage_ingress_tls() {
  local gateway_api
  gateway_api=$(ensure_gateway_api)

  helmk repo add jetstack https://charts.jetstack.io --force-update >>"$LOG_FILE" 2>&1
  helmk upgrade --install cert-manager jetstack/cert-manager --version "$CERT_MANAGER_VERSION" \
    --namespace cert-manager --create-namespace --wait --timeout 10m \
    --set crds.enabled=true \
    --set config.apiVersion=controller.config.cert-manager.io/v1alpha1 \
    --set config.kind=ControllerConfiguration \
    --set config.enableGatewayAPI=true \
    >>"$LOG_FILE" 2>&1 || die $EXIT_PLATFORM "cert-manager installation failed"
  install_dns_webhook

  mkdir -p "$VALUES_DIR"
  traefik_values >"$VALUES_DIR/traefik.yaml"
  helmk repo add traefik https://traefik.github.io/charts --force-update >>"$LOG_FILE" 2>&1
  helmk upgrade --install traefik traefik/traefik --version "$TRAEFIK_CHART_VERSION" \
    --namespace traefik --create-namespace --wait --timeout 10m \
    -f "$VALUES_DIR/traefik.yaml" \
    >>"$LOG_FILE" 2>&1 || die $EXIT_PLATFORM "Traefik installation failed"
  mark_system_namespace traefik
  echo "Traefik · cert-manager · Hetzner DNS-01 · Gateway API $gateway_api"
}

# Traefik binds 80/443 directly on every node (hostNetwork DaemonSet). This
# works identically on cloud and dedicated servers; a Hetzner Load Balancer
# can target the nodes later without changing anything here.
#
# Prometheus metrics on :9101 (the host network; the firewall keeps it
# inside the cluster) are scraped by vmagent through the Kwerft chart's
# VMPodScrape. Router labels are what the console's request, error and
# latency charts are built from (recording rules in the chart's
# metrics-rules.yaml); per-service series would only duplicate them.
traefik_values() {
  cat <<'EOF'
deployment:
  kind: DaemonSet
hostNetwork: true
updateStrategy:
  type: RollingUpdate
  rollingUpdate:
    maxUnavailable: 1
    maxSurge: 0
service:
  enabled: false
ports:
  web:
    port: 80
  websecure:
    port: 443
  metrics:
    port: 9101          # node-exporter owns 9100 on the host network
  traefik:
    port: 9000
    expose:
      default: false
gateway:
  enabled: false        # Kwerft owns the Gateway resource
providers:
  kubernetesGateway:
    enabled: true
  kubernetesIngress:
    enabled: true
securityContext:
  capabilities:
    drop: [ALL]
    add: [NET_BIND_SERVICE]
  readOnlyRootFilesystem: true
  runAsNonRoot: false
  runAsUser: 0
  runAsGroup: 0
metrics:
  prometheus:
    entryPoint: metrics
    addEntryPointsLabels: true
    addRoutersLabels: true
    addServicesLabels: false
    buckets: "0.01,0.025,0.05,0.1,0.25,0.5,1,2.5,5,10"
EOF
}

# Hetzner's cert-manager webhook solves DNS-01 through Hetzner DNS (Cloud API),
# for the apps wildcard certificate (Settings or --config appsDomain + dns).
# Installed always, so Settings can turn DNS-01 on without the installer.
#
# Its chart lets the webhook read every Secret in the cluster. It only reads
# the token (one Get per challenge), which Kwerft keeps in kwerft-system next
# to its namespaced DNS-01 Issuer, so the cluster-wide binding is replaced by
# "get" on that one Secret. (A Helm upgrade recreates the cluster-wide
# binding; every run removes it again.)
install_dns_webhook() {
  local release=cert-manager-webhook-hetzner
  helmk repo add hcloud https://charts.hetzner.cloud --force-update >>"$LOG_FILE" 2>&1
  helmk upgrade --install "$release" hcloud/cert-manager-webhook-hetzner --version "$HETZNER_WEBHOOK_CHART_VERSION" \
    --namespace cert-manager --wait --timeout 10m \
    >>"$LOG_FILE" 2>&1 || die $EXIT_PLATFORM "Hetzner DNS webhook for cert-manager failed to install"
  kc create namespace kwerft-system --dry-run=client -o yaml | kc apply -f - >/dev/null
  kc -n kwerft-system create role "$release:dns-token" --verb=get --resource=secrets --resource-name=kwerft-dns-token \
    --dry-run=client -o yaml | kc apply -f - >>"$LOG_FILE" 2>&1
  kc -n kwerft-system create rolebinding "$release:dns-token" --role="$release:dns-token" --serviceaccount="cert-manager:$release" \
    --dry-run=client -o yaml | kc apply -f - >>"$LOG_FILE" 2>&1
  kc delete clusterrolebinding "$release:read-secrets" --ignore-not-found >>"$LOG_FILE" 2>&1
}

# vector_remap prints the VRL program Vector runs on every container log line
# before it goes to VictoriaLogs. It flattens what Kwerft filters on into
# top-level fields (internal/logs relies on these names):
#
#   namespace pod container node stream    from the kubernetes_logs source
#   project app build task schedule         pod labels kwerft.dev/<name>
#   job                                     the owning Job (pod_owner Job/<name>)
#   level                                   level|lvl|severity of a JSON line, lower case
#   log.*                                   the fields of a JSON line
#   message                                 the raw line (VictoriaLogs' _msg)
#
# The rest of the Kubernetes metadata (all labels, annotations, uid, image)
# is dropped to keep storage and Vector's memory small. A JSON line's own
# fields stay under log.*, so a line can never set namespace or app itself.
vector_remap() {
  cat <<'EOF'
k = object(.kubernetes) ?? {}
labels = object(k.pod_labels) ?? {}
.namespace = k.pod_namespace
.pod = k.pod_name
.container = k.container_name
.node = k.pod_node_name
.project = labels."kwerft.dev/project"
.app = labels."kwerft.dev/app"
.build = labels."kwerft.dev/build"
.task = labels."kwerft.dev/task"
.schedule = labels."kwerft.dev/schedule"
owner = string(k.pod_owner) ?? ""
if starts_with(owner, "Job/") {
  .job = slice!(owner, 4)
}
parsed = parse_json(string(.message) ?? "") ?? null
if is_object(parsed) {
  .log = parsed
  lvl = parsed.level || parsed.lvl || parsed.severity
  if is_string(lvl) { .level = downcase(string!(lvl)) }
}
del(.kubernetes)
del(.file)
del(.source_type)
. = compact(., recursive: false)
EOF
}

# write_vlogs_values prints the values for the victoria-logs-single chart:
# Vector's pipeline (vector_remap), the stream fields, small resource
# limits, and a NetworkPolicy so that only Vector (kwerft-observability) and
# the console (kwerft-system) reach VictoriaLogs: it has no authentication,
# and the console confines every query to the user's projects.
#
# Stream fields are constant for a container, so they add no streams beyond
# namespace/pod/container/stream, and the console's namespace confinement
# and app/build/task filters become fast stream filters.
write_vlogs_values() {
  cat <<'EOF'
server:
  resources:
    requests: {cpu: 50m, memory: 128Mi}
    limits: {memory: 1Gi}
networkPolicy:
  enabled: true
  ingress:
    - from:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kwerft-observability
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kwerft-system
      ports:
        - port: 9428
          protocol: TCP
vector:
  enabled: true
  resources:
    requests: {cpu: 20m, memory: 64Mi}
    limits: {memory: 256Mi}
  customConfig:
    data_dir: /vector-data-dir
    api:
      enabled: false
    sources:
      k8s:
        type: kubernetes_logs
        insert_namespace_fields: false
        use_apiserver_cache: true
        pod_annotation_fields:
          pod_annotations: ""
          pod_uid: ""
          pod_ip: ""
          pod_ips: ""
          container_id: ""
          container_image: ""
          container_image_id: ""
        node_annotation_fields:
          node_labels: ""
      internal_metrics:
        type: internal_metrics
    transforms:
      parser:
        type: remap
        inputs: [k8s]
        source: |
EOF
  vector_remap | sed 's/^/          /'
  cat <<'EOF'
    sinks:
      exporter:
        type: prometheus_exporter
        address: 0.0.0.0:9090
        inputs: [internal_metrics]
      vlogs:
        type: elasticsearch
        inputs: [parser]
        mode: bulk
        api_version: v8
        compression: gzip
        healthcheck:
          enabled: false
        request:
          headers:
            VL-Time-Field: timestamp
            VL-Msg-Field: message
            VL-Stream-Fields: namespace,pod,container,stream,project,app,build,task
            AccountID: "0"
            ProjectID: "0"
EOF
}

stage_observability() {
  local retention=30d log_retention=14d
  (( LITE )) && { retention=7d; log_retention=3d; }
  helmk repo add vm https://victoriametrics.github.io/helm-charts/ --force-update >>"$LOG_FILE" 2>&1
  # disableNamespaceMatcher: Kwerft routes alerts to notification channels
  # with one VMAlertmanagerConfig per channel in kwerft-observability; by
  # default the operator confines such a route to alerts whose namespace
  # label is kwerft-observability, which would drop every project and node
  # alert. Timing (crash loop in Slack < 2 min) needs no change here: vmalert
  # evaluates every 20s (Kwerft's crash-loop group every 10s) and Kwerft's
  # routes set their own group_wait (10s).
  mkdir -p "$VALUES_DIR"
  vm_stack_values >"$VALUES_DIR/vm-stack.yaml"
  helmk upgrade --install vm vm/victoria-metrics-k8s-stack --version "$VM_STACK_CHART_VERSION" \
    --namespace kwerft-observability --create-namespace --wait --timeout 15m \
    -f "$VALUES_DIR/vm-stack.yaml" \
    --set grafana.enabled=false \
    --set vmsingle.spec.retentionPeriod="$retention" \
    --set alertmanager.enabled=true \
    --set alertmanager.spec.disableNamespaceMatcher=true \
    >>"$LOG_FILE" 2>&1 || die $EXIT_PLATFORM "VictoriaMetrics installation failed"
  write_vlogs_values >"$VALUES_DIR/vlogs.yaml"
  helmk upgrade --install vlogs vm/victoria-logs-single --version "$VLOGS_CHART_VERSION" \
    --namespace kwerft-observability --wait --timeout 10m \
    -f "$VALUES_DIR/vlogs.yaml" \
    --set server.retentionPeriod="$log_retention" \
    >>"$LOG_FILE" 2>&1 || die $EXIT_PLATFORM "VictoriaLogs installation failed"
  mark_system_namespace kwerft-observability
  echo "VictoriaMetrics ($retention) · VictoriaLogs ($log_retention) · kube-state-metrics · node-exporter"
}

# Values for victoria-metrics-k8s-stack beyond the --set flags above.
# kube-state-metrics exports the pod labels Kwerft puts on every App's pods
# (kube_pod_labels{label_kwerft_dev_app, label_kwerft_dev_project}), which
# the chart's recording rules join container metrics on. Namespaces carry
# the project label too, for alert rules scoped to projects.
vm_stack_values() {
  cat <<'EOF'
kube-state-metrics:
  metricLabelsAllowlist:
    - pods=[kwerft.dev/app,kwerft.dev/project]
    - namespaces=[kwerft.dev/project]
EOF
}

# ---------------------------------------------------------------------------
# Stage: Kwerft
# ---------------------------------------------------------------------------
chart_ref() {
  if [[ -n "$KWERFT_CHART" ]]; then echo "$KWERFT_CHART"; return; fi
  # Piped from curl there is no script file, so no checkout to look in (and
  # the current directory must not count as one).
  local src="${BASH_SOURCE[0]:-}" here
  if [[ -f "$src" ]]; then
    here=$(cd "$(dirname "$src")" && pwd)
    if [[ -f "$here/../charts/kwerft/Chart.yaml" ]]; then
      echo "$here/../charts/kwerft"    # running from a repository checkout
      return
    fi
  fi
  echo "$KWERFT_CHART_REPO"
}

# oci_manifest_status <registry> <repository> <tag> prints the HTTP status of an
# anonymous manifest request: 200 published, 404 no such tag, 401/403 private
# or no such package (registries do not tell those apart), 000 unreachable.
oci_manifest_status() {
  local url="https://$1/v2/$2/manifests/$3" accept headers code challenge realm service token
  accept="application/vnd.oci.image.index.v1+json, application/vnd.oci.image.manifest.v1+json"
  accept+=", application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.docker.distribution.manifest.v2+json"
  headers=$(curl -sS --max-time 20 -I -H "Accept: $accept" "$url" 2>/dev/null | tr -d '\r') || { echo 000; return 0; }
  code=$(awk 'NR == 1 {print $2}' <<<"$headers")
  if [[ "$code" == 401 ]]; then
    # Anonymous pulls still need a token: answer the registry's Bearer challenge.
    challenge=$(sed -n 's/^[Ww][Ww][Ww]-[Aa]uthenticate: *Bearer *//p' <<<"$headers")
    realm=$(sed -n 's/.*realm="\([^"]*\)".*/\1/p' <<<"$challenge")
    service=$(sed -n 's/.*service="\([^"]*\)".*/\1/p' <<<"$challenge")
    [[ -n "$realm" ]] || { echo 401; return 0; }
    token=$(curl -fsS --max-time 20 -G "$realm" --data-urlencode "service=$service" \
      --data-urlencode "scope=repository:$2:pull" 2>/dev/null | sed -n 's/.*"token" *: *"\([^"]*\)".*/\1/p') || true
    [[ -n "$token" ]] || { echo 401; return 0; }
    code=$(curl -sS --max-time 20 -o /dev/null -w '%{http_code}' -I -H "Accept: $accept" \
      -H "Authorization: Bearer $token" "$url" 2>/dev/null) || code=000
  fi
  echo "${code:-000}"
}

# check_release fails before anything is installed when the release cannot be
# pulled: a version that was never published, or packages that are still private.
check_release() {
  local ref; ref=$(chart_ref)
  [[ "$ref" == oci://* ]] || return 0          # a checkout installs its own chart
  local refs=("${ref#oci://}") r status
  [[ -n "$IMAGE" ]] || refs+=("$KWERFT_IMAGE_REPO")
  for r in "${refs[@]}"; do
    status=$(oci_manifest_status "${r%%/*}" "${r#*/}" "$KWERFT_VERSION")
    case "$status" in
      200) ;;
      404) die $EXIT_KWERFT "Kwerft $KWERFT_VERSION is not published: $r has no version $KWERFT_VERSION." \
             "Released versions: https://github.com/ehilzinger/kwerft-install (this installer defaults to $KWERFT_VERSION_DEFAULT)." ;;
      401|403) die $EXIT_KWERFT "Cannot pull $r:$KWERFT_VERSION anonymously: it is not published, or the package" \
             "is still private (maintainers: make it public on GitHub, see RELEASING.md)." ;;
      000) die $EXIT_NETWORK "Cannot reach ${r%%/*} to look up Kwerft $KWERFT_VERSION" ;;
      *)   die $EXIT_KWERFT "Unexpected HTTP $status from ${r%%/*} while looking up $r:$KWERFT_VERSION" ;;
    esac
  done
}

stage_kwerft() {
  kc create namespace kwerft-system --dry-run=client -o yaml | kc apply -f - >/dev/null
  mark_system_namespace kwerft-system
  if [[ -n "$CONFIG_FILE" ]]; then
    kc -n kwerft-system create secret generic kwerft-bootstrap \
      --from-file=config.yaml="$CONFIG_FILE" --dry-run=client -o yaml | kc apply -f - >/dev/null
  fi

  local ref version_args=() image_args=()
  ref=$(chart_ref)
  [[ "$ref" == oci://* ]] && version_args=(--version "$KWERFT_VERSION")
  if [[ -n "$IMAGE" ]]; then
    if [[ -n "$IMAGE_ARCHIVE" ]]; then
      k3s ctr --namespace k8s.io images import "$IMAGE_ARCHIVE" >>"$LOG_FILE" 2>&1 \
        || die $EXIT_KWERFT "Could not import $IMAGE_ARCHIVE into k3s"
    fi
    # IfNotPresent: an imported image is used as-is and never pulled.
    image_args=(--set image.repository="${IMAGE%:*}" --set image.tag="${IMAGE##*:}" --set image.pullPolicy=IfNotPresent)
  else
    image_args=(--set image.tag="$KWERFT_VERSION")
  fi
  # Helm installs a chart's CRDs only on first install, never on upgrade, so
  # apply them on every run (server-side; Helm no longer touches them).
  helmk show crds "$ref" ${version_args[@]+"${version_args[@]}"} 2>>"$LOG_FILE" \
    | kc apply --server-side --force-conflicts -f - >>"$LOG_FILE" 2>&1 \
    || die $EXIT_KWERFT "Kwerft CRDs failed to apply (chart: $ref)"
  apply_console_settings
  adopt_namespace kwerft-builds
  local taken
  taken=$(registry_address_taken)
  [[ -z "$taken" ]] || die $EXIT_KWERFT "The registry's fixed address $REGISTRY_CLUSTER_IP is taken by Service $taken." \
    "Delete or re-create that Service (it gets a new address) and re-run the installer."
  helmk upgrade --install kwerft "$ref" ${version_args[@]+"${version_args[@]}"} \
    --namespace kwerft-system --wait --timeout 10m --skip-crds \
    --set console.domain="$DOMAIN" \
    --set acme.email="$ACME_EMAIL" \
    --set platform="$PLATFORM" \
    "${image_args[@]}" \
    --set registry.image.tag="$ZOT_VERSION" \
    --set registry.clusterIP="$REGISTRY_CLUSTER_IP" \
    --set builds.buildkitImage="docker.io/moby/buildkit:${BUILDKIT_VERSION}-rootless" \
    --set builds.railpackImage="ghcr.io/railwayapp/railpack-frontend:${RAILPACK_VERSION}" \
    >>"$LOG_FILE" 2>&1 || die $EXIT_KWERFT "Kwerft installation failed (chart: $ref)"
  printf '%s\n' "$DOMAIN" >"$DOMAIN_FILE"   # fallback; the cluster setting is the record
  echo "control plane ${IMAGE:-$KWERFT_VERSION} ready · registry zot $ZOT_VERSION at $REGISTRY_CLUSTER_IP:5000"
}

# adopt_namespace hands a namespace the chart creates, but that already exists
# (made by hand or by an earlier development build), over to the Helm
# release; Helm refuses to install over objects it does not own.
adopt_namespace() {
  kc get namespace "$1" >/dev/null 2>&1 || return 0
  kc annotate namespace "$1" --overwrite meta.helm.sh/release-name=kwerft meta.helm.sh/release-namespace=kwerft-system >>"$LOG_FILE" 2>&1
  kc label namespace "$1" --overwrite app.kubernetes.io/managed-by=Helm >>"$LOG_FILE" 2>&1
}

# registry_address_taken prints the Service, other than zot's own, that holds
# the registry's fixed ClusterIP (an older Service may have been given it
# before Kwerft reserved it); nothing when the address is free.
registry_address_taken() {
  kc get services --all-namespaces \
    -o jsonpath="{range .items[?(@.spec.clusterIP==\"$REGISTRY_CLUSTER_IP\")]}{.metadata.namespace}/{.metadata.name}{\"\\n\"}{end}" 2>/dev/null \
    | grep -v -x "kwerft-system/kwerft-registry" || true
}

# apply_console_settings records the console hostname, and the apps domain
# from --config, in the ConsoleSettings "kwerft" that the console's Settings
# page edits and its reconcilers apply. Precedence: a hostname given to this
# run replaces the setting; without one the setting stays as it is (the
# first run creates it). An apps domain or DNS-01 solver in --config likewise
# replaces what Settings chose; without them it is left alone.
apply_console_settings() {
  if [[ -z "$(cluster_setting '{.metadata.name}')" ]]; then
    printf 'apiVersion: kwerft.dev/v1alpha1\nkind: ConsoleSettings\nmetadata:\n  name: kwerft\nspec:\n  consoleDomain: %s\n' "$DOMAIN" \
      | kc apply -f - >>"$LOG_FILE" 2>&1 || die $EXIT_KWERFT "Could not save the console settings"
  elif (( DOMAIN_EXPLICIT )); then
    kc patch consolesettings.kwerft.dev kwerft --type merge -p "{\"spec\":{\"consoleDomain\":\"$DOMAIN\"}}" \
      >>"$LOG_FILE" 2>&1 || die $EXIT_KWERFT "Could not save the console hostname"
  fi
  [[ -n "$APPS_DOMAIN" ]] || return 0

  local spec="{\"appsDomain\":\"$APPS_DOMAIN\",\"tls\":\"http01\",\"dns\":null}"
  if [[ "$DNS_SOLVER" == "hetzner" ]]; then
    # The token file stays where it is; the cluster gets a copy in the Secret
    # only owners and admins may write (Settings) and nobody may read.
    kc -n kwerft-system create secret generic kwerft-dns-token --from-file=token="$DNS_TOKEN_FILE" \
      --dry-run=client -o yaml | kc apply -f - >>"$LOG_FILE" 2>&1 \
      || die $EXIT_KWERFT "Could not store the DNS API token from $DNS_TOKEN_FILE"
    # With dns.records (the default) Kwerft also keeps the A/AAAA records of
    # the console hostname and *.<appsDomain> in the token's zones.
    spec="{\"appsDomain\":\"$APPS_DOMAIN\",\"tls\":\"dns01\",\"dns\":{\"provider\":\"hetzner\",\"manageRecords\":$DNS_RECORDS}}"
    # A changed token makes the console retry a failed wildcard certificate.
    local sum
    sum=$(sha256sum "$DNS_TOKEN_FILE" | awk '{print $1}')
    if [[ "$sum" != "$(cat "$DNS_TOKEN_SUM_FILE" 2>/dev/null || true)" ]]; then
      kc annotate consolesettings.kwerft.dev kwerft --overwrite "kwerft.dev/dns-token-updated-at=$(date -u +%FT%TZ)" \
        >>"$LOG_FILE" 2>&1 || die $EXIT_KWERFT "Could not save the console settings"
      printf '%s\n' "$sum" >"$DNS_TOKEN_SUM_FILE"
    fi
  fi
  kc patch consolesettings.kwerft.dev kwerft --type merge -p "{\"spec\":$spec}" \
    >>"$LOG_FILE" 2>&1 || die $EXIT_KWERFT "Could not save the apps domain"
}

stage_handoff() {
  # DNS must point here before Let's Encrypt can issue the console certificate.
  # getent exits 2 when the name does not resolve yet; that is the warning
  # below, not a stage failure (errexit + pipefail would otherwise exit 2).
  local resolved managed
  resolved=$(getent ahostsv4 "$DOMAIN" 2>/dev/null | awk '{print $1; exit}' || true)
  managed=$(cluster_setting '{.spec.dns.manageRecords}')
  if [[ "$resolved" != "$PUBLIC_IP" && "$managed" == "true" && -z "$resolved" ]]; then
    printf '  DNS         Kwerft creates %s → %s in Hetzner DNS (Settings shows the record); the certificate follows.\n' "$DOMAIN" "$PUBLIC_IP"
    printf '  Sign in with the owner account from %s\n' "$CONFIG_FILE"
  elif [[ "$resolved" != "$PUBLIC_IP" ]]; then
    warn "$DOMAIN resolves to '${resolved:-nothing}', expected $PUBLIC_IP. The certificate is issued once DNS is fixed."
  fi

  mkdir -p "$CONF_DIR"; chmod 0700 "$CONF_DIR"
  local state
  state=$(setup_token_state)
  case "$state" in
    missing|expired)
      create_setup_token
      state="token ready" ;;
    complete)
      # Used up or no longer needed; nothing left to protect.
      rm -f "$SETUP_TOKEN_FILE"
      kc -n kwerft-system delete secret kwerft-setup-token --ignore-not-found >/dev/null 2>&1 || true
      state="setup complete" ;;
    pending)
      state="token ready" ;;
    config)
      state="owner from config" ;;
  esac
  echo "DNS ${resolved:-unresolved} · $state"
}

# setup_token_state prints where first-run setup stands:
#   config    owner comes from --config, no token needed
#   missing   no token yet (first install, or the file was lost)
#   pending   a valid token is waiting to be used
#   expired   the token ran out before setup was done
#   complete  Kwerft consumed the token (it deletes the Secret once the owner exists)
setup_token_state() {
  if [[ -n "$CONFIG_FILE" ]]; then echo config; return; fi
  # The console knows best: once an owner exists, setup is over for good —
  # even if the token file and Secret are gone (a re-run after a re-run).
  if console_setup_complete; then echo complete; return; fi
  local expires
  expires=$(kc -n kwerft-system get secret kwerft-setup-token \
    -o jsonpath='{.data.expires}' 2>/dev/null | base64 -d 2>/dev/null || true)
  if [[ -z "$expires" ]]; then
    # The Secret is written before the file, so file-without-Secret means used.
    if [[ -s "$SETUP_TOKEN_FILE" ]]; then echo complete; else echo missing; fi
    return
  fi
  [[ -s "$SETUP_TOKEN_FILE" ]] || { echo missing; return; }
  # Both timestamps are fixed-width UTC (%FT%TZ), so string order is time order.
  if [[ "$expires" > "$(date -u +%FT%TZ)" ]]; then echo pending; else echo expired; fi
}

# console_setup_complete asks the running console, through the API server's
# service proxy (no ports to open, no TLS to trust), whether an owner exists.
# Unreachable or not yet ready counts as "no": the token logic below decides.
console_setup_complete() {
  local body
  body=$(kc get --raw "/api/v1/namespaces/kwerft-system/services/kwerft:http/proxy/api/v1/setup" \
    --request-timeout=10s 2>/dev/null || true)
  [[ "$body" == *'"complete":true'* ]]
}

create_setup_token() {
  local token hash
  token="kwft_setup_$(head -c 15 /dev/urandom | base32 | tr -d '=' | tr '[:upper:]' '[:lower:]')"
  hash=$(printf '%s' "$token" | sha256sum | awk '{print $1}')
  # Kwerft only ever sees the hash; the token itself never leaves this disk.
  # Secret first, then the file: see setup_token_state.
  kc -n kwerft-system create secret generic kwerft-setup-token \
    --from-literal=sha256="$hash" \
    --from-literal=expires="$(date -u -d '+24 hours' +%FT%TZ)" \
    --dry-run=client -o yaml | kc apply -f - >/dev/null
  (umask 077; printf '%s\n' "$token" >"$SETUP_TOKEN_FILE")
}

print_summary() {
  local secs=$(( $(date +%s) - START_TS ))
  echo
  if [[ -n "$CONFIG_FILE" ]]; then
    printf '  Open        %shttps://%s%s\n' "$C_ACC" "$DOMAIN" "$C_0"
    printf '  Sign in with the owner account from %s\n' "$CONFIG_FILE"
  elif [[ -s "$SETUP_TOKEN_FILE" ]]; then
    printf '  Open        %shttps://%s/setup%s\n' "$C_ACC" "$DOMAIN" "$C_0"
    printf '  Setup token stored at %s (mode 0600, single use, 24 h)\n' "$SETUP_TOKEN_FILE"
  else
    printf '  Open        %shttps://%s%s and sign in\n' "$C_ACC" "$DOMAIN" "$C_0"
  fi
  local active apps tls
  active=$(cluster_setting '{.status.consoleDomain}')
  if [[ -n "$active" && "$active" != "$DOMAIN" ]]; then
    printf '  %s!%s The console moves to %s once its certificate is issued; until then it is at https://%s\n' "$C_WARN" "$C_0" "$DOMAIN" "$active"
  fi
  apps=$(cluster_setting '{.spec.appsDomain}')
  tls=$(cluster_setting '{.spec.tls}')
  local apps_dns="DNS: *.$apps → $PUBLIC_IP"
  [[ "$(cluster_setting '{.spec.dns.manageRecords}')" == "true" ]] && apps_dns="DNS records kept by Kwerft"
  if [[ -n "$apps" ]]; then
    if [[ "$tls" == "dns01" ]]; then
      printf '  Apps        *.%s (wildcard certificate via Hetzner DNS) · %s\n' "$apps" "$apps_dns"
    else
      printf '  Apps        <app>.%s (a certificate per hostname) · %s\n' "$apps" "$apps_dns"
    fi
  fi
  printf '  Log         %s\n\n' "$LOG_FILE"
  if is_temp_domain; then
    printf '%s!%s %s is a temporary hostname from the public sslip.io service.\n' "$C_WARN" "$C_0" "$DOMAIN"
    printf '  To use your own, point an A record at %s and change it under Settings in the console,\n  or re-run with --domain ops.example.com\n\n' "$PUBLIC_IP"
  fi
  printf '%sDone in %dm %02ds.%s Re-run the same command any time to repair or upgrade.\n' "$C_OK" $((secs / 60)) $((secs % 60)) "$C_0"
}

# ---------------------------------------------------------------------------
# Join mode: k3s agent (or additional server) joining an existing cluster
# ---------------------------------------------------------------------------
# TODO(phase-5): when nodes have no shared private network, the console must open
# 6443/tcp, 10250/tcp and the Cilium ports on existing nodes for the joiner's
# public IP before it connects. Today joining assumes a Cloud Network or vSwitch.
stage_join() {
  local info server k3s_token
  info=$(curl -fsS --max-time 20 -H "Authorization: Bearer $JOIN_TOKEN" "${JOIN_URL%/}/api/v1/join?role=$JOIN_ROLE") \
    || die $EXIT_NETWORK "Could not reach $JOIN_URL or the join token was rejected"
  server=$(jq -r .server <<<"$info"); k3s_token=$(jq -r .token <<<"$info")
  [[ -n "$server" && "$server" != null ]] || die $EXIT_NETWORK "Join response did not contain a server address"

  local node_ip=${PRIVATE_IP:-$PUBLIC_IP} kind=agent
  [[ "$JOIN_ROLE" == "control-plane" ]] && kind=server
  mkdir -p /etc/rancher/k3s
  {
    echo "# Managed by Kwerft installer (join)."
    echo "server: $server"
    echo "token: $k3s_token"
    echo "node-ip: $node_ip"
    echo "node-external-ip: $PUBLIC_IP"
    echo "node-label: [kwerft.dev/platform=$PLATFORM]"
  } >/etc/rancher/k3s/config.yaml
  chmod 0600 /etc/rancher/k3s/config.yaml
  if [[ "$kind" == "server" ]]; then
    # Control-plane joiners need the same cluster-wide settings as the first server.
    write_k3s_config
    sed -i '/^cluster-init:/d' /etc/rancher/k3s/config.yaml
    printf 'server: %s\ntoken: %s\n' "$server" "$k3s_token" >>/etc/rancher/k3s/config.yaml
  fi
  write_registry_mirror >/dev/null   # before k3s first starts, as on the first server
  curl -fsSL https://get.k3s.io | INSTALL_K3S_VERSION="$K3S_VERSION" sh -s - "$kind" >>"$LOG_FILE" 2>&1 \
    || die $EXIT_K8S "k3s $kind installation failed"
  echo "k3s $kind joined $server"
}

# ---------------------------------------------------------------------------
# Maintenance
# ---------------------------------------------------------------------------
do_reset_firewall() {
  [[ $EUID -eq 0 ]] || die $EXIT_PREFLIGHT "Run as root (sudo)."
  nft delete table inet kwerft 2>/dev/null || true
  rm -f /etc/nftables.d/kwerft.nft "$STATE_DIR/stages/firewall.done"
  say "Kwerft host firewall removed. Run the installer again to restore the baseline."
}

do_uninstall() {
  [[ $EUID -eq 0 ]] || die $EXIT_PREFLIGHT "Run as root (sudo)."
  confirm "Remove Kwerft, k3s and all workloads on this server?" || die $EXIT_USAGE "Aborted"
  if (( DRY_RUN )); then say "Would run k3s uninstall and remove $STATE_DIR $CONF_DIR $REGISTRIES_FILE"; return; fi
  [[ -x /usr/local/bin/k3s-uninstall.sh ]] && /usr/local/bin/k3s-uninstall.sh >>"$LOG_FILE" 2>&1
  [[ -x /usr/local/bin/k3s-agent-uninstall.sh ]] && /usr/local/bin/k3s-agent-uninstall.sh >>"$LOG_FILE" 2>&1
  do_reset_firewall >/dev/null
  remove_registry_mirror
  rm -rf "$STATE_DIR" "$CONF_DIR" /etc/sysctl.d/90-kwerft.conf /etc/modules-load.d/kwerft.conf /etc/ssh/sshd_config.d/90-kwerft.conf
  say "Kwerft and k3s removed. Logs kept in $LOG_DIR."
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
  parse_args "$@"
  if (( ! DRY_RUN )) && [[ $EUID -eq 0 ]]; then
    mkdir -p "$LOG_DIR" "$STATE_DIR"; chmod 0700 "$STATE_DIR"
    touch "$LOG_FILE"; chmod 0600 "$LOG_FILE"
  fi
  trap 'on_error $? $LINENO' ERR

  case "$MODE" in
    reset-firewall) do_reset_firewall; return ;;
    uninstall)      do_uninstall; return ;;
  esac

  detect_platform
  detect_addresses
  [[ "$MODE" == "join" ]] || resolve_domain

  printf '%s▸ Kwerft installer %s%s  %schannel=%s · platform=%s · mode=%s%s\n\n' \
    "$C_ACC$C_B" "$KWERFT_VERSION" "$C_0" "$C_DIM" "$CHANNEL" "$PLATFORM" "$MODE" "$C_0"

  if [[ "$MODE" != "join" ]] && is_temp_domain; then
    warn "No --domain given: using temporary hostname $DOMAIN (fine for trying Kwerft, not for production)."
    echo
  fi

  run_stage preflight "Preflight" stage_preflight force
  run_stage system    "System"    stage_system
  run_stage firewall  "Firewall"  stage_firewall force

  if [[ "$MODE" == "join" ]]; then
    run_stage join "Join cluster" stage_join
    run_stage registry "Registry mirror" stage_registry_mirror force
    printf '\n%sThis node will appear in the console within a minute.%s\n' "$C_OK" "$C_0"
    return
  fi

  run_stage kubernetes    "Kubernetes"    stage_kubernetes
  run_stage registry      "Registry mirror" stage_registry_mirror force
  run_stage helm          "Helm"          stage_helm
  run_stage network       "Network"       stage_network force
  run_stage ingress       "Ingress & TLS" stage_ingress_tls force
  run_stage observability "Observability" stage_observability force
  run_stage kwerft         "Kwerft"         stage_kwerft force
  run_stage handoff       "Handoff"       stage_handoff force

  (( DRY_RUN )) || print_summary
}

# Tests source this file with KWERFT_SOURCED=1 to call individual functions.
if [[ "${KWERFT_SOURCED:-0}" != "1" ]]; then
  main "$@"
fi
