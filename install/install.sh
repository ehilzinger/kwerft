#!/usr/bin/env bash
# Kwerft installer — turns a fresh Ubuntu server on Hetzner (Cloud or dedicated)
# into a single-node Kubernetes cluster with the Kwerft console on top.
#
#   curl -fsSL https://kwerft.dev/install.sh | sudo bash -s -- --domain ops.example.com --email ops@example.com --yes
#
# Released copies come from the public ehilzinger/kwerft-install repository:
# main holds the latest stable release, v<version>/install.sh every release.
#
# The script is idempotent and safe to re-run. Most stages converge on every
# run: system packages and sysctls, the firewall, Helm, the platform charts and
# Kwerft itself, so re-running the installer of a newer release upgrades Kwerft
# and its platform. Kubernetes (k3s) and joining a cluster run once: k3s keeps
# the version it was installed with, on every node, and is upgraded from the
# console (Settings › Updates), node by node. Settings given once (--email,
# --acme-server, --lite, …) are remembered in /var/lib/kwerft/install.env.
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
# Hetzner Cloud integrations (charts.hetzner.cloud), latest as of 2026-10-05.
HCLOUD_CCM_CHART_VERSION="1.38.0"       # hcloud cloud-controller-manager, only with hcloud.cloudControllerManager
HCLOUD_CSI_CHART_VERSION="2.23.0"       # hcloud CSI driver: storage class hcloud-volumes
# The Cloud locations hcloud-volumes may provision in (2026-10-05). Its
# allowedTopologies keep Cloud Volumes off nodes without the CSI driver
# (dedicated servers); a new Hetzner location needs adding here.
HCLOUD_LOCATIONS="fsn1 nbg1 hel1 ash hil sin"
# Builds from Git, latest stable as of 2026-10-04. The chart's values.yaml has
# the same defaults (install/test/install.bats checks that they agree).
ZOT_VERSION="v2.1.21"                   # in-cluster registry, ghcr.io/project-zot/zot-minimal
BUILDKIT_VERSION="v0.33.1"              # docker.io/moby/buildkit:<version>-rootless
RAILPACK_VERSION="v0.40.1"              # ghcr.io/railwayapp/railpack-frontend (contains the railpack CLI)
# k3s upgrades from the console (Settings › Updates) go through Rancher's
# system-upgrade-controller and its Plans; latest stable as of 2026-10-05.
SYSTEM_UPGRADE_CONTROLLER_VERSION="v0.20.2"
# Backups (docs/phase6.md), latest stable as of 2026-10-05. Chart 12.2.0
# ships Velero 1.18.2; the image runs the patch release, and the chart's CRD
# job (velero install --crds-only, from that image) applies its CRDs.
VELERO_VERSION="v1.18.4"                # docker.io/velero/velero
VELERO_CHART_VERSION="12.2.0"           # vmware-tanzu/velero
VELERO_PLUGIN_AWS_VERSION="v1.14.4"     # docker.io/velero/velero-plugin-for-aws (S3-compatible storage)
KWERFT_CHART_REPO="oci://ghcr.io/ehilzinger/charts/kwerft"
KWERFT_IMAGE_REPO="ghcr.io/ehilzinger/kwerft"   # the chart's image.repository; checked before installing

# ---------------------------------------------------------------------------
# Exit codes are part of the automation contract — do not renumber.
# ---------------------------------------------------------------------------
readonly EXIT_OK=0 EXIT_USAGE=2 EXIT_PREFLIGHT=10 EXIT_NETWORK=20 EXIT_K8S=30 EXIT_PLATFORM=40 EXIT_KWERFT=50
readonly EXIT_RESTORE=60   # --restore: the backup could not be read or restored

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
readonly ACME_STAGING_URL="https://acme-staging-v02.api.letsencrypt.org/directory"   # --acme-server staging
readonly REGISTRY_HOST="registry.kwerft.internal:5000"   # internal/builds.RegistryHost
readonly REGISTRY_CLUSTER_IP="10.43.0.50"  # zot's fixed ClusterIP (chart: registry.clusterIP), inside SERVICE_CIDR
readonly REGISTRY_MARKER="# Managed by Kwerft installer (registry mirror)."
REGISTRIES_FILE="/etc/rancher/k3s/registries.yaml"   # not readonly so tests can point it elsewhere
BUILD_APPARMOR_FILE="/etc/apparmor.d/kwerft-buildkit"  # likewise
DOMAIN_FILE="$STATE_DIR/domain"           # not readonly so tests can point it elsewhere
DNS_TOKEN_SUM_FILE="$STATE_DIR/dns-token.sha256"  # likewise; tells a changed DNS token from the same one
FIREWALL_STATE_DIR="$STATE_DIR/firewall"  # the node agent's state (internal/firewall); likewise
HCLOUD_TOKEN_SUM_FILE="$STATE_DIR/hcloud-token.sha256"  # likewise; tells a changed Cloud API token from the same one
HCLOUD_TMP_DIR="$STATE_DIR"               # likewise; short-lived copies of the Cloud API token (0700)
K3S_CONFIG_FILE="/etc/rancher/k3s/config.yaml"  # likewise
INSTALL_ENV_FILE="$STATE_DIR/install.env" # settings later runs reuse (remember_settings); likewise
readonly INSTALLER_NODE_LABEL="kwerft.dev/installer"   # the node this script runs on, where $STATE_DIR is
K3S_ETCD_CONFIG_FILE="/etc/rancher/k3s/config.yaml.d/50-kwerft-etcd-snapshots.yaml"  # likewise
readonly ETCD_MARKER="# Managed by Kwerft installer (etcd snapshots)."
readonly ETCD_S3_SECRET="kwerft-etcd-s3"  # kube-system; the console keeps it from Settings › Backups
readonly ETCD_SNAPSHOT_SCHEDULE_DEFAULT="0 */6 * * *"
readonly ETCD_SNAPSHOT_RETENTION_DEFAULT=28
readonly VELERO_NS="velero"
readonly BSL_NAME="kwerft"                # the BackupStorageLocation the console keeps
readonly CONSOLE_UID=65532                # the chart's podSecurityContext.runAsUser
RESTORE_TIMEOUT=${KWERFT_RESTORE_TIMEOUT:-14400}          # seconds a restore may take (volume data)
RESTORE_SYNC_TIMEOUT=${KWERFT_RESTORE_SYNC_TIMEOUT:-600}  # seconds until Velero has read the bucket
RESTORE_POLL=${KWERFT_RESTORE_POLL:-10}                   # seconds between status checks; tests set 0

# env_bool <value> prints 1 for 1/true/yes, 0 for 0/false/no, nothing else.
env_bool() {
  case "$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]')" in
    1|true|yes) echo 1 ;;
    0|false|no) echo 0 ;;
  esac
}

# Options. Precedence: flag, then KWERFT_* environment variable, then what an
# earlier run remembered in install.env (apply_install_env), then the default.
DOMAIN="${KWERFT_DOMAIN:-}"
ACME_EMAIL="${KWERFT_EMAIL:-}"
ACME_SERVER="${KWERFT_ACME_SERVER:-}"   # empty: the chart's default, Let's Encrypt production
CONFIG_FILE="${KWERFT_CONFIG:-}"
PLATFORM="${KWERFT_PLATFORM:-auto}"
PRIVATE_IFACE="${KWERFT_PRIVATE_IFACE:-}"
KWERFT_VERSION="${KWERFT_VERSION:-$KWERFT_VERSION_DEFAULT}"
CHANNEL="${KWERFT_CHANNEL:-stable}"
JOIN_URL="${KWERFT_JOIN_URL:-}"
JOIN_TOKEN="${KWERFT_JOIN_TOKEN:-}"
JOIN_ROLE="${KWERFT_JOIN_ROLE:-worker}"
CONSOLE_URL="${KWERFT_CONSOLE:-}"         # --agent: the console this cluster connects to
CLUSTER_TOKEN="${KWERFT_CLUSTER_TOKEN:-}" # --agent: the cluster's agent token (kwag_<cluster>_…)
NODE_LABELS=()          # --node-label k=v (repeatable): k3s node-label
NODE_TAINTS=()          # --node-taint k=v:Effect (repeatable): k3s node-taint
KWERFT_CHART="${KWERFT_CHART:-}"
IMAGE="${KWERFT_IMAGE:-}"
IMAGE_ARCHIVE="${KWERFT_IMAGE_ARCHIVE:-}"
DOMAIN_EXPLICIT=0       # 1: the console hostname came from --domain, KWERFT_DOMAIN or --config
APPS_DOMAIN=""          # --config appsDomain
DNS_SOLVER=""           # --config dns.solver (hetzner)
DNS_TOKEN_FILE=""       # --config dns.tokenFile
DNS_RECORDS="true"      # --config dns.records: Kwerft keeps the console's and *.<appsDomain>'s records
HCLOUD_TOKEN_FILE=""    # --config hcloud.tokenFile: Hetzner Cloud API token (Cloud Firewall, Load Balancer, CSI, CCM)
HCLOUD_CCM=""           # --config hcloud.cloudControllerManager: true only takes effect on a first install
HCLOUD_LB=""            # --config hcloud.loadBalancer: true|false, a Load Balancer in front of the ingress
MODE="install"
DRY_RUN=0
ASSUME_YES=0
HARDEN_SSH=$(env_bool "${KWERFT_HARDEN_SSH:-}"); HARDEN_SSH=${HARDEN_SSH:-0}
AGENT=0                 # --agent (MODE becomes agent unless --uninstall or --reset-firewall)
AWAIT_CLOUD_TOKEN=0     # --await-cloud-token (agent mode): Cloud Volumes once the console hands over its token
CLOUD_TOKEN_WAIT=${KWERFT_CLOUD_TOKEN_WAIT:-300}   # seconds --await-cloud-token waits
LITE=$(env_bool "${KWERFT_LITE:-}"); LITE=${LITE:-0}
K3S_REQUESTED="${KWERFT_K3S_VERSION:-}"   # --k3s-version: what a new server installs instead of K3S_VERSION
PROGRESS_FILE="${KWERFT_PROGRESS:-}"      # --progress: one JSON line per stage (progress)
FLAGS_GIVEN=" "         # install.env keys this run's flags set (apply_install_env leaves them alone)
PRIVATE_IFACE_GIVEN=""  # --private-iface as given (flag, environment or install.env), not as detected
RESTORE_FROM="${KWERFT_RESTORE:-}"  # --restore latest|<backup>
BACKUP_ENDPOINT="" BACKUP_REGION="" BACKUP_BUCKET="" BACKUP_PREFIX=""   # --config backups.*
BACKUP_ACCESS_KEY_FILE="" BACKUP_SECRET_KEY_FILE="" BACKUP_RECOVERY_KEY_FILE=""

# Discovered facts.
PUBLIC_IP=""
PRIVATE_IP=""
PRIVATE_CIDR=""
PRIVATE_NETWORK=""      # the whole private network the nodes share (private_network)
HCLOUD_NETWORK_ID=""    # the Cloud Network of PRIVATE_IP (metadata service), for the CCM
HCLOUD_NETWORK_RANGE="" # its IP range: Traefik accepts the PROXY protocol from it (Load Balancer)
CURRENT_STAGE="startup"
CURRENT_STAGE_ID=""     # the id of the stage running now (--progress)
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
ok()   { printf '%s✓%s %-15s %s\n' "$C_OK" "$C_0" "$1" "${2:-}"; log "OK $1 ${2:-}"; progress ok "$1" "${2:-}"; }
skip() { printf '%s·%s %-15s %s%s%s\n' "$C_DIM" "$C_0" "$1" "$C_DIM" "${2:-already done}" "$C_0"; log "SKIP $1"; progress skip "$1" "${2:-already done}"; }
warn() { printf '%s!%s %s\n' "$C_WARN" "$C_0" "$*" >&2; log "WARN $*"; }
die()  {
  local code=$1; shift
  printf '%s✗ %s%s\n' "$C_ERR" "$*" "$C_0" >&2; log "FAIL($code) $*"
  progress_fail "$*"
  exit "$code"
}

on_error() {
  local rc=$1 line=$2
  (( BASH_SUBSHELL == 0 )) || return 0   # report once, from the top-level shell
  printf '\n%s✗ Stage "%s" failed (exit %s, line %s).%s\n' "$C_ERR" "$CURRENT_STAGE" "$rc" "$line" "$C_0" >&2
  printf '  Full log: %s · re-run the same command to resume.\n' "$LOG_FILE" >&2
  log "ERROR stage=$CURRENT_STAGE rc=$rc line=$line"
  progress_fail "exit $rc at line $line; see $LOG_FILE"
}

