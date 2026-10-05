# Phase 5 — Nodes & clusters: work split and contracts

Exit criteria:
1. A cluster with Cloud nodes loses a node while apps stay reachable (the
   mixed dedicated + Cloud variant is built and tested with fakes; a real
   dedicated server comes later, decided 2026-10-05).
2. A second cluster is managed from the same console: projects, apps,
   builds, logs, metrics and alerts work there as in the local cluster.
3. Every release tag passes a fresh install and an upgrade from the
   previous release on a new Hetzner Cloud server.

Verification uses real Hetzner Cloud servers created with a token from a
separate test project that the user provides; **the coordinator asks before
creating any**. Workers use fakes only.

Five workers build this in parallel, each in its own branch. Shared, already
on main: `api/v1alpha1` (`Cluster`, `NodePool`), `internal/clusters`
(the `Registry` seam, `Local`, a `Static` registry), the pluggable Hetzner
fake (`hetznertest.Server.Handle`), and the Clusters page skeleton
(`web/src/pages/Clusters*.tsx`).

## Architecture

- **Every cluster runs Kwerft's controllers and CRDs.** Kubernetes stays the
  source of truth per cluster; an App in a remote cluster is reconciled
  there.
- **The management cluster** (where the console runs, Cluster `local`)
  holds the console, its SQLite store, `Cluster` and `NodePool` objects and
  the Hetzner credentials.
- **Remote clusters** run Kwerft in *agent mode*: the chart without the
  console UI and store, plus `kwerft agent`, which dials out to the console
  (`wss://<console>/api/v1/clusters/connect`) with that cluster's token and
  carries the console's requests to its Kubernetes API (a reverse tunnel).
  Remote API servers are never exposed publicly.
- **The console** reaches each cluster through `clusters.Registry`:
  `RESTConfig(name)` gives Kwerft's own identity there (the agent's service
  account, with the same impersonation rights the console has locally), and
  user requests add impersonation as today.
- **Project names are unique across clusters**; the console resolves a
  project to its cluster, so API URLs stay `/api/v1/projects/{p}/…`. Lists
  aggregate across connected clusters and carry a `cluster` field; project
  creation picks a cluster (default `local`).
- **Observability per cluster**: each cluster runs its own metrics, logs and
  Alertmanager; the console queries a remote one through the tunnel (the
  Kubernetes service proxy), with the same project confinement.

## Join material

- Secret `cluster-<name>-join` in `kwerft-system` (management cluster) with
  keys `server` (`https://<control-plane private IP>:6443`) and `token` (the
  k3s join token). For `local` the installer writes it; for remote clusters
  the agent publishes it after bootstrap.
- Secret `cluster-<name>-agent` in `kwerft-system`: the agent's token
  (stored hashed in the console as well) and the console URL, for the
  installer's agent mode.

## Ownership

