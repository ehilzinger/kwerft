# Phase 7 spike: Kwerft on Apple `container`

Run on 2026-10-04 on an M-series Mac (15 cores, 24 GB, macOS 27.0) with
Apple `container` 1.5.0. Scripts: `hack/spike-mac/`. Everything is built from
a committed ref (`SPIKE_REF`, here `9db312e`) exported with `git archive`.

**Question:** which local cluster should Kwerft for Mac run on?

- **A — `container k8s`:** Apple's kind integration (`kindest/node` in one VM).
- **B — k3s in a `container machine`:** a persistent Ubuntu VM with systemd,
  set up by the unmodified `install/install.sh`.

## Result

**Recommendation: B, k3s in a container machine, set up by install.sh.**
Both options ran Kwerft end to end once the kernel was fixed: an App with a
health check behind Kwerft's default-deny policy, CA-verified HTTPS from the
Mac through Traefik and the Gateway, a volume, the console, and project
isolation. What decides it is restarts: a `container k8s` cluster cannot come
back after a stop (finding 4), while the machine came back on a new IP with
the API ready in 34 s, all pods running in about two minutes, and the
volume's data intact. B also runs the production stack unchanged, through the
same installer, so Kwerft for Mac stays one code path rather than a second
platform to test.

Both options need Kwerft's own kernel (finding 2). B can select it per
machine (`--kernel`); A only through `container`'s global default.

## Findings

### 1. A VPN on the Mac cuts containers off from the internet

With a VPN connected (`utun4`), containers reached the Mac (192.168.64.1) and
resolved DNS, but every connection beyond it timed out — even `ping 1.1.1.1`.
Image pulls still worked because the Mac pulls them, not the VM, so the
failure surfaced late and obscurely: `npm ci` timing out inside
`container build`, and the kind node never becoming Ready (it could not pull
Cilium). Disconnecting the VPN fixed it.

**For the app:** check egress from a throwaway container before anything else
and name the VPN as the likely cause, with split tunnelling for
`192.168.64.0/24` as the fix.

### 2. Neither available kernel runs Kwerft's network stack

