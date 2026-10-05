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

## Installer flags and shared names (W2 relies on these)

Node pool servers run the installer from cloud-init; these are the exact
flags (`internal/controllers/nodepool_cloudinit.go`), aligned with W3's agent
mode on 2026-10-05.

- **Join** (workers, build nodes, joining control-plane servers):
  `install.sh --join <console URL> --token <kwft_join_…> --role worker|control-plane --platform cloud [--node-label k=v]… [--node-taint k=v:Effect]… --yes`.
  The installer trades the token at `GET <console>/api/v1/join?role=<role>&node=<hostname>`
  (Bearer token) for `{"server": "https://<ip>:6443", "token": "<k3s token>"}`.
  Dedicated servers run the same through
  `curl -fsSL <console>/join.sh | sudo bash -s -- --token <kwft_join_…> --role worker`;
  the console serves `/join.sh` and `/install.sh` (its own version).
- **Agent-mode bootstrap** (the first server of a new `hetzner-cloud`
  cluster's control-plane pool, which W3's Cluster reconciler creates as
  `<cluster>-control-plane`, while `cluster-<name>-join` does not exist yet):
  `install.sh --agent --console <consoleURL> --cluster-token <token> --platform cloud [--version <release>] [--node-label k=v]… --yes`,
  from Secret `kwerft-system/cluster-<name>-agent`, keys **`token`** and
  **`consoleURL`**, which W3 deletes after the agent's first connection.
  The private interface is detected (the first RFC 1918 address off the
  default route), so no `--private-iface`. Every other server of the
  cluster waits for `cluster-<name>-join` (keys `server`, `token`), which
  W3 writes from the agent's report — and for `local`, W3's
  `write_join_secret` in `install.sh`.
- `--node-label` and `--node-taint` (repeatable) are added by W2 for every
  mode: k3s `node-label` / `node-taint`, so a build node is tainted before
  any pod can land on it.
- **Cloud Network of a cluster** (`internal/hetzner/names.go`): the network
  labelled `kwerft.dev/cluster=<cluster>`, else the one named
  `kwerft-<cluster>` (`hetzner.NetworkName`). W1 creates it with both;
  node pools attach every server to it and wait while it is missing.
- **Cloud token**: Secret `kwerft-hcloud-token` in `kwerft-system`, key
  `token` (W1 writes it; node pools and the nodes API read it).
- **Labels** (`internal/hetzner/names.go`): `kwerft.dev/cluster`,
  `kwerft.dev/pool`, `kwerft.dev/role`, `kwerft.dev/bootstrap` on servers;
  `kwerft.dev/ssh-key` marks the SSH keys put on new servers (all keys of
  the project when none is marked).
- `server.Config.Clusters` (a `clusters.Registry`) is how the nodes API
  reaches remote clusters; W3's tunnel registry replaces the
  `clusters.Static` that `cmd/kwerft` passes for now, and W4 builds the
  per-cluster impersonators on the same field.

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

## As built (W2)