| Worker | Owns | Touches lightly |
|---|---|---|
| W1 Hetzner Cloud integrations | Cloud API token setting (write-only Secret `kwerft-hcloud-token`, Settings › Hetzner Cloud API, verified on save), `internal/hetzner` cloud files for firewalls, networks, load balancers, volumes (+ fakes via `Handle`), Cloud Firewall sync (FirewallRules → one Cloud Firewall per cluster, applied by label), hcloud cloud-controller-manager and CSI driver (installer and chart, storage class `hcloud-volumes`), an optional Load Balancer in front of the ingress | `install.sh`, `api_settings.go`/`Settings.tsx`, firewall controller (sync only) |
| W2 node pools & HA | `internal/hetzner` cloud files for servers, SSH keys, placement groups, server types, locations, images (+ fakes); `internal/controllers/nodepool_*.go`: create/replace/drain/delete Cloud servers with cloud-init that runs `install.sh` (join, or agent-mode bootstrap for a new cluster's first control-plane node); control-plane pools for a 3-node HA control plane (k3s embedded etcd); build pools tainted for builds that scale to zero; dedicated-server join (the console shows the join command; remove a node); Robot/vSwitch coupling for mixed clusters (client + fake only); UI: `ClusterNodes.tsx` | `install.sh` / `join.sh` (cloud-init flags), Build reconciler (schedule onto build pools) |
| W3 multi-cluster core | `kwerft agent` (tunnel client, publishes join material, health), the tunnel endpoint and a `clusters.Registry` backed by it (`internal/clusters/tunnel*`), `internal/controllers/cluster_*.go` (the `local` Cluster, hetzner-cloud creation through a control-plane NodePool, adopted clusters and their install command, status from the agent), agent mode in the chart and `install.sh --agent`; UI: `ClustersList.tsx`, `ClusterOverview.tsx` | `main.go`, chart values |
| W4 multi-cluster console | Make the console cluster-aware on top of `clusters.Registry`: per-cluster impersonators and informer caches, project → cluster resolution and cross-cluster unique names, aggregated lists with `cluster`, metrics/logs/alerts/Hubble per cluster through the service proxy, the isolation suite extended to two clusters; UI: cluster column/badges, a cluster filter, the cluster picker in project creation | most of `internal/server`, `internal/kube`, list pages |
| W5 e2e install runs | `.github/workflows/e2e.yml`, `hack/e2e/` (create a Cloud server, install, assert, upgrade, destroy; sweeper), `RELEASING.md`; design in `docs/phase4.md` › e2e | `release.yml`, `install.sh` only if a flag is missing |

## Security

- Agent tokens: one per cluster, shown once (install command), stored as a
  hash; rotating one disconnects the old agent. The agent verifies the
  console's TLS certificate (public CA). The tunnel only carries requests to
  that cluster's API server — no other destinations.
- The agent's service account has exactly the console's local rights
  (impersonating the console's user/groups, the reconcilers' needs); user
  requests are always impersonated, as today.
- Hetzner Cloud token: write-only like the DNS token; never sent to remote
  clusters except the CCM/CSI Secret a Cloud cluster needs (decide whether a
  remote cluster gets the token or its own — document).
- Cloud servers created by Kwerft carry labels `kwerft.dev/cluster=<name>`,
  `kwerft.dev/pool=<pool>` and are only ever deleted by those labels.

## Verification

Each worker: `make check` green (and `GOTOOLCHAIN=go1.26.0 go vet ./...`),
envtest tests, fakes for Hetzner Cloud/Robot and for a remote cluster (a
second envtest API server behind an in-process tunnel is a good model).
Nobody creates real infrastructure; the coordinator does, after asking.

## As built (W1): Hetzner Cloud integrations

Code: `internal/hetzner/{actions,firewalls,networks,loadbalancers,project}.go`
(+ fakes in `hetznertest/{actions,firewalls,networks,loadbalancers,serverlist}.go`),
`internal/firewall/cloud.go`, `internal/controllers/hcloud_*.go` and
`firewall_cloud.go`, `internal/server/api_hcloud.go`,
`web/src/pages/SettingsHCloud.tsx`; `ConsoleSettings.spec|status.hetznerCloud`;
install.sh stage **Hetzner Cloud** (`stage_hcloud`).

- **Cloud API client** (checked against `docs.hetzner.cloud/cloud.spec.json`,
  2026-10-05): firewalls (create, `set_rules`, `apply_to_resources`,
  `remove_from_resources`, delete), networks (+ `add_subnet`), Load Balancers
  (services, targets, `attach_to_network`), a read-only server list
  (`ServerSummaries`, matching nodes by address or `hcloud://` provider ID)
  and action polling (`WaitActions`; changing calls wait for their actions).
  403 is now `ErrForbidden` (still `errors.Is(ErrTokenRejected)`).
  `ProbeWrite` tells a Read-only token from a Read & Write one without
  changing anything (`POST /firewalls` without a name: 422 vs 403). Limits:
  50 rules per firewall, 100 CIDRs per rule, 5 firewalls per server.
  **For W2:** the fake's families live in their own files and share
  `NewAction` (actions) and the server list State key `serverList`
  (`AddServer`); W2's servers fake should register `servers` and keep
  `ServerSummary`-compatible JSON there, or replace `serverlist.go`.
- **Token** (Settings › Hetzner Cloud API, management cluster only):
  write-only Secret `kwerft-hcloud-token` (key `token`), created empty by the
  Domain reconciler, Role `kwerft:hcloud-token` (patch only, owners and
  admins). `PUT /api/v1/settings/hcloud-token` lists the project's servers,
  probes write access, finds this cluster's nodes among them ("the token's
  project has 3 servers in fsn1; this cluster's kwerft-1 (fsn1)", a warning
  when none of the Cloud nodes is there), stores the token as the user, sets
  `kwerft.dev/hcloud-token-updated-at` and audits `settings.hcloud_token`.
  `DELETE` empties it (Kwerft stops managing; the Cloud Firewall and Load
  Balancer stay in the project). install.sh `--config` `hcloud.tokenFile`
  stores it the same way. Token routes refuse `?cluster=` other than local;
  `PUT /api/v1/settings/hcloud` (firewall mode, Load Balancer) takes
  `?cluster=` (default local) — only local is wired; the coordinator routes
  others through W4's per-cluster clients in `settingsAPI.clusterClient`.
