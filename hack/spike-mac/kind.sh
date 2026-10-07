#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Enzo Hilzinger
# SPDX-License-Identifier: AGPL-3.0-only

# Phase 7 spike, option A: Kwerft on `container k8s` (kind node image in an
# Apple container VM), with Cilium as the CNI and the same platform charts the
# installer uses.
#
#   hack/spike-mac/kind.sh up       create the cluster and install everything
#   hack/spike-mac/kind.sh platform storage, Gateway API, cert-manager, Traefik (part of up)
#   hack/spike-mac/kind.sh kwerft   load the image and install the chart (part of up)
#   hack/spike-mac/kind.sh demo     deploy a Project + App and test it from the Mac
#   hack/spike-mac/kind.sh restart  stop and start the node, see what survives
#   hack/spike-mac/kind.sh down     delete the cluster

OPTION=a
# shellcheck source=hack/spike-mac/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

readonly CLUSTER="kwerft-spike"
readonly CTX="$CLUSTER"
readonly DOMAIN="kwerft.test"
readonly LOCAL_PATH_VERSION="v0.0.37"   # 2026-08-05
k()     { kubectl --context "$CTX" "$@"; }
helmk() { helm --kube-context "$CTX" "$@"; }

render_cilium() {
  helm repo add cilium https://helm.cilium.io --force-update >/dev/null
  # As close to the installer as one node allows: kube-proxy replacement and BPF
  # masquerading, no WireGuard. The agent runs on the host network, so the API
  # server is always at 127.0.0.1 even though the node IP changes.
  helm template cilium cilium/cilium --version "$CILIUM_VERSION" --namespace kube-system \
    --set ipam.mode=kubernetes --set operator.replicas=1 \
    --set kubeProxyReplacement=true --set k8sServiceHost=127.0.0.1 --set k8sServicePort=6443 \
    --set bpf.masquerade=true --set hubble.enabled=false >"$WORK/cilium.yaml"
}

create_cluster() {
  container k8s create --name "$CLUSTER" --cpus 6 --memory 8g --cni "$WORK/cilium.yaml"
  k wait --for=condition=Ready nodes --all --timeout=5m
}

# Cilium needs xt_socket: without it its iptables rules fail as a whole, host
# traffic (kubelet probes, Traefik on the host network) is classified as world,
# and Kwerft's default-deny policies drop it. Container's default Kata kernel
# lacks it; Apple's containerization kernel config has it.
check_kernel() {
  container exec "$CLUSTER" sh -c 'zcat /proc/config.gz | grep -q "^CONFIG_NETFILTER_XT_MATCH_SOCKET=y"' \
    || die "The default kernel lacks xt_socket; build apple/containerization/kernel and run: container system kernel set --binary <vmlinux>"
  ok "Kernel $(container exec "$CLUSTER" uname -r) has xt_socket"
}

# Apple's kind node has no StorageClass at all. k3s names its provisioner's
# class local-path, which is what the App reconciler maps local-nvme to.
local_path() {
  k apply -f "https://raw.githubusercontent.com/rancher/local-path-provisioner/${LOCAL_PATH_VERSION}/deploy/local-path-storage.yaml" >/dev/null
  k annotate storageclass local-path storageclass.kubernetes.io/is-default-class=true --overwrite >/dev/null
}

gateway_api() {
  k apply --server-side -f "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml" >/dev/null
}

cert_manager() {
  helm repo add jetstack https://charts.jetstack.io --force-update >/dev/null
  helmk upgrade --install cert-manager jetstack/cert-manager --version "$CERT_MANAGER_VERSION" \
    --namespace cert-manager --create-namespace --wait --timeout 10m \
    --set crds.enabled=true \
    --set config.apiVersion=controller.config.cert-manager.io/v1alpha1 \
    --set config.kind=ControllerConfiguration \
    --set config.enableGatewayAPI=true >/dev/null
}

local_ca() {
  k apply -f "$SPIKE_DIR/local-ca.yaml" >/dev/null
  k wait --for=condition=Ready clusterissuer/kwerft-local --timeout=2m >/dev/null
}