**Hetzner Cloud client** (`internal/hetzner`: `servers.go`, `ssh_keys.go`,
`placement_groups.go`, `server_types.go`, `locations.go`, `images.go`,
`actions.go`, `names.go`, `robot.go`), checked against
`docs.hetzner.cloud/cloud.spec.json` (2026-10-05): servers carry `location`
(no datacenter), create takes `location`/`server_type`/`image` by name,
`networks`, `placement_group`, `ssh_keys`, `user_data` (≤ 32 KiB) and
`labels`; server types list availability and prices per location. Images:
the newest of `ubuntu-26.04`, `24.04`, `22.04` for the server type's
architecture (the spec's examples only show 24.04; the catalogue decides).
Fakes in `hetznertest/nodes.go` (`FakeNodes`, `FakeClusterNetworks` —
W1's networks fake supersedes the latter) and `hetznertest/robot.go`.

**NodePool reconciler** (`internal/controllers/nodepool_*.go`), in the
management cluster, reaching each pool's cluster through `ClusterClients`
(`RegistryClients` over `clusters.Registry`):

- Servers `<cluster>-<pool>-<5 chars>`, image newest Ubuntu LTS, attached to
  the cluster network, in a spread placement group `kwerft-<cluster>-<pool>`
  (up to 10 servers; more go without), labelled as above, cloud-init as in
  *Installer flags*. Only servers with both `kwerft.dev/cluster` and
  `kwerft.dev/pool` of the pool are listed, and deletion checks both again.
- Status per server: Creating → Joining → Ready (the Node of the server's
  name is Ready), Draining, Deleting, Failed (no Ready node after 20 min:
  reported, not deleted — no create/delete loops that cost money; "Replace"
  in the UI deletes it and the pool makes a new one).
- **Scale down**: broken servers first, then servers of another type, then
  the newest. Drain = cordon + Eviction API (PDBs respected) for up to
  15 min, then the remaining pods are deleted (like GKE's pool upgrades);
  DaemonSet and static pods stay. A node holding local-path volumes is not
  drained unless forced ("remove node" with force): their data lives on
  that disk. Then the Node is deleted, then the server.
- **Replace** (server type or location changed): one extra server first;
  once it is Ready the oldest outdated server is drained and deleted; repeat.
- **Repair**: worker and build nodes NotReady for 15 min are drained
  (forced: nothing can be evicted gracefully from a dead node) and replaced.
  Control-plane nodes are never replaced automatically.
- **HA control plane**: control-plane pools join as k3s servers (embedded
  etcd), one at a time. The cluster's control-plane nodes (in this pool or
  not) must stay odd and ≥ 1: count 2 (with no other) or 0 for the last
  ones is refused (`EvenControlPlane`, `LastControlPlane`), by the API and
  by the reconciler. Removal is one at a time, only while every other
  control-plane node is Ready; before the Node is deleted, k3s takes the
  member out of etcd (`etcd.k3s.cattle.io/remove=true`, waiting up to 3 min
  for `etcd.k3s.cattle.io/removed-node-name`).
- **New hetzner-cloud clusters**: without `cluster-<name>-join`, only the
  first control-plane pool (by name) creates one server, in agent mode,
  labelled `kwerft.dev/bootstrap=true`; everything else waits for the join
  material the agent publishes.
- **Build pools** (role `builds`): nodes labelled and tainted
  `kwerft.dev/builds=true:NoSchedule`. While such a pool exists with count
  > 0, ConfigMap `kwerft-build-nodes` in the cluster's `kwerft-system` tells
  that cluster's Build reconciler to add the toleration and a required node
  affinity to build Jobs. The pool polls the cluster's unfinished build Jobs
  every 20 s: with builds it grows to their number (≤ count), with none for
  `scaleDownAfter` (default 15 min, `status.lastBuildAt`) it goes to zero. A
  build waits about 3–5 min for its first server; the build timeout
  (30 min) includes that wait.
- Finalizer `kwerft.dev/nodepool`: deleting a pool drains and deletes its
  servers (or just deletes them, by label, when its Cluster is gone or
  being deleted — W3's Cluster finalizer waits for the pools), then the
  placement group. An unreachable cluster does not hold the deletion up.

**Node removal reconciler** (every cluster): `kwerft.dev/drain=true` on a
Node cordons and drains it (kept; "Uncordon" clears it);
`kwerft.dev/remove=true|force` drains, removes the etcd member of a
control-plane node and deletes the Node. Pool nodes are the pool
reconciler's. A removed dedicated server must be switched off
(`install.sh --uninstall`), or k3s registers it again.

**Join tokens** (`internal/jointoken`): `kwft_join_<claims>.<HMAC>` signed
with a key derived from the console's data key, holding cluster, role,
expiry and — for servers Kwerft creates — the server name. Nothing is
stored; rotating the data key invalidates outstanding tokens. Why not k3s's
token: cloud-init user data is readable through the Cloud API and the
metadata service, and a join command gets pasted around. The console hands
out at `GET /api/v1/join` (public, rate limited, audited):

- **workers**: a k3s bootstrap token (Secret `bootstrap-token-<id>` in
  kube-system, as `k3s token create --ttl 1h` makes; agent token
  `K10<CA hash>::<id>.<secret>`), never the server token;
- **control plane**: the server token (k3s encrypts its bootstrap data with
  it, a joining server has to decrypt it) — so control-plane join commands
  are owners' only.

A token bound to a server is refused for another hostname and once a node
of its name exists. Dedicated join commands (owners/admins, 30 min–24 h,
default 1 h) work for any number of servers until they expire.

**vSwitch coupling** (owners, `POST /api/v1/clusters/{c}/vswitch`): Robot
webservice user and password in the request (used once, never stored or
logged) → create a vSwitch (`kwerft-<cluster>`, VLAN 4000–4091) or use an
existing one → add the dedicated servers → add a `vswitch` subnet (e.g.
`10.0.64.0/24`, inside the network's range) to the cluster's Cloud Network.
By hand on each dedicated server afterwards (Hetzner's
"connect dedicated servers" guide): a VLAN interface on the vSwitch's VLAN
with an address in that subnet, MTU 1400, and a route to the network's
range via the subnet's gateway (`.1`); then the join command (the installer
finds the VLAN interface as the private network, or pass
`--private-iface`). No UI yet; client and fake only, as agreed.

**API** (`internal/server/api_nodes.go`, owners and admins, impersonated,
audited): `GET /api/v1/clusters/{c}/nodes` (pools, nodes, joinable, cloud,
control-plane count), `POST …/pools`, `PATCH|DELETE …/pools/{pool}`,
`POST …/pools/{pool}/servers/{server}/remove`, `POST
…/nodes/{node}/drain|uncordon|remove`, `POST …/join-command`, `POST
…/vswitch` (owners), `GET /api/v1/hetzner/catalog`; public `GET
/api/v1/join`, `/join.sh`, `/install.sh`. Pool objects are named
`<cluster>-<pool>`. RBAC: owners and admins get nodes get/list/watch/patch
(roles.yaml); the controller gets pods/eviction and persistentvolumes.

**UI** (`ClusterNodes.tsx`): pools with server status and "Replace" for
servers that never joined, add/scale dialog with location and server type
pickers and prices from the Cloud API, delete; the nodes table with drain,
uncordon and remove (with force); "Join a server" with role and validity.

**Try on real Cloud servers** (coordinator, with the test project's token;
costs at 2026-10 list prices, about €0.01 per cx23-hour):

1. Settings › Hetzner Cloud: store the token; make sure the local cluster's
   Cloud Network is labelled `kwerft.dev/cluster=local` (or named
   `kwerft-local`) and the test server is attached to it.
2. Re-run the installer on the test server (W3's `write_join_secret`
   writes `cluster-local-join`).
3. Clusters › local › Nodes › Add node pool: `workers`, cx23, fsn1, 2. Watch
   Creating → Joining → Ready (~4 min); `kubectl get nodes -L kwerft.dev/pool`;
   on a server `cat /var/log/kwerft-join.log`; check the k3s agent's config
   holds a `K10…::<id>.<secret>` token and `kubectl -n kube-system get secret
   | grep bootstrap-token`. **Reboot one worker after 1 h** (bootstrap token
   expired): it must come back Ready — the open risk below.
4. Scale to 1 with a 2-replica app and a PDB `minAvailable: 1`: the app
   stays reachable; the newest server is drained, removed and deleted.
5. Change the type to cx33: one new server, then the old one goes.
6. Kill a worker in the Hetzner console (power off): after 15 min it is
   replaced; apps stay reachable meanwhile (exit criterion 1).
7. Control plane: pool `cp`, cx23, count 2 (three with the test server):
   servers join one at a time; `kubectl get nodes -l node-role.kubernetes.io/etcd`
   shows three. Count 1 is refused (two in all). Scale to 0: the test
   server stays the only control-plane node; the two leave one at a time,
   each after k3s removed its etcd member.
8. Build pool: `builds`, cx33, count 1, idle 15: start a Git build; a server
   starts, the build runs there (`kubectl -n kwerft-builds get pod -o wide`),
   and 15 min later the server is gone.
9. Delete the pools; `hcloud server list -l kwerft.dev/cluster=local` is
   empty afterwards.

Cost of the full run: five cx23/cx33 servers for about two hours, about
€0.20, plus traffic (none to speak of).

**Open risks**: k3s bootstrap tokens as agent tokens are from k3s's
documented `k3s token create`, but not yet tried here — and whether an
agent restarts fine after its bootstrap token expired is to be checked
(step 3); the fallback is a longer TTL. The etcd removal annotations are
k3s's member controller's; the 3-min fallback deletes the Node anyway. The
bootstrap server's user data (readable through the metadata service) holds
the cluster's bootstrap token; W3 deletes its Secret after the first
connection, so it should not work twice — worth checking with W3's tunnel. The Cloud Network wait loop in
cloud-init assumes the private address is RFC 1918.

## Verification

Each worker: `make check` green (and `GOTOOLCHAIN=go1.26.0 go vet ./...`),
envtest tests, fakes for Hetzner Cloud/Robot and for a remote cluster (a
second envtest API server behind an in-process tunnel is a good model).
Nobody creates real infrastructure; the coordinator does, after asking.