- **Cloud Firewall sync** (`HetznerCloudReconciler`, one pass per change,
  resync 5 min): one firewall `kwerft-<cluster>-<instance[:8]>` labelled
  `kwerft.dev/managed-by=kwerft`, `kwerft.dev/instance=<kube-system UID>`,
  `kwerft.dev/cluster=<cluster>`, applied to the label selector
  `kwerft.dev/cluster=<cluster>` plus, by ID, the Cloud nodes without that
  label (the installer's first server; servers are never relabelled).
  Rules = `firewall.CloudRules` of the rule set **as last confirmed on the
  nodes** (the snapshot behind "Roll back now"): the required public rules
  (SSH from the confirmed SSH sources or anywhere, HTTP, HTTPS, WireGuard,
  ICMP — always present, checked again before sending) and the enabled
  custom rules (ports open to everyone become `0.0.0.0/0` + `::/0`;
  control-plane-only rules open on every Cloud server, the host firewall
  narrows them). A pending change (e.g. a new SSH narrowing) reaches the
  Cloud only after it was confirmed on the hosts; a rollback on the hosts
  needs no API call; before any confirmation SSH narrowing is never sent.
  `cluster-private` has no Cloud counterpart (private networks are not
  filtered). Per rule `status.cloudFirewall`: Applied, Pending, Private,
  Disabled, Invalid, Off. Drift (rules edited in the Hetzner Console, nodes
  joining or leaving) is put back. More than 50 Cloud rules: the firewall
  stays as it is, status Error. `spec.hetznerCloud.firewall: off` detaches
  and deletes it. A foreign firewall of the same name is never touched.
  Dedicated nodes (`kwerft.dev/platform=dedicated`) are never added.
  Behaviour change: the Cloud Firewall also closes NodePorts, which the host
  firewall cannot (Cilium answers them in eBPF). UI: Settings card (state,
  rules, servers) and Network › Server firewall (a "Cloud" pill per rule, a
  Hetzner Cloud Firewall line).
- **Load Balancer** (`spec.hetznerCloud.loadBalancer`, optional, per
  cluster; `--config hcloud.loadBalancer`): same name and labels, type
  `lb11` by default, location of the first Cloud node unless set, attached
  to the nodes' Cloud Network, TCP 80→80 and 443→443 **with the PROXY
  protocol**, TCP health checks, targets the selector + unlabelled Cloud
  nodes over their private IPs. Needs the console flag
  `--hcloud-proxy-network` (install.sh sets it to the Cloud Network's range
  on Cloud servers in a network and configures Traefik's
  `proxyProtocol.trustedIPs` for it, so client addresses and the firewall's
  lock-out check stay right). DNS: once a target is healthy on 443 the
  status is `active` and the Domain reconciler reports the Load Balancer's
  IPv4/IPv6 as `status.publicAddresses`, which the DNS reconciler and the
  Settings checks follow; it then stays active (no flapping). Turning it off
  moves DNS back to the nodes at once and deletes the Load Balancer
  `LBDrain` (10 min, 2 × TTL) later. Services and targets changed by hand
  are put back; a different type is reported, not changed.
- **Token in other places**: the reconciler copies a changed token into
  `kube-system/hcloud` (the CCM's and CSI driver's) when install.sh created
  it (label `app.kubernetes.io/managed-by=kwerft`) and restarts
  `hcloud-cloud-controller-manager` and `hcloud-csi-controller` (pod
  template annotation). An operator's own Secret is never touched.
- **Remote clusters (decision)**: Hetzner tokens cannot be scoped, and a
  Cloud cluster's servers, volumes and Load Balancers must live in the
  token's project, so a `hetzner-cloud` cluster gets **the management
  project's token**, and only as its CCM/CSI Secret `kube-system/hcloud`
  (the Security rule above; W3 writes it when bootstrapping). Its own Kwerft
  controllers (`KWERFT_CLUSTER_NAME=<name>`, W3's agent mode sets it) fall
  back to that Secret for their Cloud Firewall and Load Balancer when
  `kwerft-hcloud-token` is empty; the management cluster never falls back,
  so removing the token in Settings stops its sync. Risk: a cluster-admin
  of a remote Cloud cluster can read the project token; separate trust
  boundaries need separate Hetzner projects (adopted clusters bring their
  own token through their own installer `--config`).
- **CSI driver** (chart `hcloud-csi` 2.23.0): installed by `stage_hcloud`
  on Cloud servers whenever a token is known (`--config` or stored in
  Settings — so: set the token, re-run the installer). Its pods run only on
  nodes not labelled `kwerft.dev/platform=dedicated`. Works without the CCM
  (the node plugin reads the metadata service). Storage class
  `hcloud-volumes` is Kwerft's own (not the chart's, not default; local-path
  stays default), WaitForFirstConsumer, expandable, reclaim Delete, with
  `allowedTopologies` on `csi.hetzner.cloud/location` (the locations pinned
  in install.sh), so a pod with a Cloud Volume is never scheduled onto a
  dedicated server. The Volumes page asks `GET /api/v1/volume-classes` and
  offers Cloud Volume only where the class exists; creating one elsewhere
  is refused (422 `class`).
- **cloud-controller-manager (decision: install-time only)** (chart
  `hcloud-cloud-controller-manager` 1.38.0, `--config`
  `hcloud.cloudControllerManager: true` + `hcloud.tokenFile`, Cloud only).
  k3s's embedded cloud controller gives every node the provider ID
  `k3s://<node>`, and a Node's provider ID can never change, so switching an
  existing cluster to the hcloud CCM would mean re-registering every node.
  With the option on a first install: k3s `disable-cloud-controller: true`
  and `kubelet-arg: cloud-provider=external` (k3s sets the latter only while
  its own controller runs, so it is explicit, also for joiners: the join
  response's `cloudProvider: "external"` — **W2's join API** should send it
  when the console runs with `--hcloud-ccm`, `server.Config.HCloudCCM`),
  Cilium installed without `--wait` while the node is still tainted, then
  the CCM with `HCLOUD_NETWORK` (the Cloud Network of the private address,
  from the metadata service) so InternalIP stays the private `--node-ip`,
  routes controller off (`HCLOUD_NETWORK_ROUTES_ENABLED=false`: Cilium
  tunnels, no Cloud routes), and the installer waits until no node is
  uninitialized. On an existing cluster the option only warns. Nothing in
  Kwerft needs the CCM: the firewall, Load Balancer and CSI work without it.
- **Uninstall** leaves the Cloud Firewall and Load Balancer in the project
  (named `kwerft-<cluster>-…`): delete them in the Hetzner Console (detach
  the firewall first). Turning them off in Settings before uninstalling
  removes them.

### Trying it on a real Cloud server (coordinator)

Costs (2026-10, excl. VAT, billed hourly; check the Hetzner price list): a
small shared-vCPU server and an LB11 cost a few cents per hour each, a
10 GB Volume a fraction of a cent; Cloud Firewalls and Networks are free.
Use the test project's token only.

1. In the test project create a Network `kwerft-test` (10.0.0.0/16, cloud
   subnet in eu-central) and a Cloud server (Ubuntu 24.04, ≥ 8 GB) attached
   to it. Keep the Hetzner Console (VNC) open as the way back.
2. Install with `--config` containing `hcloud: { tokenFile: /root/hcloud.token,
   cloudControllerManager: true }` (+ owner and domain as usual). Check:
   `kubectl get nodes -o wide` (Ready, InternalIP 10.0.0.x, ExternalIP the
   public one), `kubectl get nodes -o jsonpath='{.items[*].spec.providerID}'`
   → `hcloud://<id>`, no `uninitialized` taint; `kubectl -n kube-system get
   pods` (CCM, CSI controller and node Running); `kubectl get sc`
   (local-path default, hcloud-volumes not); `kubectl -n traefik get ds
   traefik -o yaml | grep proxyProtocol` (trustedIPs 10.0.0.0/16).
3. Settings › Hetzner Cloud API shows the server and its location. Within a
   minute a firewall `kwerft-local-…` exists (Hetzner Console › Firewalls)
   with 22/80/443/udp 51871 and ICMP, applied to the server; a new SSH
   session still works; the console still loads. Network › Server firewall
   shows "Cloud" pills and the Cloud Firewall in sync. Open a port (TCP 8080
   from your IP): it appears in the Cloud Firewall within seconds. Narrow
   SSH to your address and keep it: the Cloud rule follows only after
   "Keep". A NodePort Service is no longer reachable from outside.
4. Volumes: create a 10 GB Cloud Volume in a project and mount it from an
   app: a Volume appears in the Hetzner Console, attached to the server;
   resize it to 11 GB; delete it (the Hetzner Volume goes too).
5. Load Balancer: turn it on in Settings. An LB11 `kwerft-local-…` appears
   with services 80/443 (proxy protocol) and the server as target over
   10.0.0.x; Settings shows Waiting, then Active with its addresses; with
   managed DNS records they switch to the Load Balancer's addresses (`dig`).
   Open an app and the console through it; the audit log shows your real
   address. Turn it off: DNS goes back at once, the Load Balancer is gone
   after 10 minutes.
6. Replace the token in Settings: `kube-system/hcloud` gets the new one and
   the CCM and CSI controller restart.
7. Without the CCM: install a second server without
   `cloudControllerManager`, store the token in Settings, re-run the
   installer: CSI and `hcloud-volumes` appear, the provider ID stays `k3s://`.
8. Clean up: turn the firewall and Load Balancer off in Settings, then
   delete the servers, Volumes and the Network.
