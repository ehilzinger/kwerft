#!/usr/bin/env bash
# Phase 7 spike, option B: k3s inside a persistent `container machine`,
# installed by the real install/install.sh. The machine mounts the Mac home
# directory at the same path, so this checkout is visible inside it.
#
#   hack/spike-mac/machine.sh up       build the image, create the machine, run install.sh
#   hack/spike-mac/machine.sh demo     deploy a Project + App and test it from the Mac
#   hack/spike-mac/machine.sh restart  stop and start the machine, see what survives
#   hack/spike-mac/machine.sh down     delete the machine

OPTION=b
# shellcheck source=hack/spike-mac/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/common.sh"

readonly MACHINE="kwerft-k3s"
readonly BASE_IMAGE="kwerft-spike-ubuntu:24.04"
readonly DOMAIN="console.kwerft.test"
# `container machine run` joins its arguments and runs them through a shell
# again, so quoting is lost: pass anything non-trivial as a script file (the
# Mac home directory is mounted at the same path inside the machine).
m()  { container machine run -n "$MACHINE" --root -- "$@"; }
# kubectl runs inside the machine: install.sh's firewall opens 6443 only to the
# private network and pods, and the API certificate names the install-time IP.
k()  { printf '%q ' k3s kubectl "$@" >"$WORK/k.sh"; m bash "$WORK/k.sh"; }

base_image() {
  container build -t "$BASE_IMAGE" -f "$SPIKE_DIR/machine/Containerfile" "$SPIKE_DIR/machine" >"$WORK/machine-build.log" 2>&1 \
    || { tail -20 "$WORK/machine-build.log"; die "Base image build failed"; }
}

# KERNEL=path/to/vmlinux picks a kernel for this machine only (see kernel/).
create_machine() {
  container machine create "$BASE_IMAGE" --name "$MACHINE" --cpus 6 --memory 8G \
    ${KERNEL:+--kernel "$KERNEL"}
  # `create` returns before systemd is up; the first `run` races the boot.
  local _
  for _ in $(seq 60); do
    m systemctl is-system-running 2>/dev/null | grep -qE '^(running|degraded)$' && return 0
    sleep 1
  done
  die "Machine did not finish booting within a minute"
}

# Makes the VM look like a stock server to install.sh, which is kept
# unmodified for the spike: the kernel has overlay, br_netfilter and wireguard
# built in with no /lib/modules (modprobe fails), and no swap support
# (`swapoff -a` exits 16). Both belong in the installer as real fixes.
prepare_machine() {
  m bash "$SPIKE_DIR/machine/prepare.sh"
}

kwerft_archive() {
  container image save -o "$WORK/kwerft-image.tar" "$KWERFT_IMAGE"
}

install() {
  # Before d47ecf0, install.sh's Handoff died when the console domain did not
  # resolve (the spike ref 9db312e predates the fix), so give it a hosts entry.
  printf 'grep -q kwerft.test /etc/hosts || echo "10.255.255.1 %s whoami.kwerft.test" >>/etc/hosts\n' "$DOMAIN" >"$WORK/hosts.sh"
  m sh "$WORK/hosts.sh"
  # --platform dedicated: no Hetzner metadata service here. No --domain DNS
  # exists for kwerft.test, so the console certificate stays pending.
  # --private-iface kwerft0: the stable address from prepare.sh, so k3s and
  # etcd survive the machine's eth0 address changing on restart.
  m "$SRC/install/install.sh" --yes --platform dedicated --domain "$DOMAIN" --private-iface kwerft0 \
    --image "$KWERFT_IMAGE" --image-archive "$WORK/kwerft-image.tar"
}

# eth0's current address: what the Mac uses to reach Traefik and the API.
machine_ip() {
  container machine ls --format json | jq -r --arg n "$MACHINE" '.[] | select(.id == $n) | .ipAddress'
}

up() {
  : >"$WORK/timings-$OPTION.txt"
  check_egress
  timed "base-image" base_image
  timed "create-machine" create_machine
  prepare_machine
  timed "image-archive" kwerft_archive
  timed "install-sh" install
  ok "Machine IP $(machine_ip)"
}

# install.sh always configures Let's Encrypt, which cannot issue for
# *.kwerft.test; switch Kwerft to the local CA (see kind.sh for the same step).
local_ca() {
  k apply -f "$SPIKE_DIR/local-ca.yaml" >/dev/null
  k wait --for=condition=Ready clusterissuer/kwerft-local --timeout=2m >/dev/null
  local i
  i=$(k -n kwerft-system get deploy kwerft -o json | jq '.spec.template.spec.containers[0].args | map(startswith("--cluster-issuer=")) | index(true)')
  k -n kwerft-system patch deploy kwerft --type=json \
    -p "[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args/$i\",\"value\":\"--cluster-issuer=kwerft-local\"}]" >/dev/null
  k -n kwerft-system rollout status deploy/kwerft --timeout=3m >/dev/null
}

demo() {
  local_ca
  k apply -f "$SPIKE_DIR/demo.yaml"   # path is the same inside the machine
  k -n spike wait --for=condition=Ready app/whoami --timeout=3m || k -n spike describe app whoami
  local ip; ip=$(machine_ip)
  say "HTTPS from the Mac to whoami.kwerft.test via $ip"
  k get secret -n cert-manager kwerft-local-ca -o 'jsonpath={.data.ca\.crt}' | base64 -d >"$WORK/kwerft-local-ca-b.crt"
  local _
  for _ in $(seq 40); do
    curl -s --cacert "$WORK/kwerft-local-ca-b.crt" --max-time 5 --resolve "whoami.kwerft.test:443:$ip" \
      "https://whoami.kwerft.test/" -o "$WORK/whoami-b.txt" && break
    sleep 3
  done
  head -4 "$WORK/whoami-b.txt"
  say "Console via $ip"
  curl -s --cacert "$WORK/kwerft-local-ca-b.crt" --max-time 5 -o /dev/null -w '%{http_code}\n' --resolve "$DOMAIN:443:$ip" "https://$DOMAIN/healthz"
}

restart() {
  container machine stop "$MACHINE"
  m true
  k wait --for=condition=Ready nodes --all --timeout=5m && k get pods -A
}

down() { container machine delete "$MACHINE"; }

case "${1:-}" in
  up|demo|restart|down) "$1" ;;
  *) die "Usage: $0 up|demo|restart|down" ;;
esac