# ---------------------------------------------------------------------------
# Progress (--progress FILE), read by the console's upgrade runner
# (docs/phase6-upgrades.md): one JSON object per line and stage,
#   {"id":"network","label":"Network","state":"ok","detail":"Cilium …","at":"2026-10-05T10:00:00Z"}
# with state ok, skip or fail, and last {"exit":<code>}.
# ---------------------------------------------------------------------------
# json_str prints $1 as a JSON string. Other control characters (terminal
# colours) are dropped.
json_str() {
  local s=$1 bs=\\ q=\"
  s=${s//"$bs"/"$bs$bs"}
  s=${s//"$q"/"$bs$q"}
  s=${s//$'\t'/"${bs}t"}
  s=${s//$'\n'/"${bs}n"}
  s=${s//$'\r'/"${bs}r"}
  printf '"%s"' "$(printf '%s' "$s" | LC_ALL=C tr -d '\000-\010\013\014\016-\037\177')"
}

# progress <state> <label> <detail> appends the running stage's line. It
# never fails the install: the file reports the work, it is not part of it.
progress() {
  [[ -n "$PROGRESS_FILE" && -n "$CURRENT_STAGE_ID" ]] || return 0
  printf '{"id":%s,"label":%s,"state":%s,"detail":%s,"at":"%s"}\n' \
    "$(json_str "$CURRENT_STAGE_ID")" "$(json_str "$2")" "$(json_str "$1")" "$(json_str "$3")" "$(date -u +%FT%TZ)" \
    >>"$PROGRESS_FILE" 2>/dev/null || true
}

# progress_fail <detail>: the running stage failed. die (in the stage's
# subshell) and then on_error (in the top-level shell) both say so; the stage
# gets one line, with die's message when there is one.
progress_fail() {
  [[ -n "$PROGRESS_FILE" && -n "$CURRENT_STAGE_ID" ]] || return 0
  local last
  last=$(tail -n 1 "$PROGRESS_FILE" 2>/dev/null || true)
  if [[ "$last" == "{\"id\":$(json_str "$CURRENT_STAGE_ID"),"* && "$last" == *'"state":"fail"'* ]]; then
    return 0
  fi
  progress fail "$CURRENT_STAGE" "$1"
}

# progress_exit <code> ends the file: the EXIT trap of the top-level shell.
progress_exit() {
  [[ -n "$PROGRESS_FILE" ]] && (( BASH_SUBSHELL == 0 )) || return 0
  printf '{"exit":%d}\n' "$1" >>"$PROGRESS_FILE" 2>/dev/null || true
}

# progress_start empties the file for this run.
progress_start() {
  [[ -n "$PROGRESS_FILE" ]] || return 0
  (umask 077; : >"$PROGRESS_FILE") 2>/dev/null || die $EXIT_USAGE "Cannot write the progress file $PROGRESS_FILE"
}

usage() {
  cat <<EOF
Kwerft installer ${KWERFT_VERSION_DEFAULT}

Usage: install.sh [options]

Install:
  --domain HOST          Console hostname. Without it, a temporary <public-ip>.sslip.io
                         name is used so you can try Kwerft before setting up DNS
  --email ADDR           Let's Encrypt account contact (optional)
  --acme-server URL      ACME directory for certificates, or "staging" for Let's Encrypt's
                         staging CA (untrusted certificates, generous rate limits: for tests).
                         Default: Let's Encrypt production
  --config FILE          Pre-seed owner, DNS, Hetzner tokens; skips the setup wizard
  --platform P           auto | cloud | dedicated (default: auto)
  --private-iface IF     Interface for node-to-node and API traffic
  --version V            Kwerft release to install (default: ${KWERFT_VERSION_DEFAULT})
  --channel C            stable | edge
  --lite                 Smaller footprint for 4 GB servers (no Hubble, short retention)
  --harden-ssh           Disable SSH password login and root password login
  --k3s-version V        k3s release a new server installs (default: ${K3S_VERSION}); also
                         with --join and --agent. A running cluster keeps its version:
                         upgrade it under Settings › Updates

  Re-running converges everything but Kubernetes. --email, --acme-server, --platform,
  --private-iface, --lite, --harden-ssh, --channel, the mode and --console are
  remembered in ${STATE_DIR}/install.env for later runs (flags and KWERFT_* win).

Join an existing cluster:
  --join URL --token T   Join the cluster whose console runs at URL
  --role R               worker | control-plane (default: worker)
  --node-label K=V       Label this node (repeatable; node pools set kwerft.dev/pool)
  --node-taint K=V:E     Taint this node (repeatable; build pools set kwerft.dev/builds=true:NoSchedule)

Connect a new cluster to a console (agent mode):
  --agent                Install Kwerft without its own console: this cluster is
                         managed from the console at --console (Clusters › Adopt
                         shows the whole command)
  --console URL          The console, https://<console>
  --cluster-token T      This cluster's agent token from the console (kwag_…)
  --await-cloud-token    Wait for the console to hand over its Hetzner Cloud token,
                         then add Cloud Volumes (servers Kwerft creates use this)

Development:
  --image REF            Run this console image (repository:tag) instead of the release
  --image-archive FILE   Import FILE (docker/OCI tarball) into k3s first; needs --image.
                         hack/dev-server.sh uses both to test unreleased builds

Restore onto a new server (docs: Backups):
  --restore B            Restore the console and every project from a backup: "latest"
                         (the newest complete cluster backup) or a backup's name. Needs
                         --config with a backups block (endpoint, region, bucket, prefix,
                         accessKeyFile, secretKeyFile, recoveryKeyFile). The console
                         hostname comes from the backup unless --domain is given

Maintenance:
  --reset-firewall       Remove Kwerft's host firewall and pause the console's
                         firewall rules on this server (rescue)
  --uninstall            Remove Kwerft and k3s from this server

General:
  --dry-run              Print the plan without changing anything
  --yes, -y              Never prompt
  --progress FILE        Write one JSON line per stage to FILE, then {"exit":<code>}
                         (the console's upgrades read it)
  --help, -h             Show this help

Every option can also be set as KWERFT_<NAME> in the environment, e.g. KWERFT_DOMAIN.
Exit codes: 0 ok · 2 usage · 10 preflight · 20 network/DNS · 30 Kubernetes · 40 platform · 50 Kwerft · 60 restore
EOF
}

# ---------------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------------
need_arg() { [[ $# -ge 2 && -n "$2" && "$2" != --* ]] || die $EXIT_USAGE "$1 needs a value"; }

# valid_node_label: key=value with a Kubernetes label key (optional DNS
# prefix) and value. Keeps the k3s config free of anything YAML would read
# differently.
valid_node_label() {
  [[ "$1" =~ ^([a-z0-9]([-a-z0-9.]*[a-z0-9])?/)?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?=([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$ ]]
}

# node_settings prints the node-label and node-taint lists of the k3s
# config: the platform label, then --node-label and --node-taint.
node_settings() {
  local l
  echo "node-label:"
  echo "  - kwerft.dev/platform=$PLATFORM"
  for l in ${NODE_LABELS[@]+"${NODE_LABELS[@]}"}; do echo "  - $l"; done
  if (( ${#NODE_TAINTS[@]} > 0 )); then
    echo "node-taint:"
    for l in "${NODE_TAINTS[@]}"; do echo "  - $l"; done
  fi
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --domain)         need_arg "$@"; DOMAIN=$2; shift 2 ;;
      --email)          need_arg "$@"; ACME_EMAIL=$2; given KWERFT_EMAIL; shift 2 ;;
      --acme-server)    need_arg "$@"; ACME_SERVER=$2; given KWERFT_ACME_SERVER; shift 2 ;;
      --config)         need_arg "$@"; CONFIG_FILE=$2; shift 2 ;;
      --platform)       need_arg "$@"; PLATFORM=$2; given KWERFT_PLATFORM; shift 2 ;;
      --private-iface)  need_arg "$@"; PRIVATE_IFACE=$2; given KWERFT_PRIVATE_IFACE; shift 2 ;;
      --version)        need_arg "$@"; KWERFT_VERSION=$2; shift 2 ;;
      --channel)        need_arg "$@"; CHANNEL=$2; given KWERFT_CHANNEL; shift 2 ;;
      --k3s-version)    need_arg "$@"; K3S_REQUESTED=$2; shift 2 ;;
      --progress)       need_arg "$@"; PROGRESS_FILE=$2; shift 2 ;;
      --join)           need_arg "$@"; JOIN_URL=$2; MODE="join"; shift 2 ;;
      --token)          need_arg "$@"; JOIN_TOKEN=$2; shift 2 ;;
      --role)           need_arg "$@"; JOIN_ROLE=$2; shift 2 ;;
      --agent)          AGENT=1; shift ;;
      --console)        need_arg "$@"; CONSOLE_URL=$2; given KWERFT_CONSOLE; shift 2 ;;
      --cluster-token)  need_arg "$@"; CLUSTER_TOKEN=$2; shift 2 ;;
      --await-cloud-token) AWAIT_CLOUD_TOKEN=1; shift ;;
      --node-label)     need_arg "$@"; NODE_LABELS+=("$2"); shift 2 ;;
      --node-taint)     need_arg "$@"; NODE_TAINTS+=("$2"); shift 2 ;;
      --image)          need_arg "$@"; IMAGE=$2; shift 2 ;;
      --image-archive)  need_arg "$@"; IMAGE_ARCHIVE=$2; shift 2 ;;
      --lite)           LITE=1; given KWERFT_LITE; shift ;;
      --restore)        need_arg "$@"; RESTORE_FROM=$2; shift 2 ;;
      --harden-ssh)     HARDEN_SSH=1; given KWERFT_HARDEN_SSH; shift ;;
      --reset-firewall) MODE="reset-firewall"; shift ;;
      --uninstall)      MODE="uninstall"; shift ;;
      --dry-run)        DRY_RUN=1; shift ;;
      --yes|-y)         ASSUME_YES=1; shift ;;
      --help|-h)        usage; exit $EXIT_OK ;;
      *)                usage >&2; die $EXIT_USAGE "Unknown option: $1" ;;
    esac
  done

  apply_install_env
  if [[ -n "$JOIN_URL" ]]; then
    (( AGENT )) && die $EXIT_USAGE "--agent and --join exclude each other"
    MODE="join"
  fi
  if (( AGENT )) && [[ "$MODE" == "install" ]]; then
    MODE="agent"
  fi
  if [[ "$MODE" == "agent" && -z "$CLUSTER_TOKEN" ]]; then
    CLUSTER_TOKEN=$(stored_agent_token)   # a re-run: the token this cluster's agent uses
  fi
  if [[ "$MODE" == "agent" ]]; then
    [[ -n "$CONSOLE_URL" && -n "$CLUSTER_TOKEN" ]] || die $EXIT_USAGE "--agent needs --console and --cluster-token (Clusters › Adopt in the console shows the whole command)"
    [[ -z "$DOMAIN" && -z "$CONFIG_FILE" ]] || die $EXIT_USAGE "--agent installs no console: --domain and --config do not apply"
    CONSOLE_URL=${CONSOLE_URL%/}
    valid_console_url "$CONSOLE_URL" || die $EXIT_USAGE "--console must look like https://ops.example.com, got '$CONSOLE_URL'"
    valid_cluster_token "$CLUSTER_TOKEN" || die $EXIT_USAGE "--cluster-token is not an agent token (kwag_<cluster>_…); copy the command from the console again"
  elif [[ -n "$CONSOLE_URL" || -n "$CLUSTER_TOKEN" ]] || (( AWAIT_CLOUD_TOKEN )); then
    die $EXIT_USAGE "--console, --cluster-token and --await-cloud-token need --agent"
  fi
  # Releases are tagged v0.2.0; chart versions and image tags drop the "v".
  KWERFT_VERSION=${KWERFT_VERSION#v}
  [[ "$KWERFT_VERSION" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] \
    || die $EXIT_USAGE "--version must look like 0.2.0, v0.2.0 or v0.2.0-rc.1"
  case "$PLATFORM" in auto|cloud|dedicated) ;; *) die $EXIT_USAGE "--platform must be auto, cloud or dedicated" ;; esac
  case "$CHANNEL" in stable|edge) ;; *) die $EXIT_USAGE "--channel must be stable or edge" ;; esac
  case "$JOIN_ROLE" in worker|control-plane) ;; *) die $EXIT_USAGE "--role must be worker or control-plane" ;; esac
  if [[ "$MODE" == "join" ]] && ! stage_done join; then
    [[ -n "$JOIN_URL" ]] || die $EXIT_USAGE "This server has not joined its cluster yet: run the join command (--join URL --token T) again"
    [[ -n "$JOIN_TOKEN" ]] || die $EXIT_USAGE "--join needs --token"
  fi
  if [[ -n "$K3S_REQUESTED" ]] && ! valid_k3s_version "$K3S_REQUESTED"; then
    die $EXIT_USAGE "--k3s-version must be a k3s release like $K3S_VERSION, got '$K3S_REQUESTED'"
  fi
  if [[ -n "$PROGRESS_FILE" && ! -d "$(dirname "$PROGRESS_FILE")" ]]; then
    die $EXIT_USAGE "--progress: the directory of $PROGRESS_FILE does not exist"
  fi
  PRIVATE_IFACE_GIVEN=$PRIVATE_IFACE
  local l
  for l in ${NODE_LABELS[@]+"${NODE_LABELS[@]}"}; do
    valid_node_label "$l" || die $EXIT_USAGE "--node-label must look like key=value (a Kubernetes label), got '$l'"
  done
  for l in ${NODE_TAINTS[@]+"${NODE_TAINTS[@]}"}; do
    [[ "$l" =~ ^[A-Za-z0-9./_-]+(=[A-Za-z0-9._-]*)?:(NoSchedule|PreferNoSchedule|NoExecute)$ ]] \
      || die $EXIT_USAGE "--node-taint must look like key=value:NoSchedule, got '$l'"
  done
  if [[ -n "$CONFIG_FILE" && ! -r "$CONFIG_FILE" ]]; then die $EXIT_USAGE "Config file not readable: $CONFIG_FILE"; fi
  [[ "$ACME_SERVER" == staging ]] && ACME_SERVER=$ACME_STAGING_URL
  if [[ -n "$ACME_SERVER" && ! "$ACME_SERVER" =~ ^https://[^[:space:]]+$ ]]; then
    die $EXIT_USAGE "--acme-server must be an https:// ACME directory URL or staging, got '$ACME_SERVER'"
  fi
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
    HCLOUD_TOKEN_FILE=$(config_get_in hcloud tokenFile)
    HCLOUD_CCM=$(lower "$(config_get_in hcloud cloudControllerManager)")
    HCLOUD_LB=$(lower "$(config_get_in hcloud loadBalancer)")
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
  case "$HCLOUD_CCM" in
    ""|true|false) ;;
    *) die $EXIT_USAGE "hcloud.cloudControllerManager in $CONFIG_FILE must be true or false, got '$HCLOUD_CCM'" ;;
  esac
  case "$HCLOUD_LB" in
    ""|true|false) ;;
    *) die $EXIT_USAGE "hcloud.loadBalancer in $CONFIG_FILE must be true or false, got '$HCLOUD_LB'" ;;
  esac
  if [[ -n "$HCLOUD_TOKEN_FILE" && ! -r "$HCLOUD_TOKEN_FILE" ]]; then
    die $EXIT_USAGE "hcloud.tokenFile not readable: '$HCLOUD_TOKEN_FILE'"
  fi
  if [[ "$HCLOUD_CCM" == "true" && -z "$HCLOUD_TOKEN_FILE" ]]; then
    die $EXIT_USAGE "hcloud.cloudControllerManager in $CONFIG_FILE needs hcloud.tokenFile: the cloud-controller-manager works with the Cloud API"
  fi
  parse_restore_args
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