| Kernel | `xt_socket` | legacy iptables | eBPF JIT | BTF | WireGuard |
|---|---|---|---|---|---|
| Kata 6.18.35-197-debug (`container`'s default) | missing | yes | yes | not checked | missing |
| apple/containerization 0.48.0 config, Linux 6.18.5 | yes | missing | missing | missing | yes |
| containerization config + `hack/spike-mac/kernel/kwerft.config` | yes | yes | yes | yes | yes |

Each gap stopped the spike once, in this order:

- **Default kernel:** Cilium 1.20 always installs an `iptables -m socket
  --transparent` rule. Without `xt_socket` the whole static rule set fails, so
  traffic from the host is never marked as host. Cilium then classifies
  kubelet probes and Traefik (host network) as `world`, and Kwerft's
  default-deny NetworkPolicies drop them: every App with a health check
  crash-loops and no public route works. No Cilium setting avoids it
  (`enableXTSocketFallback=false`, `l7Proxy=false` and kube-proxy replacement
  were all tried; `installIptablesRules` no longer exists in 1.20).
- **Apple's kernel config, legacy iptables:** has `xt_socket` and WireGuard,
  but Linux 6.18 only builds legacy xtables with `NETFILTER_XTABLES_LEGACY`.
  kind's node entrypoint and `container k8s`'s own node prep (`iptables -t
  mangle … -j TCPMSS`) use iptables-legacy, so cluster creation fails with
  `node prep failed`.
- **eBPF JIT:** Apple's config has `HAVE_EBPF_JIT` but not `BPF_JIT`; the
  Cilium agent exits with "Require support for the eBPF JIT".
- **BTF:** Cilium 1.20 loads its programs with CO-RE relocations and loops on
  "no BTF found for kernel version"; pods stay in ContainerCreating. BTF needs
  pahole ≥ 1.16 at build time, and Apple's Ubuntu 20.04 build image has 1.15.

`kwerft.config` adds these, the rest of Cilium's documented requirements and
`CONFIG_DUMMY` (finding 5). `hack/spike-mac/kernel/build.sh` builds it with
Apple's `make` and a native Ubuntu 24.04 image in under three minutes. It is
selected with `container system kernel set --binary …` (global) or
`container machine create --kernel …` (per machine).

**For the app:** it has to ship and select its own kernel. Building it is
cheap; owning its config and security updates is the real cost.

### 3. Apple's kind node has no StorageClass

Stock kind ships `standard`; `container k8s` ships none, so Kwerft's own
SQLite PVC would stay Pending. The spike installs
local-path-provisioner v0.0.37 as `local-path`, which is the class the App
reconciler already maps `local-nvme` to. With it, Kwerft's PVC and an App
with a volume (StatefulSet) bind.

### 4. A `container k8s` cluster does not survive a restart

The node gets a new IP on every start (192.168.64.7 → .9). kind's
entrypoint then regenerates the API certificate from `/kind/kubeadm.conf`,
which Apple's plugin never writes, and the control plane never comes back.
There is no `container k8s stop`/`start`; Apple's docs say to delete and
re-create. Volume data is lost with the cluster.

### 5. A container machine needs four adjustments before install.sh runs

The unmodified installer stops on things that are always true on a real
server. `hack/spike-mac/machine/prepare.sh` fixes them inside the VM; each one
becomes a real change for the local profile:

- **`modprobe` fails for built-in modules.** The kernel has overlay,
  br_netfilter and wireguard built in and ships no `/lib/modules`, so
  `modprobe overlay` exits 1 (System stage). Installer fix: treat a module as
  present when `/sys/module/<name>` or `/proc/filesystems` says so.
- **`swapoff -a` exits 16.** No swap support in the kernel, so `/proc/swaps`
  is empty and util-linux reports bad usage. Installer fix: skip when
  `/proc/swaps` is empty.
- **Mounts are private.** A machine is still a container underneath; kubelet
  refuses Cilium's `/sys/fs/bpf` mount (`not a shared mount`). Fix: a boot
  unit that runs `mount --make-rshared /` and mounts a shared bpffs.
- **The eth0 address changes on every start** (192.168.64.18 → .21). k3s pins
  etcd and the node IP to it, so after a restart etcd fails with `bind:
  cannot assign requested address`. Fix: a dummy interface with a stable
  private address (10.255.255.1) passed as `--private-iface kwerft0`; the
  installer already pins k3s to the private network.

Also: `container machine run` joins its arguments and runs them through a
shell again, so `sh -c '…'` runs only the first word. Pass scripts as files
(the Mac home directory is mounted at the same path).

Install.sh's firewall then works against the Mac: it opens 6443 only to the
private network and pods, and the API certificate names the install-time IP.
`kubectl` from the Mac needs either a firewall rule for the vmnet gateway or,
better, Kwerft's own authenticating API proxy (already planned).

### 6. What worked on the first try

- `container k8s create` with a Cilium manifest: node Ready in 88 s on the
  default kernel.
- The node IP is reachable from the Mac; Traefik on the host network answers
  on 80/443 there, so `--resolve host:443:<node-ip>` reaches Gateway routes.
- `container build` built the Kwerft image from the repository Dockerfile
  (BuildKit; the builder defaults to 2 CPUs / 2 GB). `container k8s
  load-image` imports it without a registry.
- The platform charts the installer uses (Gateway API CRDs, cert-manager,
  Traefik with the installer's own values) installed unchanged.
- A container machine looks like a fresh server to `install.sh`: systemd,
  Ubuntu 24.04, cgroup v2, a sparse 504 GB disk, the Mac home directory
  mounted at the same path.

## Comparison

Same Mac, same kernel (`hack/spike-mac/kernel/`), same Kwerft image
(`9db312e`), images already cached unless noted.

| | A — `container k8s` | B — k3s in a container machine |
|---|---|---|
| Kubernetes | kubeadm, `kindest/node` v1.35.5 | k3s v1.37.1, as in production |
| Set up by | `hack/spike-mac/kind.sh`: Cilium manifest, then the platform charts by hand | the unmodified `install/install.sh` after `prepare.sh` |
| Platform | Cilium (KPR), Traefik, cert-manager, local-path | everything the installer installs: Cilium with WireGuard and Hubble, Traefik, cert-manager, VictoriaMetrics/Logs |
| First setup | ~6 min (cluster 4 min incl. Cilium pulls, charts 1.5 min) | ~9 min (machine 14 s, installer 8.5 min incl. pulls; Cilium 3 min) |
| Memory in the VM | 1.7 GB (no observability) | 2.5 GB (with observability; the Hetzner server measured 2.4 GB) |
| StorageClass | none shipped; local-path added | local-path from k3s |
| Survives stop/start | **no** (finding 4) | **yes**: new IP, API in 34 s, pods in ~2 min, volume intact |
| Kernel selection | global default only | per machine (`--kernel`) |
| Reaching the cluster from the Mac | node IP; kubeconfig written by `container` | node IP for HTTP(S); API blocked by the installer's firewall (finding 5) |
| Status upstream | `container k8s` is marked EXPERIMENTAL | `container machine` is a 1.0 feature |

## Changes Kwerft needs for a `local` profile

**Kernel**
- Build Kwerft's kernel in CI from apple/containerization's config plus
  `hack/spike-mac/kernel/kwerft.config` (native arm64 image with pahole:
  2 min 52 s on this Mac, against ~11 min with Apple's Rosetta image) and ship
  it with the app. Track Apple's config and kernel security releases.

**Installer** (`--platform mac`, or detected)
- Treat built-in kernel modules as loaded instead of failing on `modprobe`.
- Skip `swapoff` when the kernel has no swap support.
- Install the boot unit from `prepare.sh` (shared mounts, bpffs, the stable
  `kwerft0` address) and pin k3s to that address.
- Use a local CA issuer instead of Let's Encrypt, and default to `--lite`
  observability.
- Let the Mac reach the API: allow 6443 from the vmnet gateway with the
  stable address in the certificate, or go through Kwerft's API proxy.
- Found here and already fixed (`d47ecf0`): Handoff died when the console
  domain did not resolve yet (`getent` exits 2 under `pipefail`).

**Chart and controllers**
- Let the chart name any ClusterIssuer (`acme.issuer` or similar); the spike
  had to patch the Deployment's `--cluster-issuer` argument.
- With no issuer configured, the Domain reconciler still creates HTTPS
  listeners that reference TLS secrets nobody creates; Traefik rejects them
  (`InvalidCertificateRef`) and every public route returns 404, while the
  Domain claims "HTTPS uses the ingress default certificate". Create no HTTPS
  listener, or a self-signed certificate, in that case.

**Mac app**
- Check container egress first and name a VPN as the likely cause.
- Treat `container machine run` as an unquoted shell line: pass scripts as
  files, or use the XPC client library once it is a stable API.
- Load locally built images the way the installer loads Kwerft's own:
  `container image save` into the shared home directory, then
  `k3s ctr images import` inside the machine.
- Add the local CA to the login keychain so `*.kwerft.test` is trusted, and
  register `kwerft.test` with `container system dns` (or `/etc/resolver`).

**Repository**
- Add `.claude` to `.dockerignore`: agent worktrees (260 MB here) end up in
  every `container build` context.

## Reproducing

```bash
hack/spike-mac/kernel/build.sh
KERNEL="$PWD/bin/spike-mac/vmlinux-kwerft-arm64" hack/spike-mac/machine.sh up
hack/spike-mac/machine.sh demo
hack/spike-mac/machine.sh restart
```

Option A needs the kernel as `container`'s default
(`container system kernel set --arch arm64 --binary …`; revert with
`--recommended`), then `hack/spike-mac/kind.sh up` and `demo`.
