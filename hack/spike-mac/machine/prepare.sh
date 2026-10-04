#!/bin/sh
# Run once as root inside a fresh machine, before install.sh (see machine.sh).
# Makes the VM look like a stock server to the unmodified installer. Each item
# here is a finding the local profile has to handle for real.
set -eu

# 1. Boot-time setup, re-applied on every start (none of it persists):
#    - a machine's mounts are private; kubelet and Cilium need shared
#      propagation (systemd sets it on a real server) and a BPF filesystem;
#    - eth0's address changes on every start, but etcd and the k3s node IP
#      must not. A dummy interface holds a stable private address, which
#      install.sh picks up as the "private network" and pins k3s to.
cat >/usr/local/sbin/kwerft-machine-boot <<'SH'
#!/bin/sh
set -eu
mount --make-rshared /
mountpoint -q /sys/fs/bpf || mount -t bpf bpffs /sys/fs/bpf
mount --make-shared /sys/fs/bpf
ip link show kwerft0 >/dev/null 2>&1 || ip link add kwerft0 type dummy
ip addr replace 10.255.255.1/24 dev kwerft0
ip link set kwerft0 up
SH
chmod +x /usr/local/sbin/kwerft-machine-boot
cat >/etc/systemd/system/kwerft-machine-boot.service <<'UNIT'
[Unit]
Description=Kwerft machine: shared mounts, BPF filesystem, stable node address
DefaultDependencies=no
After=local-fs.target
Before=network-online.target k3s.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/kwerft-machine-boot

[Install]
WantedBy=sysinit.target
UNIT
systemctl daemon-reload
systemctl enable --now kwerft-machine-boot.service

# 2. The kernel has overlay, br_netfilter and wireguard built in and ships no
#    /lib/modules, so `modprobe overlay` fails. Declare them as built-ins.
d=/lib/modules/$(uname -r)
mkdir -p "$d"
printf '%s\n' kernel/fs/overlayfs/overlay.ko kernel/net/bridge/br_netfilter.ko \
  kernel/drivers/net/wireguard/wireguard.ko >"$d/modules.builtin"
depmod 2>/dev/null
modprobe overlay && modprobe br_netfilter && modprobe wireguard

# 3. No swap support in the kernel: `swapoff -a` exits 16 on an empty /proc/swaps.
if [ ! -s /proc/swaps ] && [ ! -e /usr/local/sbin/swapoff ]; then
  cat >/usr/local/sbin/swapoff <<'SH'
#!/bin/sh
[ -s /proc/swaps ] || exit 0
exec /usr/sbin/swapoff "$@"
SH
  chmod +x /usr/local/sbin/swapoff
fi

echo "prepared: boot unit (shared mounts, bpffs, kwerft0 10.255.255.1), built-in modules, swapoff"