# valid_console_url accepts https://<hostname>[:port] only: the agent verifies
# the console's certificate against public CAs.
valid_console_url() {
  local host=${1#https://}
  [[ "$1" == https://* ]] || return 1
  host=${host%:[0-9]*}
  valid_hostname "$host" && [[ "${1#https://}" =~ ^[^/?#@]+$ ]]
}

# valid_cluster_token: kwag_<cluster>_<secret> (internal/clusters.NewAgentToken).
valid_cluster_token() {
  [[ "$1" =~ ^kwag_[a-z]([-a-z0-9]*[a-z0-9])?_[A-Za-z0-9_-]{32,}$ ]]
}

# cluster_of_token prints the cluster an agent token belongs to.
cluster_of_token() {
  local rest=${1#kwag_}
  printf '%s' "${rest%%_*}"
}

# valid_k3s_version: a k3s release, v1.37.1+k3s1 (or a candidate, v1.38.0-rc1+k3s1).
valid_k3s_version() {
  [[ "$1" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc[0-9]+)?\+k3s[0-9]+$ ]]
}

# ---------------------------------------------------------------------------
# Remembered settings: /var/lib/kwerft/install.env (0600), written by every
# run (remember_settings) and read by the next one (apply_install_env), so a
# re-run, or the console's upgrade (install.sh --version V --yes --progress
# F), needs no flags. One KWERFT_<NAME>=<value> per line, named like the
# environment variables; an empty value is not set. Never sourced.
#   KWERFT_MODE           install | agent | join
#   KWERFT_EMAIL          --email (or the --config email)
#   KWERFT_ACME_SERVER    --acme-server, as a URL
#   KWERFT_PLATFORM       cloud | dedicated, as detected or given
#   KWERFT_PRIVATE_IFACE  --private-iface as given; empty when detected
#   KWERFT_LITE           0 | 1
#   KWERFT_HARDEN_SSH     0 | 1
#   KWERFT_CHANNEL        stable | edge
#   KWERFT_CONSOLE        --console (agent mode only)
# Not remembered: the console's hostname (ConsoleSettings is the record),
# tokens (agent mode reads its token back from Secret kwerft-system/kwerft-agent),
# --config, --version, --k3s-version and the join command.
# ---------------------------------------------------------------------------
# given <key> records that a flag set a remembered setting.
given() { FLAGS_GIVEN+="$1 "; }

# apply_install_env fills in what install.env remembers and neither a flag
# nor the environment gives, and the mode when no flag chose one.
apply_install_env() {
  [[ "$MODE" == install || "$MODE" == join ]] || return 0   # --uninstall, --reset-firewall
  local line key value stored_mode="" stored_console=""
  if [[ -r "$INSTALL_ENV_FILE" ]]; then
    while IFS= read -r line || [[ -n "$line" ]]; do
      line=${line%$'\r'}
      [[ "$line" =~ ^KWERFT_[A-Z_]+= ]] || continue
      key=${line%%=*} value=${line#*=}
      [[ -n "$value" && "$FLAGS_GIVEN" != *" $key "* && -z "${!key:-}" ]] || continue
      case "$key" in
        KWERFT_MODE)          stored_mode=$value ;;
        KWERFT_EMAIL)
          # An email in this run's --config wins over the remembered one.
          if [[ -z "$CONFIG_FILE" || ! -r "$CONFIG_FILE" || -z "$(config_get email)" ]]; then ACME_EMAIL=$value; fi ;;
        KWERFT_ACME_SERVER)   ACME_SERVER=$value ;;
        KWERFT_PLATFORM)      PLATFORM=$value ;;
        KWERFT_PRIVATE_IFACE) PRIVATE_IFACE=$value ;;
        KWERFT_LITE)          LITE=$(env_bool "$value"); LITE=${LITE:-0} ;;
        KWERFT_HARDEN_SSH)    HARDEN_SSH=$(env_bool "$value"); HARDEN_SSH=${HARDEN_SSH:-0} ;;
        KWERFT_CHANNEL)       CHANNEL=$value ;;
        KWERFT_CONSOLE)       stored_console=$value ;;
      esac
    done <"$INSTALL_ENV_FILE"
  fi
  # Installed before install.env existed: the stages done tell the mode.
  [[ -n "$stored_mode" ]] || stored_mode=$(installed_mode)
  if [[ "$MODE" == install && -z "$JOIN_URL" ]] && (( ! AGENT )); then
    case "$stored_mode" in
      agent) AGENT=1 ;;
      join)  MODE="join" ;;
    esac
  fi
  if (( AGENT )) && [[ -z "$CONSOLE_URL" ]]; then CONSOLE_URL=$stored_console; fi
  return 0
}

# installed_mode prints the mode this server was installed in, from its
# stages, or nothing on a new server.
installed_mode() {
  if stage_done kwerft-agent; then echo agent
  elif stage_done join; then echo join
  elif stage_done kubernetes; then echo install
  fi
}

# stored_agent_token prints the agent token of this cluster (agent mode),
# from the Secret write_agent_secret keeps; nothing before k3s runs.
stored_agent_token() {
  command -v k3s >/dev/null 2>&1 || return 0
  kc -n kwerft-system get secret kwerft-agent -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null | tr -d '[:space:]' || true
}

# install_env prints install.env for this run's settings.
install_env() {
  local v
  printf '# Kwerft installer %s, %s: settings later runs reuse.\n' "$KWERFT_VERSION" "$(date -u +%FT%TZ)"
  printf '# Flags and KWERFT_* environment variables win over this file.\n'
  printf 'KWERFT_MODE=%s\n' "$MODE"
  for v in "KWERFT_EMAIL=$ACME_EMAIL" "KWERFT_ACME_SERVER=$ACME_SERVER" "KWERFT_PLATFORM=$PLATFORM" \
           "KWERFT_PRIVATE_IFACE=$PRIVATE_IFACE_GIVEN" "KWERFT_LITE=$LITE" "KWERFT_HARDEN_SSH=$HARDEN_SSH" \
           "KWERFT_CHANNEL=$CHANNEL"; do
    printf '%s\n' "${v//[$'\n\r']/}"
  done
  if [[ "$MODE" == agent ]]; then printf 'KWERFT_CONSOLE=%s\n' "${CONSOLE_URL//[$'\n\r']/}"; fi
}

# remember_settings writes install.env (0600), replacing it in one step.
remember_settings() {
  if (( DRY_RUN )) || [[ ! -w "$(dirname "$INSTALL_ENV_FILE")" ]]; then return 0; fi
  case "$MODE" in install|agent|join) ;; *) return 0 ;; esac
  (umask 077; install_env >"$INSTALL_ENV_FILE.kwerft-new")
  mv -f "$INSTALL_ENV_FILE.kwerft-new" "$INSTALL_ENV_FILE"
}