traefik() {
  traefik_values >"$WORK/traefik.yaml"
  helm repo add traefik https://traefik.github.io/charts --force-update >/dev/null
  helmk upgrade --install traefik traefik/traefik --version "$TRAEFIK_CHART_VERSION" \
    --namespace traefik --create-namespace --wait --timeout 10m -f "$WORK/traefik.yaml" >/dev/null
  k label namespace traefik kwerft.dev/system=true --overwrite >/dev/null
}

kwerft() {
  container k8s load-image --name "$CLUSTER" "$KWERFT_IMAGE"
  k create namespace kwerft-system --dry-run=client -o yaml | k apply -f - >/dev/null
  k label namespace kwerft-system kwerft.dev/system=true --overwrite >/dev/null
  k apply --server-side --force-conflicts -f "$SRC/charts/kwerft/crds" >/dev/null
  # No ACME locally. Without any issuer the HTTPS listeners reference TLS
  # secrets nobody creates and Traefik rejects them, so point Kwerft at the
  # local CA. The chart cannot name an issuer other than its own letsencrypt
  # yet, hence the patch.
  helmk upgrade --install kwerft "$SRC/charts/kwerft" --namespace kwerft-system \
    --wait --timeout 10m --skip-crds \
    --set console.domain="console.$DOMAIN" --set acme.enabled=false --set platform=dedicated \
    --set image.repository="${KWERFT_IMAGE%:*}" --set image.tag="${KWERFT_IMAGE##*:}" \
    --set image.pullPolicy=Never >/dev/null
  local i
  i=$(k -n kwerft-system get deploy kwerft -o json | jq '.spec.template.spec.containers[0].args | index("--cluster-issuer=")')
  k -n kwerft-system patch deploy kwerft --type=json \
    -p "[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args/$i\",\"value\":\"--cluster-issuer=kwerft-local\"}]" >/dev/null
  k -n kwerft-system rollout status deploy/kwerft --timeout=3m >/dev/null
}

node_ip() {
  container ls --format json | jq -r --arg c "$CLUSTER" \
    '.[] | select(.id == $c) | .status.networks[0].ipv4Address' | cut -d/ -f1
}

up() {
  : >"$WORK/timings-$OPTION.txt"
  check_egress
  timed "build-image" build_kwerft_image
  timed "render-cilium" render_cilium
  timed "create-cluster" create_cluster
  check_kernel
  platform
  timed "kwerft" kwerft
  ok "Node IP $(node_ip) · context $CTX"
}

platform() {
  timed "local-path" local_path
  timed "gateway-api" gateway_api
  timed "cert-manager" cert_manager
  timed "local-ca" local_ca
  timed "traefik" traefik
}

demo() {
  k apply -f "$SPIKE_DIR/demo.yaml"
  k -n spike wait --for=condition=Ready app/whoami --timeout=3m || k -n spike describe app whoami
  local ip; ip=$(node_ip)
  say "HTTPS from the Mac to whoami.$DOMAIN via $ip"
  curl -sk --max-time 5 --resolve "whoami.$DOMAIN:443:$ip" "https://whoami.$DOMAIN/" | head -5
  say "Console via $ip"
  curl -sk --max-time 5 -o /dev/null -w '%{http_code}\n' --resolve "console.$DOMAIN:443:$ip" "https://console.$DOMAIN/healthz"
}

restart() {
  # There is no `container k8s stop`; the node is a plain container named after the cluster.
  say "Stopping $CLUSTER"; container stop "$CLUSTER"
  say "Starting $CLUSTER"; container start "$CLUSTER" || warn "start failed"
  k wait --for=condition=Ready nodes --all --timeout=5m && k get pods -A
}

down() { container k8s delete --name "$CLUSTER"; }

case "${1:-}" in
  up|platform|kwerft|demo|restart|down) "$1" ;;
  *) die "Usage: $0 up|platform|kwerft|demo|restart|down" ;;
esac