# parse_restore_args checks --restore and reads the backups block of --config:
#   backups:
#     endpoint: https://fsn1.your-objectstorage.com
#     region: fsn1                      # default: from a Hetzner endpoint, else us-east-1
#     bucket: acme-kwerft
#     prefix: ops.example.com           # default: --domain
#     accessKeyFile: /root/s3.access
#     secretKeyFile: /root/s3.secret
#     recoveryKeyFile: /root/kwerft-recovery.key
parse_restore_args() {
  [[ -n "$RESTORE_FROM" ]] || return 0
  [[ "$MODE" == "install" ]] || die $EXIT_USAGE "--restore rebuilds a console on a new server; it does not combine with --agent, --join, --uninstall or --reset-firewall"
  (( LITE )) && die $EXIT_USAGE "--restore needs Velero, which --lite leaves out"
  [[ "$RESTORE_FROM" == "latest" || ( ${#RESTORE_FROM} -le 253 && "$RESTORE_FROM" =~ ^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$ ) ]] \
    || die $EXIT_USAGE "--restore takes latest or a backup's name, got '$RESTORE_FROM'"
  [[ -n "$CONFIG_FILE" ]] || die $EXIT_USAGE "--restore needs --config with a backups block (endpoint, region, bucket, prefix, accessKeyFile, secretKeyFile, recoveryKeyFile)"
  BACKUP_ENDPOINT=$(config_get_in backups endpoint)
  BACKUP_REGION=$(config_get_in backups region)
  BACKUP_BUCKET=$(config_get_in backups bucket)
  BACKUP_PREFIX=$(config_get_in backups prefix)
  BACKUP_ACCESS_KEY_FILE=$(config_get_in backups accessKeyFile)
  BACKUP_SECRET_KEY_FILE=$(config_get_in backups secretKeyFile)
  BACKUP_RECOVERY_KEY_FILE=$(config_get_in backups recoveryKeyFile)
  BACKUP_ENDPOINT=${BACKUP_ENDPOINT%/}
  [[ "$BACKUP_ENDPOINT" =~ ^https://[A-Za-z0-9.-]+(:[0-9]+)?$ ]] \
    || die $EXIT_USAGE "backups.endpoint in $CONFIG_FILE must look like https://fsn1.your-objectstorage.com, got '$BACKUP_ENDPOINT'"
  [[ "$BACKUP_BUCKET" =~ ^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$ ]] \
    || die $EXIT_USAGE "backups.bucket in $CONFIG_FILE must be a bucket name, got '$BACKUP_BUCKET'"
  if [[ -z "$BACKUP_REGION" ]]; then
    local host=${BACKUP_ENDPOINT#https://}
    host=${host%%:*}
    if [[ "$host" == *.your-objectstorage.com ]]; then BACKUP_REGION=${host%%.*}; else BACKUP_REGION=us-east-1; fi
  fi
  [[ "$BACKUP_REGION" =~ ^[a-z0-9-]+$ ]] || die $EXIT_USAGE "backups.region in $CONFIG_FILE must look like fsn1, got '$BACKUP_REGION'"
  BACKUP_PREFIX=${BACKUP_PREFIX#/}
  BACKUP_PREFIX=${BACKUP_PREFIX%/}
  [[ -n "$BACKUP_PREFIX" ]] || BACKUP_PREFIX=$DOMAIN
  [[ -n "$BACKUP_PREFIX" ]] \
    || die $EXIT_USAGE "backups.prefix in $CONFIG_FILE is needed: the folder in the bucket, by default the console's hostname (Settings › Backups shows it)"
  [[ ${#BACKUP_PREFIX} -le 200 && "$BACKUP_PREFIX" =~ ^[A-Za-z0-9._-]+(/[A-Za-z0-9._-]+)*$ && "/$BACKUP_PREFIX/" != */../* ]] \
    || die $EXIT_USAGE "backups.prefix in $CONFIG_FILE must be a folder like ops.example.com, got '$BACKUP_PREFIX'"
  local key f
  for key in accessKeyFile secretKeyFile recoveryKeyFile; do
    f=$(config_get_in backups "$key")
    [[ -n "$f" && -r "$f" ]] || die $EXIT_USAGE "backups.$key in $CONFIG_FILE not readable: '$f'"
  done
  [[ "$(recovery_key <"$BACKUP_RECOVERY_KEY_FILE")" =~ ^[A-Z2-7]{52}$ ]] \
    || die $EXIT_USAGE "backups.recoveryKeyFile ($BACKUP_RECOVERY_KEY_FILE) does not hold a recovery key: 52 characters A-Z and 2-7, as the console showed it"
  return 0
}

# recovery_key turns a recovery key as the console shows it (groups of four
# base32 characters) into the Kopia repository password Velero uses: upper
# case, without spaces, dashes or line breaks. Reads stdin.
recovery_key() {
  tr -d '[:space:]-' | tr '[:lower:]' '[:upper:]'
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
# must always converge, such as the Helm releases on upgrade). Only
# kubernetes and join run once: k3s is upgraded from the console.
run_stage() {
  local id=$1 label=$2 fn=$3 force=${4:-}
  CURRENT_STAGE=$label CURRENT_STAGE_ID=$id
  if (( DRY_RUN )); then
    printf '%s→%s %-15s %s(would run %s)%s\n' "$C_ACC" "$C_0" "$label" "$C_DIM" "$fn" "$C_0"
    CURRENT_STAGE_ID=""
    return 0
  fi
  if [[ -z "$force" ]] && stage_done "$id"; then
    skip "$label" "$(skip_detail "$id")"
    CURRENT_STAGE_ID=""
    return 0
  fi
  local detail
  detail=$("$fn")              # a failure here exits via errexit with the stage's code
  mark_done "$id"
  ok "$label" "${detail##*$'\n'}"
  CURRENT_STAGE_ID=""
}

# skip_detail <id> prints what a stage that already ran reports instead of
# "already done": for k3s, its version, and whether it is behind this release.
skip_detail() {
  case "$1" in
    kubernetes|join) k3s_status ;;
  esac
}

# k3s_status: "k3s v1.37.1+k3s1 · installed", or that it is older than the pin.
k3s_status() {
  local running
  running=$(k3s --version 2>/dev/null | awk 'NR == 1 {print $3}' || true)
  [[ -n "$running" ]] || return 0
  if k3s_older "$running" "$K3S_VERSION"; then
    echo "k3s $running is older than this release's $K3S_VERSION: upgrade it in Settings › Updates"
  else
    echo "k3s $running · installed"
  fi
}

# k3s_older <a> <b>: k3s version a is older than b (major, minor, patch, then
# the k3s build: v1.37.1+k3s1 < v1.37.1+k3s2 < v1.37.2+k3s1). Unknown formats
# are never older.
k3s_older() {
  local re='^v?([0-9]+)\.([0-9]+)\.([0-9]+)(-[^+]*)?(\+k3s([0-9]+))?$' a b i
  [[ "$1" =~ $re ]] || return 1
  a=("${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}" "${BASH_REMATCH[6]:-0}")
  [[ "$2" =~ $re ]] || return 1
  b=("${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}" "${BASH_REMATCH[6]:-0}")
  for i in 0 1 2 3; do
    (( 10#${a[i]} < 10#${b[i]} )) && return 0
    (( 10#${a[i]} > 10#${b[i]} )) && return 1
  done
  return 1
}

# k3s_target: the k3s version a new server installs.
k3s_target() { echo "${K3S_REQUESTED:-$K3S_VERSION}"; }

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
    # Never the cluster's own: once it runs, cilium_host carries a pod
    # address, so CNI and container interfaces and the pod and service
    # networks are skipped (a re-run on a server without a private network
    # must not take one).
    local pod_prefix=${POD_CIDR%.0.0/16}. svc_prefix=${SERVICE_CIDR%.0.0/16}.
    PRIVATE_CIDR=$(ip -4 -o addr show scope global 2>/dev/null \
      | awk -v d="$default_if" '$2 != d && $2 !~ /^(cilium|lxc|veth|cni|flannel|kube-|docker|br-|virbr|vxlan|genev)/ {print $2, $4}' \
      | awk -v p="$pod_prefix" -v s="$svc_prefix" '$2 ~ /^(10\.|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.)/ && index($2, p) != 1 && index($2, s) != 1 {print $2; exit}' || true)
    PRIVATE_IFACE=$(ip -4 -o addr show scope global 2>/dev/null | awk -v c="$PRIVATE_CIDR" '$4 == c {print $2; exit}' || true)
  fi
  PRIVATE_IP=${PRIVATE_CIDR%/*}
}

# detect_hcloud_network finds the Cloud Network the private address is in,
# from the metadata service: its ID (the cloud-controller-manager's
# HCLOUD_NETWORK) and its whole range (where a Load Balancer connects from).
detect_hcloud_network() {
  [[ "$PLATFORM" == "cloud" && -n "$PRIVATE_IP" ]] || return 0
  local meta range id
  meta=$(curl -fsS --max-time 3 http://169.254.169.254/hetzner/v1/metadata/private-networks 2>/dev/null) || return 0
  id=$(hcloud_network_field "$meta" "$PRIVATE_IP" network_id)
  range=$(hcloud_network_field "$meta" "$PRIVATE_IP" network)
  [[ "$id" =~ ^[0-9]+$ ]] && HCLOUD_NETWORK_ID=$id
  [[ "$range" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+$ ]] && HCLOUD_NETWORK_RANGE=$(network_of "$range")
  return 0
}

# private_network sets PRIVATE_NETWORK, where the other nodes' private
# addresses are: the Cloud Network's whole range (from the metadata service;
# it also holds vSwitch subnets), else the route through the private
# interface (Hetzner Cloud hands out /32 addresses with a route to the
# network), else the interface's own network. Never just this server's /32:
# the firewall would then refuse every other node.
private_network() {
  PRIVATE_NETWORK=""
  [[ -n "$PRIVATE_CIDR" ]] || return 0
  if [[ -n "$HCLOUD_NETWORK_RANGE" ]]; then
    PRIVATE_NETWORK=$HCLOUD_NETWORK_RANGE
    return 0
  fi
  if [[ "${PRIVATE_CIDR#*/}" == "32" ]]; then
    local route
    route=$(ip -4 route show dev "$PRIVATE_IFACE" 2>/dev/null \
      | awk '$1 ~ /^[0-9.]+\/[0-9]+$/ && $1 !~ /\/32$/ {print $1; exit}' || true)
    if [[ -n "$route" ]]; then
      PRIVATE_NETWORK=$(network_of "$route")
      return 0
    fi
  fi
  PRIVATE_NETWORK=$(network_of "$PRIVATE_CIDR")
}

# hcloud_network_field <metadata yaml> <ip> <key> prints key of the entry of
# the metadata's private-networks list whose ip is <ip>:
#   - ip: 10.0.0.2
#     network_id: 1234
#     network: 10.0.0.0/16
hcloud_network_field() {
  awk -v want="$2" -v key="$3" '
    function flush() { if (f["ip"] == want && (key in f)) { print f[key]; found = 1 } delete f }
    /^-/ { if (!found) flush(); sub(/^-[[:space:]]*/, "") }
    !found && match($0, /^[[:space:]]*[a-z_]+:/) {
      k = substr($0, RSTART, RLENGTH - 1); gsub(/[[:space:]]/, "", k)
      v = substr($0, RSTART + RLENGTH); gsub(/^[[:space:]]+|[[:space:]]+$/, "", v)
      f[k] = v
    }
    END { if (!found) flush() }
  ' <<<"$1"
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
  if [[ "$MODE" == "install" || "$MODE" == "agent" ]]; then check_release; fi
  if [[ "$HCLOUD_CCM" == "true" && "$PLATFORM" != "cloud" ]]; then
    die $EXIT_PREFLIGHT "hcloud.cloudControllerManager needs a Hetzner Cloud server; this is $PLATFORM."
  fi

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
# The console's node agent (Network → Server firewall) only ever fills the
# two chains managed_ssh and managed_open, which the base chain jumps to:
# Kwerft can narrow public SSH and open more ports, never close HTTP(S), the
# cluster's traffic or SSH from the private network. Empty chains (or a
# missing table) mean exactly this baseline.
# ---------------------------------------------------------------------------
# firewall_ruleset prints /etc/nftables.d/kwerft.nft. Loading it replaces the
# base chain in one transaction and keeps what the node agent put in the
# managed chains (declaring a chain does not flush it), so re-running the
# installer never opens a window without a firewall.
firewall_ruleset() {
  local priv_rule="# no private network detected"
  [[ -n "$PRIVATE_NETWORK" ]] && priv_rule="ip saddr $PRIVATE_NETWORK accept"
  cat <<EOF
# Managed by Kwerft — edit rules in the console (Network → Server firewall).
# Rescue: install.sh --reset-firewall (removes this table and pauses the
# console's rules on this node).
table inet kwerft {
  # Filled by Kwerft's node agent: narrowed public SSH (sources accept, rest drop).
  chain managed_ssh {
  }
  # Filled by Kwerft's node agent: ports opened in the console.
  chain managed_open {
  }
  chain input {
    type filter hook input priority 0; policy drop;
  }
}
flush chain inet kwerft input
table inet kwerft {
  chain input {
    ct state established,related accept
    iif lo accept
    meta l4proto { icmp, ipv6-icmp } accept
    iifname { "cilium_*", "lxc*" } accept
    ip saddr $POD_CIDR accept
    $priv_rule
    udp dport $WG_PORT accept
    tcp dport 22 jump managed_ssh
    tcp dport { 22, 80, 443 } accept
    jump managed_open
  }
}
EOF
}

stage_firewall() {
  mkdir -p /etc/nftables.d
  firewall_ruleset >/etc/nftables.d/kwerft.nft
  if ! grep -q 'include "/etc/nftables.d/\*.nft"' /etc/nftables.conf 2>/dev/null; then
    printf '\ninclude "/etc/nftables.d/*.nft"\n' >>/etc/nftables.conf
  fi
  nft -f /etc/nftables.d/kwerft.nft
  systemctl enable nftables >>"$LOG_FILE" 2>&1
  local paused=""
  [[ -e "$FIREWALL_STATE_DIR/paused" ]] && paused=" · console rules paused (--reset-firewall)"
  echo "nftables: 22, 80, 443 public$([[ -n "$PRIVATE_CIDR" ]] && echo " · cluster ports on $PRIVATE_IFACE only")$paused"
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
  mkdir -p "$(dirname "$K3S_CONFIG_FILE")"
  cat >"$K3S_CONFIG_FILE" <<EOF
# Managed by Kwerft installer.
cluster-init: true
node-ip: $node_ip
node-external-ip: $PUBLIC_IP
advertise-address: $node_ip
tls-san:
${DOMAIN:+  - $DOMAIN
}  - $PUBLIC_IP
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
kubelet-arg:
  - max-pods=200
$(k3s_cloud_provider_config)
$(node_settings)
EOF
}

# With the hcloud cloud-controller-manager (hcloud.cloudControllerManager,
# first install only) k3s's own cloud controller is off and every kubelet
# registers with --cloud-provider=external: nodes wait (tainted) until the
# CCM gives them their hcloud://<id> provider ID, addresses and zone. k3s
# itself sets the kubelet flag only while its own controller runs, so it is
# explicit here and in joined nodes' configs. Prints the lines to append
# to kubelet-arg, nothing otherwise.
k3s_cloud_provider_config() {
  [[ "$HCLOUD_CCM" == "true" ]] || return 0
  printf '  - cloud-provider=external\ndisable-cloud-controller: true\n'
}

# ccm_active: this node's k3s runs without its own cloud controller, i.e.
# with the hcloud CCM. Decided once, at the first install: switching later
# would need every node to register again (the provider ID cannot change).
ccm_active() {
  grep -qx 'disable-cloud-controller: true' "$K3S_CONFIG_FILE" 2>/dev/null
}

# nodes_initialized: no node waits for a cloud-controller-manager any more.
nodes_initialized() {
  local tainted
  tainted=$(kc get nodes -o jsonpath='{range .items[*]}{.spec.taints[?(@.key=="node.cloudprovider.kubernetes.io/uninitialized")].key}{end}' 2>/dev/null) || return 1
  [[ -z "$tainted" ]]
}

stage_kubernetes() {
  local version; version=$(k3s_target)
  write_k3s_config
  write_registry_mirror >/dev/null   # before k3s first starts: no restart needed
  write_etcd_snapshot_config "$ETCD_SNAPSHOT_SCHEDULE_DEFAULT" "$ETCD_SNAPSHOT_RETENTION_DEFAULT" >/dev/null   # likewise
  curl -fsSL https://get.k3s.io \
    | INSTALL_K3S_VERSION="$version" INSTALL_K3S_SKIP_ENABLE=false sh -s - server >>"$LOG_FILE" 2>&1 \
    || die $EXIT_K8S "k3s installation failed"
  retry 60 2 kc get --raw /readyz >/dev/null 2>&1 || die $EXIT_K8S "Kubernetes API did not become ready"
  echo "k3s $version · server · embedded etcd · secrets encryption on"
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

# restart_k3s [file] applies a changed registries.yaml (or the file named).
# Restarting k3s leaves running containers alone (the unit's
# KillMode=process); the API is back in seconds. Prints the unit it
# restarted, nothing when k3s is not running yet.
restart_k3s() {
  local unit changed=${1:-$REGISTRIES_FILE}
  for unit in k3s k3s-agent; do
    systemctl is-active --quiet "$unit" || continue
    systemctl restart "$unit" >>"$LOG_FILE" 2>&1 || die $EXIT_K8S "Could not restart $unit to apply $changed"
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
# Stage: upgrades
# Rancher's system-upgrade-controller (namespace system-upgrade, CRD
# plans.upgrade.cattle.io) at its pin: the console's Kubernetes upgrades
# write its Plans, which move k3s node by node, joined nodes included. And
# the node label kwerft.dev/installer=true on this server, where
# /var/lib/kwerft lives: the console's upgrade runner is scheduled there.
# ---------------------------------------------------------------------------
suc_manifest_url() {
  echo "https://github.com/rancher/system-upgrade-controller/releases/download/$SYSTEM_UPGRADE_CONTROLLER_VERSION/$1"
}

stage_upgrades() {
  kc apply --server-side --force-conflicts -f "$(suc_manifest_url crd.yaml)" >>"$LOG_FILE" 2>&1 \
    || die $EXIT_PLATFORM "system-upgrade-controller's CRDs failed to install"
  kc wait --for=condition=Established crd/plans.upgrade.cattle.io --timeout=60s >>"$LOG_FILE" 2>&1 \
    || die $EXIT_PLATFORM "The CRD plans.upgrade.cattle.io was not established"
  kc apply --server-side --force-conflicts -f "$(suc_manifest_url system-upgrade-controller.yaml)" >>"$LOG_FILE" 2>&1 \
    || die $EXIT_PLATFORM "system-upgrade-controller failed to install"
  local node state="starts with the network"
  node=$(label_installer_node)
  # On a first install the node has no network yet (Cilium is next), so its
  # pod cannot start now; on every later run it must be ready.
  if node_ready "$node"; then
    kc -n system-upgrade rollout status deployment/system-upgrade-controller --timeout=5m >>"$LOG_FILE" 2>&1 \
      || die $EXIT_PLATFORM "system-upgrade-controller did not become ready (kubectl -n system-upgrade get pods)"
    state="ready"
  fi
  echo "system-upgrade-controller $SYSTEM_UPGRADE_CONTROLLER_VERSION ($state) · installer node $node"
}

node_ready() {
  [[ "$(kc get node "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)" == "True" ]]
}

# installer_node prints this server's node: named after the host (k3s's
# default), else the node with this server's address.
installer_node() {
  local name ip=${PRIVATE_IP:-$PUBLIC_IP}
  name=$(hostname | tr '[:upper:]' '[:lower:]')
  if kc get node "$name" >/dev/null 2>&1; then echo "$name"; return 0; fi
  [[ -n "$ip" ]] || return 0
  kc get nodes -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}' 2>/dev/null \
    | awk -v ip="$ip" '$2 == ip {print $1; exit}' || true
}

# label_installer_node labels this server's node, and no other, as the
# installer's; prints its name.
label_installer_node() {
  local node other
  node=$(installer_node)
  [[ -n "$node" ]] || die $EXIT_K8S "Could not find this server's node to label it $INSTALLER_NODE_LABEL=true"
  kc label node "$node" "$INSTALLER_NODE_LABEL=true" --overwrite >>"$LOG_FILE" 2>&1 \
    || die $EXIT_K8S "Could not label node $node $INSTALLER_NODE_LABEL=true"
  for other in $(kc get nodes -l "$INSTALLER_NODE_LABEL=true" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null || true); do
    [[ "$other" == "$node" ]] && continue
    kc label node "$other" "$INSTALLER_NODE_LABEL-" >>"$LOG_FILE" 2>&1 || true
  done
  echo "$node"
}

# ---------------------------------------------------------------------------
# Stage: networking (Cilium)
# ---------------------------------------------------------------------------
stage_network() {
  local api_ip=${PRIVATE_IP:-$PUBLIC_IP} hubble wait_args=(--wait)
  hubble=$(hubble_enabled)
  # On a first install with the hcloud CCM the node keeps its
  # "uninitialized" taint until the CCM (stage Hetzner Cloud, next) runs, and
  # Hubble's relay cannot be scheduled before: do not wait for it here.
  if ccm_active && ! nodes_initialized; then wait_args=(); fi
  helmk repo add cilium https://helm.cilium.io --force-update >>"$LOG_FILE" 2>&1
  # The console reads flows from the relay over plain gRPC (Cilium's default
  # for the relay's own server, pinned here; relay ↔ agents stay mTLS). The
  # Kwerft chart admits only the console and the nodes to the relay.
  helmk upgrade --install cilium cilium/cilium --version "$CILIUM_VERSION" \
    --namespace kube-system ${wait_args[@]+"${wait_args[@]}"} --timeout 10m \
    --set kubeProxyReplacement=true \
    --set k8sServiceHost="$api_ip" --set k8sServicePort=6443 \
    --set ipam.mode=kubernetes \
    --set operator.replicas=1 \
    --set encryption.enabled=true --set encryption.type=wireguard \
    --set encryption.wireguard.persistentKeepalive=25s \
    --set hubble.enabled="$hubble" --set hubble.relay.enabled="$hubble" \
    --set hubble.relay.tls.server.enabled=false \
    --set bpf.masquerade=true \
    >>"$LOG_FILE" 2>&1 || die $EXIT_PLATFORM "Cilium installation failed"
  retry 90 2 kc wait --for=condition=Ready nodes --all --timeout=5s >/dev/null 2>&1 \
    || die $EXIT_K8S "Node did not become Ready after installing Cilium"
  echo "Cilium $CILIUM_VERSION · kube-proxy replacement · WireGuard$([[ $hubble == true ]] && echo ' · Hubble')"
}

# ---------------------------------------------------------------------------
# Stage: Hetzner Cloud (Cloud servers only)
# The CSI driver (storage class hcloud-volumes, not the default: local-path
# stays it) whenever a Cloud API token is known — from --config
# hcloud.tokenFile or stored in Settings › Hetzner Cloud API — and the
# cloud-controller-manager when the cluster was installed with it. Both read
# the token from kube-system/hcloud, which this stage writes and labels as
# Kwerft's, so the console can hand them a token changed in Settings.
# ---------------------------------------------------------------------------
stage_hcloud() {
  if [[ "$PLATFORM" != "cloud" ]]; then
    echo "not a Hetzner Cloud server: local volumes only"
    return 0
  fi
  if [[ "$HCLOUD_CCM" == "true" ]] && ! ccm_active; then
    warn "hcloud.cloudControllerManager only takes effect when a cluster is first installed (nodes would have to register again); this cluster keeps k3s's own cloud controller."
  fi
  local token
  token=$(mktemp "$HCLOUD_TMP_DIR/hcloud-token.XXXXXX")
  if [[ -n "$HCLOUD_TOKEN_FILE" ]]; then
    tr -d '[:space:]' <"$HCLOUD_TOKEN_FILE" >"$token"
  elif [[ "$MODE" == "agent" ]]; then
    console_hcloud_token >"$token"
  else
    stored_hcloud_token >"$token"
  fi
  if [[ ! -s "$token" ]]; then
    rm -f "$token"
    ccm_active && die $EXIT_PLATFORM "The hcloud cloud-controller-manager needs the Cloud API token: pass --config with hcloud.tokenFile."
    if [[ "$MODE" == "agent" ]]; then
      if (( AWAIT_CLOUD_TOKEN )); then
        echo "no Cloud API token yet: the console hands it over once this cluster's agent connects"
      else
        echo "no Cloud API token: Cloud Volumes off (Hetzner Cloud clusters get the console's; re-run once it has)"
      fi
    else
      echo "no Cloud API token: Cloud Volumes off (store one under Settings › Hetzner Cloud API, then re-run)"
    fi
    return 0
  fi
  if ccm_active && [[ -n "$PRIVATE_IP" && -z "$HCLOUD_NETWORK_ID" ]]; then
    rm -f "$token"
    die $EXIT_PLATFORM "Could not find the Cloud Network of $PRIVATE_IP in the metadata service; the cloud-controller-manager needs it."
  fi
  write_hcloud_secret "$token"
  rm -f "$token"
  helmk repo add hcloud https://charts.hetzner.cloud --force-update >>"$LOG_FILE" 2>&1
  local summary=""
  if ccm_active; then
    install_hcloud_ccm
    summary="cloud-controller-manager $HCLOUD_CCM_CHART_VERSION · "
  fi
  install_hcloud_csi
  echo "${summary}CSI $HCLOUD_CSI_CHART_VERSION · storage class hcloud-volumes"
}

# stored_hcloud_token prints the token saved in Settings, if any.
stored_hcloud_token() {
  kc -n kwerft-system get secret kwerft-hcloud-token -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null | tr -d '[:space:]' || true
}

# console_hcloud_token prints the token the console handed this cluster (a
# hetzner-cloud cluster in agent mode) in kube-system/hcloud, if any.
console_hcloud_token() {
  [[ "$(kc -n kube-system get secret hcloud -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}' 2>/dev/null || true)" == "kwerft" ]] || return 0
  kc -n kube-system get secret hcloud -o jsonpath='{.data.token}' 2>/dev/null | base64 -d 2>/dev/null | tr -d '[:space:]' || true
}

# await_cloud_volumes (agent mode, --await-cloud-token): the console hands a
# Hetzner Cloud cluster its Cloud API token once the agent connects; then the
# CSI driver can be installed. Prints the summary's addition; never fails
# the install (a re-run adds the driver later).
await_cloud_volumes() {
  (( AWAIT_CLOUD_TOKEN )) && [[ "$PLATFORM" == "cloud" ]] || return 0
  helmk -n kube-system status hcloud-csi >/dev/null 2>&1 && return 0
  local waited=0
  until [[ -n "$(console_hcloud_token)" ]]; do
    if (( waited >= CLOUD_TOKEN_WAIT )); then
      warn "The console has not handed over its Hetzner Cloud token within $((CLOUD_TOKEN_WAIT / 60)) min; re-run this command later for Cloud Volumes."
      return 0
    fi
    sleep 5
    waited=$((waited + 5))
  done
  helmk repo add hcloud https://charts.hetzner.cloud --force-update >>"$LOG_FILE" 2>&1
  install_hcloud_csi
  printf ' · Cloud Volumes (CSI %s)' "$HCLOUD_CSI_CHART_VERSION"
}

# write_hcloud_secret <token file> keeps kube-system/hcloud (token, and the
# Cloud Network for the CCM) — unless an operator's own Secret of that name
# is there, which is left alone.
write_hcloud_secret() {
  local owner
  if kc -n kube-system get secret hcloud >/dev/null 2>&1; then
    owner=$(kc -n kube-system get secret hcloud -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}' 2>/dev/null || true)
    if [[ "$owner" != "kwerft" ]]; then
      warn "kube-system/hcloud was not created by Kwerft; the CSI driver and CCM use it as it is."
      return 0
    fi
  fi
  local args=(--from-file=token="$1")
  [[ -n "$HCLOUD_NETWORK_ID" ]] && args+=(--from-literal=network="$HCLOUD_NETWORK_ID")
  kc -n kube-system create secret generic hcloud "${args[@]}" --dry-run=client -o yaml \
    | kc label --local -f - app.kubernetes.io/managed-by=kwerft -o yaml \
    | kc apply -f - >>"$LOG_FILE" 2>&1 || die $EXIT_PLATFORM "Could not store the Cloud API token for the CSI driver"
}

# The CCM's values: with a Cloud Network, nodes get their private address as
# InternalIP (it must match the kubelet's --node-ip, the private one) and
# Load Balancers may target it; its routes controller stays off — Cilium
# tunnels pod traffic and needs no routes in the Cloud Network.
hcloud_ccm_values() {
  local networking=false
  [[ -n "$HCLOUD_NETWORK_ID" ]] && networking=true
  cat <<EOF
networking:
  enabled: $networking
  clusterCIDR: $POD_CIDR
env:
  HCLOUD_NETWORK_ROUTES_ENABLED:
    value: "false"
EOF
}

install_hcloud_ccm() {
  mkdir -p "$VALUES_DIR"
  hcloud_ccm_values >"$VALUES_DIR/hcloud-ccm.yaml"
  helmk upgrade --install hcloud-cloud-controller-manager hcloud/hcloud-cloud-controller-manager --version "$HCLOUD_CCM_CHART_VERSION" \
    --namespace kube-system --wait --timeout 10m -f "$VALUES_DIR/hcloud-ccm.yaml" \
    >>"$LOG_FILE" 2>&1 || die $EXIT_PLATFORM "hcloud cloud-controller-manager installation failed"
  retry 60 5 nodes_initialized \
    || die $EXIT_K8S "The hcloud cloud-controller-manager did not initialize the nodes (kubectl -n kube-system logs deploy/hcloud-cloud-controller-manager)"
}

# The CSI driver's values: no StorageClass from the chart (hcloud_storage_class
# below is Kwerft's), and its pods only where the Cloud metadata service
# answers: not on dedicated servers (label kwerft.dev/platform).
hcloud_csi_values() {
  cat <<'EOF'
storageClasses: []
controller:
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - key: kwerft.dev/platform
                operator: NotIn
                values: [dedicated]
node:
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
          - matchExpressions:
              - key: kwerft.dev/platform
                operator: NotIn
                values: [dedicated]
EOF
}

# hcloud_storage_class: Cloud Volumes for Volumes of class hcloud-volume. Not
# the default class. allowedTopologies names the CSI driver's own topology
# key, which only nodes running its node plugin carry, so a pod with a Cloud
# Volume is never placed on a dedicated server in a mixed cluster.
hcloud_storage_class() {
  local locations list=()
  read -r -a list <<<"$HCLOUD_LOCATIONS"
  locations=$(printf '%s, ' "${list[@]}")
  cat <<EOF
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: hcloud-volumes
  labels:
    app.kubernetes.io/managed-by: kwerft
  annotations:
    storageclass.kubernetes.io/is-default-class: "false"
provisioner: csi.hetzner.cloud
volumeBindingMode: WaitForFirstConsumer
allowVolumeExpansion: true
reclaimPolicy: Delete
allowedTopologies:
  - matchLabelExpressions:
      - key: csi.hetzner.cloud/location
        values: [${locations%, }]
EOF
}

install_hcloud_csi() {
  mkdir -p "$VALUES_DIR"
  hcloud_csi_values >"$VALUES_DIR/hcloud-csi.yaml"
  helmk upgrade --install hcloud-csi hcloud/hcloud-csi --version "$HCLOUD_CSI_CHART_VERSION" \
    --namespace kube-system --wait --timeout 10m -f "$VALUES_DIR/hcloud-csi.yaml" \
    >>"$LOG_FILE" 2>&1 || die $EXIT_PLATFORM "hcloud CSI driver installation failed"
  local owner
  owner=$(kc get storageclass hcloud-volumes -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}' 2>/dev/null || true)
  if kc get storageclass hcloud-volumes >/dev/null 2>&1 && [[ "$owner" != "kwerft" ]]; then
    warn "The storage class hcloud-volumes was not created by Kwerft; it is left as it is."
    return 0
  fi
  # A StorageClass cannot change; an older one of Kwerft's is replaced
  # (volumes made from it keep working: the class is only read to create them).
  if ! hcloud_storage_class | kc apply -f - >>"$LOG_FILE" 2>&1; then
    kc delete storageclass hcloud-volumes >>"$LOG_FILE" 2>&1 || true
    hcloud_storage_class | kc apply -f - >>"$LOG_FILE" 2>&1 || die $EXIT_PLATFORM "Could not create the storage class hcloud-volumes"
  fi
}

# apply_hcloud_settings stores --config hcloud.tokenFile as the console's
# Cloud API token (the Secret only owners and admins may write, nobody read)
# and hcloud.loadBalancer in ConsoleSettings.spec.hetznerCloud.
apply_hcloud_settings() {
  if [[ -n "$HCLOUD_TOKEN_FILE" ]]; then
    local token sum
    token=$(mktemp "$HCLOUD_TMP_DIR/hcloud-token.XXXXXX")
    tr -d '[:space:]' <"$HCLOUD_TOKEN_FILE" >"$token"
    if ! kc -n kwerft-system create secret generic kwerft-hcloud-token --from-file=token="$token" \
      --dry-run=client -o yaml | kc apply -f - >>"$LOG_FILE" 2>&1; then
      rm -f "$token"
      die $EXIT_KWERFT "Could not store the Cloud API token from $HCLOUD_TOKEN_FILE"
    fi
    sum=$(sha256sum "$token" | awk '{print $1}')
    rm -f "$token"
    # A changed token makes the console sync the Cloud Firewall at once.
    if [[ "$sum" != "$(cat "$HCLOUD_TOKEN_SUM_FILE" 2>/dev/null || true)" ]]; then
      kc annotate consolesettings.kwerft.dev kwerft --overwrite "kwerft.dev/hcloud-token-updated-at=$(date -u +%FT%TZ)" \
        >>"$LOG_FILE" 2>&1 || die $EXIT_KWERFT "Could not save the console settings"
      printf '%s\n' "$sum" >"$HCLOUD_TOKEN_SUM_FILE"
    fi
  fi
  if [[ -n "$HCLOUD_LB" ]]; then
    kc patch consolesettings.kwerft.dev kwerft --type merge -p "{\"spec\":{\"hetznerCloud\":{\"loadBalancer\":{\"enabled\":$HCLOUD_LB}}}}" \
      >>"$LOG_FILE" 2>&1 || die $EXIT_KWERFT "Could not save the Load Balancer setting"
  fi
  return 0
}

# hubble_enabled prints whether Hubble (flows, relay) runs: not with --lite.
# Cilium and the Kwerft chart (traffic counts, dropped connections) follow it.
hubble_enabled() {
  if (( LITE )); then echo false; else echo true; fi
}

# Pods in namespaces labelled kwerft.dev/system may reach every app (each App's
# CiliumNetworkPolicy allows them): ingress, monitoring and the console itself.
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
  local proxy_args=()
  read -r -a proxy_args <<<"$(traefik_proxy_args)"
  helmk upgrade --install traefik traefik/traefik --version "$TRAEFIK_CHART_VERSION" \
    --namespace traefik --create-namespace --wait --timeout 10m \
    -f "$VALUES_DIR/traefik.yaml" ${proxy_args[@]+"${proxy_args[@]}"} \
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

# traefik_proxy_args: on a Cloud server in a Cloud Network, Traefik accepts
# the PROXY protocol from that network, which is where a Hetzner Load
# Balancer in front of it (Settings › Hetzner Cloud API) connects from, so
# apps and the console keep seeing client addresses. Connections without the
# header (the nodes themselves) work as before. Prints helm flags.
traefik_proxy_args() {
  [[ -n "$HCLOUD_NETWORK_RANGE" ]] || return 0
  printf '%s ' --set "ports.web.proxyProtocol.trustedIPs={$HCLOUD_NETWORK_RANGE}" \
    --set "ports.websecure.proxyProtocol.trustedIPs={$HCLOUD_NETWORK_RANGE}"
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
# Stage: backups (docs/phase6.md)
# k3s's own etcd snapshots, locally and to the backup bucket, and Velero for
# backups of projects and of the console itself: file-system volume backups
# with Kopia (encrypted with the recovery key), S3-compatible storage through
# the AWS plugin. Velero starts without a BackupStorageLocation: the console
# creates it from Settings › Backups. --lite leaves Velero out.
# ---------------------------------------------------------------------------
stage_backups() {
  local etcd
  etcd=$(ensure_etcd_snapshots)
  if (( LITE )); then
    echo "Velero off (--lite) · $etcd"
    return 0
  fi
  install_velero
  echo "Velero $VELERO_VERSION · volume backups with Kopia · $etcd"
}

# etcd_snapshot_config <schedule> <retention> prints the k3s config drop-in.
# k3s reads Secret kube-system/kwerft-etcd-s3 (etcd-s3-config-secret) at
# every snapshot, not when it starts: while the Secret is missing (no backup
# target in Settings yet) k3s logs a warning and goes on taking and pruning
# local snapshots; once the console writes it, the next snapshot goes to the
# bucket as well, without a restart. No other etcd-s3-* option may be set,
# or k3s ignores the Secret. The schedule is read when k3s starts.
etcd_snapshot_config() {
  cat <<EOF
$ETCD_MARKER
# k3s reads this file when it starts; the installer restarts k3s when it changes.
etcd-snapshot-schedule-cron: "$1"
etcd-snapshot-retention: $2
etcd-snapshot-compress: true
etcd-s3: true
etcd-s3-config-secret: $ETCD_S3_SECRET
EOF
}

# write_etcd_snapshot_config <schedule> <retention> brings the drop-in up to
# date and prints "changed", "unchanged", or "none" where k3s runs no
# embedded etcd of Kwerft's making (only the first server has cluster-init).
write_etcd_snapshot_config() {
  grep -qx 'cluster-init: true' "$K3S_CONFIG_FILE" 2>/dev/null || { echo none; return 0; }
  local want
  want=$(etcd_snapshot_config "$1" "$2")
  if [[ -f "$K3S_ETCD_CONFIG_FILE" && "$(<"$K3S_ETCD_CONFIG_FILE")" == "$want" ]]; then echo unchanged; return 0; fi
  mkdir -p "$(dirname "$K3S_ETCD_CONFIG_FILE")"
  (umask 077; printf '%s\n' "$want" >"$K3S_ETCD_CONFIG_FILE.kwerft-new")
  mv -f "$K3S_ETCD_CONFIG_FILE.kwerft-new" "$K3S_ETCD_CONFIG_FILE"
  echo changed
}

# valid_cron: five cron fields (or a descriptor such as @daily), nothing
# YAML or k3s would read differently.
valid_cron() {
  [[ "$1" =~ ^@(yearly|annually|monthly|weekly|daily|midnight|hourly)$ \
    || "$1" =~ ^[0-9A-Za-z*/,?-]+( [0-9A-Za-z*/,?-]+){4}$ ]]
}

# etcd_snapshot_settings prints "<schedule>|<retention>": Settings ›
# Backups (ConsoleSettings spec.backups.etcdSnapshots), else the defaults.
etcd_snapshot_settings() {
  local schedule retention
  schedule=$(cluster_setting '{.spec.backups.etcdSnapshots.schedule}')
  retention=$(cluster_setting '{.spec.backups.etcdSnapshots.retention}')
  if [[ -n "$schedule" ]] && ! valid_cron "$schedule"; then
    warn "The etcd snapshot schedule '$schedule' (Settings › Backups) is not a cron schedule; using '$ETCD_SNAPSHOT_SCHEDULE_DEFAULT'."
    schedule=""
  fi
  if [[ -n "$retention" ]] && { [[ ! "$retention" =~ ^[1-9][0-9]{0,2}$ ]] || (( retention > 500 )); }; then
    warn "The etcd snapshot retention '$retention' (Settings › Backups) is not between 1 and 500; keeping $ETCD_SNAPSHOT_RETENTION_DEFAULT."
    retention=""
  fi
  printf '%s|%s\n' "${schedule:-$ETCD_SNAPSHOT_SCHEDULE_DEFAULT}" "${retention:-$ETCD_SNAPSHOT_RETENTION_DEFAULT}"
}

# ensure_etcd_snapshots writes the drop-in and restarts k3s when it changed
# (a schedule changed in Settings takes effect with the next run). Prints the
# summary's part.
ensure_etcd_snapshots() {
  local settings schedule retention state restarted=""
  settings=$(etcd_snapshot_settings)
  schedule=${settings%|*}
  retention=${settings##*|}
  state=$(write_etcd_snapshot_config "$schedule" "$retention")
  case "$state" in
    none) echo "etcd snapshots as k3s has them (no embedded etcd set up by Kwerft)"; return 0 ;;
    changed) restarted=$(restart_k3s "$K3S_ETCD_CONFIG_FILE") ;;
  esac
  echo "etcd snapshots ($schedule, $retention kept)${restarted:+ · $restarted restarted}"
}

# velero_values: no BackupStorageLocation, VolumeSnapshotLocation or cloud
# credentials (the console's location carries its own), the node agent on
# every node (build pools are tainted) for file-system backups, and modest
# requests for small servers.
velero_values() {
  cat <<EOF
image:
  repository: docker.io/velero/velero
  tag: $VELERO_VERSION
initContainers:
  - name: velero-plugin-for-aws
    image: docker.io/velero/velero-plugin-for-aws:$VELERO_PLUGIN_AWS_VERSION
    imagePullPolicy: IfNotPresent
    volumeMounts:
      - mountPath: /target
        name: plugins
resources:
  requests: {cpu: 50m, memory: 128Mi}
  limits: {memory: 512Mi}
upgradeJobResources:
  requests: {cpu: 50m, memory: 64Mi}
  limits: {memory: 256Mi}
credentials:
  useSecret: false
snapshotsEnabled: false
deployNodeAgent: true
nodeAgent:
  resources:
    requests: {cpu: 50m, memory: 128Mi}
    limits: {memory: 1Gi}
  tolerations:
    - operator: Exists
configuration:
  uploaderType: kopia
  defaultVolumesToFsBackup: true
  defaultBackupStorageLocation: $BSL_NAME
  backupStorageLocation: []
  volumeSnapshotLocation: []
  repositoryMaintenanceJob:
    repositoryConfigData:
      name: velero-repo-maintenance
      global:
        keepLatestMaintenanceJobs: 3
        podResources:
          cpuRequest: 50m
          memoryRequest: 128Mi
          memoryLimit: 1Gi
EOF
}

install_velero() {
  mkdir -p "$VALUES_DIR"
  velero_values >"$VALUES_DIR/velero.yaml"
  helmk repo add vmware-tanzu https://vmware-tanzu.github.io/helm-charts --force-update >>"$LOG_FILE" 2>&1
  helmk upgrade --install velero vmware-tanzu/velero --version "$VELERO_CHART_VERSION" \
    --namespace "$VELERO_NS" --create-namespace --wait --timeout 10m \
    -f "$VALUES_DIR/velero.yaml" \
    >>"$LOG_FILE" 2>&1 || die $EXIT_PLATFORM "Velero installation failed"
}

# ---------------------------------------------------------------------------
# Stage: Restore (--restore; docs/phase6.md › Full restore onto a new server)
# The stages before have built an empty cluster with Velero. This one points
# Velero at the bucket (read-only while it restores), restores a Cluster
# backup — every project, kwerft-system with the console's database volume,
# data key, tokens and certificates, kwerft-builds, the cluster-wide
# kwerft.dev objects — and marks the database for the console to swap in
# the backup hook's consistent copy. The Kwerft stage then upgrades the
# restored Helm release in place.
# ---------------------------------------------------------------------------
# Never restored: what the stages before have just set up, and what only
# makes sense on the server it was taken on.
readonly RESTORE_EXCLUDED_NAMESPACES="kube-system kube-public kube-node-lease velero cert-manager traefik kwerft-observability"
readonly RESTORE_EXCLUDED_RESOURCES="nodes events events.events.k8s.io leases.coordination.k8s.io endpoints endpointslices.discovery.k8s.io jobs.batch customresourcedefinitions.apiextensions.k8s.io storageclasses.storage.k8s.io csidrivers.storage.k8s.io csinodes.storage.k8s.io volumeattachments.storage.k8s.io apiservices.apiregistration.k8s.io mutatingwebhookconfigurations.admissionregistration.k8s.io validatingwebhookconfigurations.admissionregistration.k8s.io ciliumendpoints.cilium.io ciliumendpointslices.cilium.io ciliumidentities.cilium.io ciliumnodes.cilium.io certificaterequests.cert-manager.io orders.acme.cert-manager.io challenges.acme.cert-manager.io"
# Restored with their status: kinds whose empty status would start work
# again (a build, a one-off task, an upgrade, a project restore).
readonly RESTORE_WITH_STATUS="builds.kwerft.dev tasks.kwerft.dev upgrades.kwerft.dev restores.kwerft.dev"

stage_restore() {
  apply_kwerft_crds
  write_restore_secrets
  restore_bsl ReadOnly | kc apply -f - >>"$LOG_FILE" 2>&1 \
    || die $EXIT_RESTORE "Could not create the backup storage location $BSL_NAME in namespace $VELERO_NS"
  wait_backup_sync
  local backup name phase items
  backup=$(pick_backup)
  name=$(restore_name "$backup")
  start_restore "$backup" "$name"
  phase=$(wait_restore "$name")
  fix_restored_objects
  mark_database_restore "$backup"
  # From now on the console keeps the location (Settings › Backups) and
  # backs up into the same folder.
  kc -n "$VELERO_NS" patch backupstoragelocations.velero.io "$BSL_NAME" --type merge -p '{"spec":{"accessMode":"ReadWrite"}}' \
    >>"$LOG_FILE" 2>&1 || die $EXIT_RESTORE "Could not open the backup storage location $BSL_NAME for new backups"
  items=$(restore_field "$name" '{.status.progress.itemsRestored}')
  echo "backup $backup · ${items:-all} objects$([[ "$phase" == PartiallyFailed ]] && echo ' (with errors, see above)') · console database from the backup"
}

# apply_kwerft_crds: the restored kwerft.dev objects need their CRDs, which
# the Kwerft stage would apply only after the restore.
apply_kwerft_crds() {
  local ref version_args=()
  ref=$(chart_ref)
  [[ "$ref" == oci://* ]] && version_args=(--version "$KWERFT_VERSION")
  helmk show crds "$ref" ${version_args[@]+"${version_args[@]}"} 2>>"$LOG_FILE" \
    | kc apply --server-side --force-conflicts -f - >>"$LOG_FILE" 2>&1 \
    || die $EXIT_KWERFT "Kwerft CRDs failed to apply (chart: $ref)"
}

# write_restore_secrets writes, as the console does from Settings ›
# Backups: velero/velero-repo-credentials (repository-password: the
# recovery key, the Kopia repository's password) and
# velero/kwerft-bsl-credentials (cloud: an AWS credentials file with the
# access keys). The keys pass through a 0700 directory, never arguments or
# the log.
write_restore_secrets() {
  local tmp ok=1
  tmp=$(mktemp -d "$HCLOUD_TMP_DIR/restore.XXXXXX")
  printf '[default]\naws_access_key_id=%s\naws_secret_access_key=%s\n' \
    "$(tr -d '[:space:]' <"$BACKUP_ACCESS_KEY_FILE")" "$(tr -d '[:space:]' <"$BACKUP_SECRET_KEY_FILE")" >"$tmp/cloud"
  recovery_key <"$BACKUP_RECOVERY_KEY_FILE" >"$tmp/password"
  kc -n "$VELERO_NS" create secret generic velero-repo-credentials --from-file=repository-password="$tmp/password" \
    --dry-run=client -o yaml | kc apply -f - >>"$LOG_FILE" 2>&1 || ok=0
  if (( ok )); then
    kc -n "$VELERO_NS" create secret generic kwerft-bsl-credentials --from-file=cloud="$tmp/cloud" \
      --dry-run=client -o yaml | kc apply -f - >>"$LOG_FILE" 2>&1 || ok=0
  fi
  rm -rf "$tmp"
  (( ok )) || die $EXIT_RESTORE "Could not store the backup credentials in namespace $VELERO_NS"
}

# restore_bsl <accessMode> prints the BackupStorageLocation "kwerft" for the
# bucket in --config, as the console keeps it: <prefix>/velero.
restore_bsl() {
  cat <<EOF
apiVersion: velero.io/v1
kind: BackupStorageLocation
metadata:
  name: $BSL_NAME
  namespace: $VELERO_NS
spec:
  provider: aws
  default: true
  accessMode: $1
  objectStorage:
    bucket: "$BACKUP_BUCKET"
    prefix: "$BACKUP_PREFIX/velero"
  credential:
    name: kwerft-bsl-credentials
    key: cloud
  config:
    region: "$BACKUP_REGION"
    s3Url: "$BACKUP_ENDPOINT"
    s3ForcePathStyle: "true"
    checksumAlgorithm: ""
EOF
}

bsl_field() {
  kc -n "$VELERO_NS" get backupstoragelocations.velero.io "$BSL_NAME" -o jsonpath="$1" 2>/dev/null || true
}

restore_field() {
  kc -n "$VELERO_NS" get restores.velero.io "$1" -o jsonpath="$2" 2>/dev/null || true
}

bucket_url() {
  printf 's3://%s/%s/velero at %s' "$BACKUP_BUCKET" "$BACKUP_PREFIX" "$BACKUP_ENDPOINT"
}

# wait_backup_sync waits until Velero reaches the bucket (the location is
# Available) and has listed its backups once (lastSyncedTime).
wait_backup_sync() {
  local waited=0 step phase synced message
  step=$(( RESTORE_POLL > 0 ? RESTORE_POLL : 1 ))
  while :; do
    phase=$(bsl_field '{.status.phase}')
    synced=$(bsl_field '{.status.lastSyncedTime}')
    if [[ "$phase" == Available && -n "$synced" ]]; then return 0; fi
    (( waited < RESTORE_SYNC_TIMEOUT )) || break
    sleep "$RESTORE_POLL"
    waited=$((waited + step))
  done
  if [[ "$phase" == Available ]]; then
    die $EXIT_RESTORE "Velero reached $(bucket_url) but has not listed its backups within $((RESTORE_SYNC_TIMEOUT / 60)) min; re-run to wait longer."
  fi
  message=$(bsl_field '{.status.message}')
  die $EXIT_RESTORE "Cannot read the backups in $(bucket_url): ${message:-the location is ${phase:-not checked yet}}." \
    "Check backups.endpoint, region, bucket, prefix and the access keys in $CONFIG_FILE."
}

# pick_backup prints the backup to restore: the one named by --restore, or
# the newest Completed backup labelled kwerft.dev/backup-scope=Cluster.
pick_backup() {
  local list backup phase scope count
  if [[ "$RESTORE_FROM" != latest ]]; then
    phase=$(kc -n "$VELERO_NS" get backups.velero.io "$RESTORE_FROM" -o jsonpath='{.status.phase}' 2>/dev/null) \
      || die $EXIT_RESTORE "There is no backup named $RESTORE_FROM in $(bucket_url)."
    scope=$(kc -n "$VELERO_NS" get backups.velero.io "$RESTORE_FROM" -o jsonpath='{.metadata.labels.kwerft\.dev/backup-scope}' 2>/dev/null || true)
    [[ "$scope" == Cluster ]] \
      || die $EXIT_RESTORE "Backup $RESTORE_FROM is not a backup of the whole cluster (scope: ${scope:-none}); --restore needs a Cluster backup."
    case "$phase" in
      Completed) ;;
      PartiallyFailed) warn "Backup $RESTORE_FROM is incomplete (some objects or volumes failed); restoring what it holds." ;;
      *) die $EXIT_RESTORE "Backup $RESTORE_FROM did not complete (${phase:-no status}); name another one or use --restore latest." ;;
    esac
    echo "$RESTORE_FROM"
    return 0
  fi
  list=$(kc -n "$VELERO_NS" get backups.velero.io -l kwerft.dev/backup-scope=Cluster \
    -o jsonpath='{range .items[?(@.status.phase=="Completed")]}{.status.completionTimestamp}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null || true)
  backup=$(sort <<<"$list" | awk 'NF == 2 {name = $2} END {print name}')
  if [[ -z "$backup" ]]; then
    count=$(kc -n "$VELERO_NS" get backups.velero.io -o name 2>/dev/null | grep -c . || true)
    die $EXIT_RESTORE "There is no complete Cluster backup in $(bucket_url) (${count:-0} backups of any kind there)." \
      "Check backups.prefix, or name a backup with --restore <name>."
  fi
  echo "$backup"
}

# restore_name: one Velero Restore per backup, so a re-run after a failure
# waits for the same restore instead of starting another.
restore_name() {
  printf 'restore-%s' "$1" | cut -c1-253
}

# yaml_list "a b" prints ["a", "b"].
yaml_list() {
  local items=() item out=""
  read -r -a items <<<"$1"
  for item in "${items[@]}"; do out+="\"$item\", "; done
  printf '[%s]' "${out%, }"
}

# restore_manifest <backup> <name>: everything in the backup except
# RESTORE_EXCLUDED_*; an object that exists already (the platform the
# stages before set up) is never overwritten.
restore_manifest() {
  cat <<EOF
apiVersion: velero.io/v1
kind: Restore
metadata:
  name: $2
  namespace: $VELERO_NS
  labels:
    app.kubernetes.io/managed-by: kwerft
    kwerft.dev/restore: installer
spec:
  backupName: $1
  includedNamespaces: ["*"]
  excludedNamespaces: $(yaml_list "$RESTORE_EXCLUDED_NAMESPACES")
  excludedResources: $(yaml_list "$RESTORE_EXCLUDED_RESOURCES")
  includeClusterResources: true
  existingResourcePolicy: none
  restorePVs: true
  restoreStatus:
    includedResources: $(yaml_list "$RESTORE_WITH_STATUS")
EOF
}

start_restore() {
  if kc -n "$VELERO_NS" get restores.velero.io "$2" >/dev/null 2>&1; then
    log "Restore $2 exists already; waiting for it"
    return 0
  fi
  restore_manifest "$1" "$2" | kc create -f - >>"$LOG_FILE" 2>&1 \
    || die $EXIT_RESTORE "Could not start restoring backup $1"
}

# wait_restore <name> waits for the restore to end and prints its phase.
# Volume data can take a while: a line on stderr every minute shows that
# it moves.
wait_restore() {
  local name=$1 waited=0 said=0 step phase done_items total
  step=$(( RESTORE_POLL > 0 ? RESTORE_POLL : 1 ))
  while :; do
    phase=$(restore_field "$name" '{.status.phase}')
    case "$phase" in Completed|PartiallyFailed|Failed|FailedValidation) break ;; esac
    restore_cloud_volumes
    (( waited < RESTORE_TIMEOUT )) \
      || die $EXIT_RESTORE "The restore $name did not finish within $((RESTORE_TIMEOUT / 60)) min (${phase:-New}). Follow it with: kubectl -n $VELERO_NS get restore $name -o yaml"
    if (( waited - said >= 60 )); then
      done_items=$(restore_field "$name" '{.status.progress.itemsRestored}')
      total=$(restore_field "$name" '{.status.progress.totalItems}')
      printf '  %s… restoring: %s of %s objects, %d min%s\n' "$C_DIM" "${done_items:-0}" "${total:-?}" $((waited / 60)) "$C_0" >&2
      log "Restore $name: $phase ${done_items:-0}/${total:-?}"
      said=$waited
    fi
    sleep "$RESTORE_POLL"
    waited=$((waited + step))
  done
  case "$phase" in
    Failed)
      die $EXIT_RESTORE "Restoring backup into $name failed: $(restore_field "$name" '{.status.failureReason}')." \
        "Details: kubectl -n $VELERO_NS get restore $name -o yaml; delete that restore to try again." ;;
    FailedValidation)
      die $EXIT_RESTORE "Velero refused the restore $name: $(restore_field "$name" '{.status.validationErrors}')." ;;
    PartiallyFailed)
      warn "The restore finished with $(restore_field "$name" '{.status.errors}') errors: some objects or volumes did not come back." \
        "Details: kubectl -n $VELERO_NS get restore $name -o yaml; kubectl -n $VELERO_NS get podvolumerestores -l velero.io/restore-name=$name" ;;
  esac
  echo "$phase"
}

# restore_cloud_volumes: Cloud Volumes in the backup need the CSI driver,
# which the Hetzner Cloud stage installs only with a Cloud API token.
# Without hcloud.tokenFile in --config the token comes back with
# kwerft-system during the restore; once it is there, install the driver so
# those volumes can be created.
restore_cloud_volumes() {
  [[ "$PLATFORM" == cloud ]] || return 0
  helmk -n kube-system status hcloud-csi >/dev/null 2>&1 && return 0
  [[ -n "$(stored_hcloud_token)" ]] || return 0
  log "Restore: the Cloud API token is back; installing the CSI driver for Cloud Volumes"
  stage_hcloud >>"$LOG_FILE"
}

# fix_restored_objects prepares what Velero brought back for the Kwerft
# stage's Helm upgrade (Helm adopts the restored objects: their Helm labels,
# annotations and release history came back with them):
#  - Velero gives restored Services new cluster IPs. The registry's Service
#    has a fixed one (chart: registry.clusterIP) that cannot change in
#    place, so it goes and the chart creates it again.
#  - A Helm operation in flight when the backup was taken left a pending
#    revision, and Helm refuses to upgrade a pending release: it goes, and
#    the last deployed revision is the current one again.
fix_restored_objects() {
  local ip
  ip=$(kc -n kwerft-system get service kwerft-registry -o jsonpath='{.spec.clusterIP}' 2>/dev/null || true)
  if [[ -n "$ip" && "$ip" != "$REGISTRY_CLUSTER_IP" ]]; then
    kc -n kwerft-system delete service kwerft-registry >>"$LOG_FILE" 2>&1 \
      || die $EXIT_RESTORE "Could not remove the restored registry Service (it needs its fixed address $REGISTRY_CLUSTER_IP)"
  fi
  kc -n kwerft-system delete secret -l 'owner=helm,name=kwerft,status in (pending-install,pending-upgrade,pending-rollback)' \
    --ignore-not-found >>"$LOG_FILE" 2>&1 || true
}

# mark_database_restore leaves the marker backup/RESTORE in the restored
# database volume: the console, on its next start, replaces the volume's
# live database files (copied while it was writing them) with the
# consistent copy the backup hook made (cmd/kwerft/dbsnapshot.go). The
# console is stopped meanwhile, and the volume is handed to the console's
# user, in case the file-system restore brought files back as root.
mark_database_restore() {
  local backup=$1 pv dir
  pv=$(kc -n kwerft-system get pvc kwerft-data -o jsonpath='{.spec.volumeName}' 2>/dev/null || true)
  [[ -n "$pv" ]] \
    || die $EXIT_RESTORE "Backup $backup holds no console database volume (kwerft-system/kwerft-data); it is not a backup of a Kwerft console."
  dir=$(kc get pv "$pv" -o jsonpath='{.spec.hostPath.path}{.spec.local.path}' 2>/dev/null || true)
  [[ -n "$dir" && -d "$dir" ]] \
    || die $EXIT_RESTORE "The console's database volume $pv is not a directory on this server (${dir:-no local path}); --restore needs it on local storage (the default, local-path)."
  [[ -s "$dir/backup/kwerft.db" ]] \
    || die $EXIT_RESTORE "Backup $backup has no copy of the console's database (backup/kwerft.db): its backup hook did not run. Name a newer backup."
  kc -n kwerft-system scale deployment kwerft --replicas=0 >>"$LOG_FILE" 2>&1 \
    || die $EXIT_RESTORE "Could not stop the restored console to swap in its database"
  retry 90 2 console_stopped || die $EXIT_RESTORE "The restored console did not stop within 3 min"
  printf 'restored from backup %s at %s\n' "$backup" "$(date -u +%FT%TZ)" >"$dir/backup/RESTORE"
  chown -R "$CONSOLE_UID:$CONSOLE_UID" "$dir"
  kc -n kwerft-system scale deployment kwerft --replicas=1 >>"$LOG_FILE" 2>&1 \
    || die $EXIT_RESTORE "Could not start the restored console"
}

console_stopped() {
  local pods
  pods=$(kc -n kwerft-system get pods -l app.kubernetes.io/name=kwerft,app.kubernetes.io/instance=kwerft -o name 2>/dev/null) || return 1
  [[ -z "$pods" ]]
}

# restore_refused: --restore is for a new server. A server whose Kwerft
# came from a restore may run the same command again (it resumes).
restore_refused() {
  ! stage_done restore && { stage_done kwerft || stage_done handoff || stage_done kwerft-agent; }
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

  local ref version_args=() image_args=() acme_args=()
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
  [[ -n "$ACME_SERVER" ]] && acme_args=(--set acme.server="$ACME_SERVER")
  # Shown in the required firewall rule cluster-private (Network → Server firewall).
  local firewall_args=(--set firewall.privateNetwork="")
  [[ -n "$PRIVATE_NETWORK" ]] && firewall_args=(--set firewall.privateNetwork="$PRIVATE_NETWORK")
  # Settings › Hetzner Cloud API: whether the CCM runs, and where a Load
  # Balancer may connect from (Traefik's PROXY protocol).
  local hcloud_args=(--set hcloud.ccm=false --set hcloud.proxyNetwork="$HCLOUD_NETWORK_RANGE")
  ccm_active && hcloud_args[1]=hcloud.ccm=true
  # Helm installs a chart's CRDs only on first install, never on upgrade, so
  # apply them on every run (server-side; Helm no longer touches them).
  helmk show crds "$ref" ${version_args[@]+"${version_args[@]}"} 2>>"$LOG_FILE" \
    | kc apply --server-side --force-conflicts -f - >>"$LOG_FILE" 2>&1 \
    || die $EXIT_KWERFT "Kwerft CRDs failed to apply (chart: $ref)"
  apply_console_settings
  write_join_secret
  apply_hcloud_settings
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
    --set hubble.enabled="$(hubble_enabled)" \
    "${image_args[@]}" "${firewall_args[@]}" "${hcloud_args[@]}" ${acme_args[@]+"${acme_args[@]}"} \
    --set registry.image.tag="$ZOT_VERSION" \
    --set registry.clusterIP="$REGISTRY_CLUSTER_IP" \
    --set builds.buildkitImage="docker.io/moby/buildkit:${BUILDKIT_VERSION}-rootless" \
    --set builds.railpackImage="ghcr.io/railwayapp/railpack-frontend:${RAILPACK_VERSION}" \
    >>"$LOG_FILE" 2>&1 || die $EXIT_KWERFT "Kwerft installation failed (chart: $ref)"
  printf '%s\n' "$DOMAIN" >"$DOMAIN_FILE"   # fallback; the cluster setting is the record
  echo "control plane ${IMAGE:-$KWERFT_VERSION} ready · registry zot $ZOT_VERSION at $REGISTRY_CLUSTER_IP:5000"
}

# ---------------------------------------------------------------------------
# Agent mode: Kwerft without its console, managed from another one
# (docs/phase5.md). The chart runs `kwerft agent`: the reconcilers, and a
# tunnel that dials the console with the cluster's token and carries the
# console's requests to this cluster's API server, which stays private.
# ---------------------------------------------------------------------------
stage_kwerft_agent() {
  kc create namespace kwerft-system --dry-run=client -o yaml | kc apply -f - >/dev/null
  mark_system_namespace kwerft-system

  local ref version_args=() image_args=()
  ref=$(chart_ref)
  [[ "$ref" == oci://* ]] && version_args=(--version "$KWERFT_VERSION")
  if [[ -n "$IMAGE" ]]; then
    if [[ -n "$IMAGE_ARCHIVE" ]]; then
      k3s ctr --namespace k8s.io images import "$IMAGE_ARCHIVE" >>"$LOG_FILE" 2>&1 \
        || die $EXIT_KWERFT "Could not import $IMAGE_ARCHIVE into k3s"
    fi
    image_args=(--set image.repository="${IMAGE%:*}" --set image.tag="${IMAGE##*:}" --set image.pullPolicy=IfNotPresent)
  else
    image_args=(--set image.tag="$KWERFT_VERSION")
  fi
  local firewall_args=(--set firewall.privateNetwork="")
  [[ -n "$PRIVATE_NETWORK" ]] && firewall_args=(--set firewall.privateNetwork="$PRIVATE_NETWORK")
  # Where a Hetzner Load Balancer in front of this cluster's ingress may
  # connect from (the console's settings for this cluster turn it on).
  local hcloud_args=(--set hcloud.ccm=false --set hcloud.proxyNetwork="$HCLOUD_NETWORK_RANGE")
  ccm_active && hcloud_args[1]=hcloud.ccm=true
  helmk show crds "$ref" ${version_args[@]+"${version_args[@]}"} 2>>"$LOG_FILE" \
    | kc apply --server-side --force-conflicts -f - >>"$LOG_FILE" 2>&1 \
    || die $EXIT_KWERFT "Kwerft CRDs failed to apply (chart: $ref)"
  adopt_namespace kwerft-builds
  local taken
  taken=$(registry_address_taken)
  [[ -z "$taken" ]] || die $EXIT_KWERFT "The registry's fixed address $REGISTRY_CLUSTER_IP is taken by Service $taken." \
    "Delete or re-create that Service (it gets a new address) and re-run the installer."
  write_agent_secret
  write_join_secret
  helmk upgrade --install kwerft "$ref" ${version_args[@]+"${version_args[@]}"} \
    --namespace kwerft-system --wait --timeout 10m --skip-crds \
    --set mode=agent \
    --set agent.consoleURL="$CONSOLE_URL" \
    --set acme.email="$ACME_EMAIL" \
    --set platform="$PLATFORM" \
    --set hubble.enabled="$(hubble_enabled)" \
    "${image_args[@]}" "${firewall_args[@]}" "${hcloud_args[@]}" \
    --set registry.image.tag="$ZOT_VERSION" \
    --set registry.clusterIP="$REGISTRY_CLUSTER_IP" \
    --set builds.buildkitImage="docker.io/moby/buildkit:${BUILDKIT_VERSION}-rootless" \
    --set builds.railpackImage="ghcr.io/railwayapp/railpack-frontend:${RAILPACK_VERSION}" \
    >>"$LOG_FILE" 2>&1 || die $EXIT_KWERFT "Kwerft installation failed (chart: $ref)"
  local volumes
  volumes=$(await_cloud_volumes)
  echo "agent ${IMAGE:-$KWERFT_VERSION} · cluster $(cluster_of_token "$CLUSTER_TOKEN") → $CONSOLE_URL${volumes}"
}

# write_agent_secret stores the agent token where the chart mounts it
# (agent.tokenSecret). From stdin, so the token is not in kubectl's
# arguments; the agent rereads it before every connection, so a rotated
# token (re-run with the new command) needs no restart.
write_agent_secret() {
  printf '%s' "$CLUSTER_TOKEN" \
    | kc -n kwerft-system create secret generic kwerft-agent --from-file=token=/dev/stdin --dry-run=client -o yaml \
    | kc apply -f - >>"$LOG_FILE" 2>&1 || die $EXIT_KWERFT "Could not store the agent token"
}

# write_join_secret records how new nodes join this cluster: Secret
# cluster-local-join (keys server, token) in kwerft-system. In a remote
# cluster the agent hands it to the console (which keeps it as
# cluster-<name>-join); in the console's own cluster it is the join material
# of the Cluster "local". Only on a k3s server, which holds the join token.
write_join_secret() {
  local token_file=/var/lib/rancher/k3s/server/token server
  [[ -r "$token_file" ]] || return 0
  server="https://${PRIVATE_IP:-$PUBLIC_IP}:6443"
  kc -n kwerft-system create secret generic cluster-local-join \
    --from-literal=server="$server" --from-file=token="$token_file" --dry-run=client -o yaml \
    | kc apply -f - >>"$LOG_FILE" 2>&1 || die $EXIT_KWERFT "Could not store the cluster's join material"
}

print_agent_summary() {
  local secs=$(( $(date +%s) - START_TS ))
  echo
  printf '  Console     %s%s%s\n' "$C_ACC" "$CONSOLE_URL" "$C_0"
  printf '  This cluster (%s) appears under Clusters there once its agent connects, within a minute.\n' "$(cluster_of_token "$CLUSTER_TOKEN")"
  printf '  Log         %s\n\n' "$LOG_FILE"
  printf '%sDone in %dm %02ds.%s Re-run it any time to repair; the installer of a newer release upgrades Kwerft (Kubernetes: Settings › Updates).\n' "$C_OK" $((secs / 60)) $((secs % 60)) "$C_0"
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
    restored)
      state="accounts from the backup" ;;
  esac
  echo "DNS ${resolved:-unresolved} · $state"
}

# setup_token_state prints where first-run setup stands:
#   config    owner comes from --config, no token needed
#   missing   no token yet (first install, or the file was lost)
#   pending   a valid token is waiting to be used
#   expired   the token ran out before setup was done
#   complete  Kwerft consumed the token (it deletes the Secret once the owner exists)
#   restored  --restore: the users came back with the console's database
setup_token_state() {
  if [[ -n "$RESTORE_FROM" ]]; then echo restored; return; fi
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
  if [[ -n "$RESTORE_FROM" ]]; then
    printf '  Open        %shttps://%s%s\n' "$C_ACC" "$DOMAIN" "$C_0"
    printf '  Sign in with your accounts: users, settings and projects came back from the backup\n'
    if [[ "$(cluster_setting '{.spec.dns.manageRecords}')" == "true" ]]; then
      printf '  DNS         Kwerft points its records at this server (%s) by itself\n' "$PUBLIC_IP"
    else
      printf '  DNS         point %s and the apps hostnames at this server: %s\n' "$DOMAIN" "$PUBLIC_IP"
    fi
  elif [[ -n "$CONFIG_FILE" ]]; then
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
  printf '%sDone in %dm %02ds.%s Re-run it any time to repair; the installer of a newer release upgrades Kwerft (Kubernetes: Settings › Updates).\n' "$C_OK" $((secs / 60)) $((secs % 60)) "$C_0"
}

# ---------------------------------------------------------------------------
# Join mode: k3s agent (or additional server) joining an existing cluster
# ---------------------------------------------------------------------------
# TODO(phase-5): when nodes have no shared private network, the console must open
# 6443/tcp, 10250/tcp and the Cilium ports on existing nodes for the joiner's
# public IP before it connects. Today joining assumes a Cloud Network or vSwitch.
stage_join() {
  local info server k3s_token node
  # The console binds tokens of servers it created to their hostname.
  node=$(hostname)
  info=$(curl -fsS --max-time 20 -H "Authorization: Bearer $JOIN_TOKEN" \
    "${JOIN_URL%/}/api/v1/join?role=$JOIN_ROLE&node=$node") \
    || die $EXIT_NETWORK "Could not reach $JOIN_URL or the join token was rejected"
  server=$(jq -r .server <<<"$info"); k3s_token=$(jq -r .token <<<"$info")
  # A cluster with the hcloud CCM registers every kubelet with it.
  [[ "$(jq -r '.cloudProvider // empty' <<<"$info")" == "external" ]] && HCLOUD_CCM=true
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
    node_settings
    if [[ "$HCLOUD_CCM" == "true" ]]; then echo "kubelet-arg: [cloud-provider=external]"; fi
  } >/etc/rancher/k3s/config.yaml
  chmod 0600 /etc/rancher/k3s/config.yaml
  if [[ "$kind" == "server" ]]; then
    # Control-plane joiners need the same cluster-wide settings as the first server.
    write_k3s_config
    sed -i '/^cluster-init:/d' /etc/rancher/k3s/config.yaml
    printf 'server: %s\ntoken: %s\n' "$server" "$k3s_token" >>/etc/rancher/k3s/config.yaml
  fi
  write_registry_mirror >/dev/null   # before k3s first starts, as on the first server
  local version; version=$(k3s_target)
  curl -fsSL https://get.k3s.io | INSTALL_K3S_VERSION="$version" sh -s - "$kind" >>"$LOG_FILE" 2>&1 \
    || die $EXIT_K8S "k3s $kind installation failed"
  echo "k3s $version $kind joined $server"
}

# ---------------------------------------------------------------------------
# Maintenance
# ---------------------------------------------------------------------------
do_reset_firewall() {
  [[ $EUID -eq 0 ]] || die $EXIT_PREFLIGHT "Run as root (sudo)."
  reset_firewall
  say "Kwerft host firewall removed, and the console's firewall rules are paused on this node."
  say "Run the installer again to restore the baseline. To let the console manage this node's"
  say "firewall again, delete $FIREWALL_STATE_DIR/paused."
}

# reset_firewall removes Kwerft's table and pauses the node agent here: it
# must not put back console rules (say, an SSH narrowing that locked the
# operator out) once this rescue ran.
reset_firewall() {
  nft delete table inet kwerft 2>/dev/null || true
  rm -f /etc/nftables.d/kwerft.nft "$STATE_DIR/stages/firewall.done"
  mkdir -p "$FIREWALL_STATE_DIR"
  : >"$FIREWALL_STATE_DIR/paused"
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
  trap 'progress_exit $?' EXIT
  parse_args "$@"
  progress_start
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
  detect_hcloud_network
  private_network
  [[ "$MODE" == "join" || "$MODE" == "agent" ]] || resolve_domain
  # A server keeps the mode it was installed in: a console is not turned
  # into an agent (or back) by re-running with other flags.
  if (( ! DRY_RUN )); then
    if [[ "$MODE" == "agent" ]] && stage_done handoff; then
      die $EXIT_USAGE "This server runs a Kwerft console; --agent is for a cluster without one."
    fi
    if [[ "$MODE" == "install" ]] && stage_done kwerft-agent; then
      die $EXIT_USAGE "This server runs Kwerft in agent mode (managed from a console). Re-run the console's command with --agent."
    fi
    if [[ -n "$RESTORE_FROM" ]] && restore_refused; then
      die $EXIT_USAGE "This server already runs Kwerft; --restore is for a new server. (Back up and restore single projects in the console.)"
    fi
  fi

  printf '%s▸ Kwerft installer %s%s  %schannel=%s · platform=%s · mode=%s%s\n\n' \
    "$C_ACC$C_B" "$KWERFT_VERSION" "$C_0" "$C_DIM" "$CHANNEL" "$PLATFORM" "$MODE" "$C_0"

  if [[ -n "$RESTORE_FROM" ]]; then
    say "Restoring backup $RESTORE_FROM from $(bucket_url)$( (( DOMAIN_EXPLICIT )) || echo '; the console hostname comes from the backup')."
    echo
  elif [[ "$MODE" == "install" ]] && is_temp_domain; then
    warn "No --domain given: using temporary hostname $DOMAIN (fine for trying Kwerft, not for production)."
    echo
  fi
  if [[ "$MODE" != "join" && "$ACME_SERVER" == "$ACME_STAGING_URL" ]]; then
    warn "Certificates come from Let's Encrypt staging: browsers do not trust them (for tests)."
    echo
  elif [[ "$MODE" != "join" && -n "$ACME_SERVER" ]]; then
    say "Certificates come from the ACME directory $ACME_SERVER."
    echo
  fi

  run_stage preflight "Preflight" stage_preflight force
  remember_settings
  run_stage system    "System"    stage_system force
  run_stage firewall  "Firewall"  stage_firewall force

  if [[ "$MODE" == "join" ]]; then
    run_stage join "Join cluster" stage_join
    run_stage registry "Registry mirror" stage_registry_mirror force
    printf '\n%sThis node will appear in the console within a minute.%s\n' "$C_OK" "$C_0"
    return
  fi

  run_stage kubernetes    "Kubernetes"    stage_kubernetes
  run_stage registry      "Registry mirror" stage_registry_mirror force
  run_stage helm          "Helm"          stage_helm force
  run_stage upgrades      "Upgrades"      stage_upgrades force
  run_stage network      "Network"       stage_network force
  run_stage hcloud        "Hetzner Cloud" stage_hcloud force
  run_stage ingress       "Ingress & TLS" stage_ingress_tls force
  run_stage observability "Observability" stage_observability force
  if [[ "$MODE" == "agent" ]]; then
    run_stage kwerft-agent "Kwerft agent" stage_kwerft_agent force
    (( DRY_RUN )) || print_agent_summary
    return
  fi
  run_stage backups       "Backups"       stage_backups force
  if [[ -n "$RESTORE_FROM" ]]; then
    run_stage restore     "Restore"       stage_restore
    # The console hostname is the restored setting unless --domain says otherwise.
    if (( ! DRY_RUN && ! DOMAIN_EXPLICIT )); then DOMAIN=""; resolve_domain; fi
  fi
  run_stage kwerft         "Kwerft"         stage_kwerft force
  run_stage handoff       "Handoff"       stage_handoff force

  (( DRY_RUN )) || print_summary
}

# Tests source this file with KWERFT_SOURCED=1 to call individual functions.
if [[ "${KWERFT_SOURCED:-0}" != "1" ]]; then
  main "$@"
fi
