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

## Cluster-scoped kinds: per cluster or management only

Decided by W4 (2026-10-05). "Per cluster": every cluster has its own
objects, reconciled there; the console API addresses one with `?cluster=`
(default `local`) and lists carry `cluster`. "Management only": the objects
live in the management cluster and serve every cluster; the API always
talks to `local`, whatever the request names.

| Kind | Where | Why |
|---|---|---|
| `Project` (and everything namespaced) | per cluster, names unique across clusters | the project's apps run there; the console resolves a project to its cluster |
| `ConsoleSettings` | per cluster (apps domain, TLS, DNS); `spec.consoleDomain` only in `local` | each cluster's ingress serves its own apps; there is one console |
| `FirewallRule` | per cluster | host rules of that cluster's nodes (and its Cloud Firewall, W1) |
| `AlertRule` | per cluster | evaluated by that cluster's vmalert against its own metrics; a rule watches projects of one cluster |
| `NotificationChannel` | management only | one place for Slack/email/webhook/ntfy credentials. Remote clusters' Alertmanagers need the receivers too: **open** — mirror channels and their `notify-*` Secrets into each connected cluster (W3's agent sync, or a follow-up) |
| `GitConnection` | management only | one place for Git credentials and webhooks; the console's webhook endpoint creates Builds in whichever cluster the matching App lives. Remote Build pods need the clone credentials: **open** — mirror the connection and `kwerft-builds/git-*` Secret into connected clusters (short-lived GitHub App tokens where possible) |
| `Cluster`, `NodePool` | management only | as specified above |

## Join material

- Secret `cluster-<name>-join` in `kwerft-system` (management cluster) with
  keys `server` (`https://<control-plane private IP>:6443`) and `token` (the
  k3s join token). For `local` the installer writes it; for remote clusters
  the agent publishes it after bootstrap.
- Secret `cluster-<name>-agent` in `kwerft-system`: the agent's token
  (stored hashed in the console as well) and the console URL, for the
  installer's agent mode.
- As built (W3): every installer run on a k3s server writes
  `cluster-local-join` in its own cluster's `kwerft-system`
  (`write_join_secret`); in the management cluster that *is*
  `cluster-<local>-join`, in a remote one the agent reports it and the
  Cluster reconciler copies it to `cluster-<name>-join` (owned by the
  Cluster). The token hash lives on the Cluster (annotation
  `kwerft.dev/agent-token-hash`), not in the Secret, and
  `cluster-<name>-agent` exists only for a hetzner-cloud cluster until its
  agent first connects (keys `token`, `consoleURL`); see As built (W3).

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

## As built (W3): multi-cluster core

**Tunnel** (`internal/clusters/tunnel*.go`, diagram in `tunnel.go`). The
agent dials `wss://<console>/api/v1/clusters/connect` with
`Authorization: Bearer <agent token>`; TLS is verified against the system's
public CAs. Over the WebSocket (one binary message per write) runs HTTP/2:
the console is the client, the agent the server (`golang.org/x/net/http2`,
already in the module graph: flow control per stream, 1000 concurrent
streams, PING every 30 s of silence with a 15 s timeout on both ends,
30 s write timeout). Every connection the console opens to the cluster's
API is one HTTP/2 `CONNECT` stream carrying plain HTTP/1.1 to the agent,
which serves it with an `http.Server` whose only handler is a reverse proxy
to its own API server, with its service account's credentials. Byte streams
rather than one HTTP/2 request per API request, because protocol upgrades
(SPDY and WebSocket exec/attach/port-forward) need them; watches and log
follows stream unchanged (`FlushInterval: -1`). Upgrades go to the API
server over a transport pinned to HTTP/1.1 (net/http would otherwise put a
SPDY upgrade on a pooled HTTP/2 connection). Why not yamux: a new
dependency for what HTTP/2 already does; why not `rest.Config.Dial`:
client-go's SPDY and WebSocket round trippers ignore it.

**Registry** (`clusters.Hub`, wired in `main.go` as `server.Config.Clusters`
and `.Tunnel`). `List` = `local` first, then every Cluster object (Connected
when its agent is). `RESTConfig(local)` = the console's own config;
`RESTConfig(remote)` = `http://127.0.0.1:<port>` with a per-session bearer
key: the console listens on a loopback port per connected cluster (only
reachable inside its pod) and the agent refuses requests without that key,
then replaces it with its own credentials. Callers add impersonation as
today (`kube.NewImpersonator(cfg, …)` works unchanged; the tests do exactly
that). A config dies with its session (connection refused): rebuild clients
on `Changed()`, which closes on every connect and disconnect.
`ErrUnavailable` for a known but disconnected cluster, `ErrUnknown` for no
Cluster object. `Hub.Agent(name)` reports versions, nodes, join material,
the agent's address and when it connected and last answered (polled every
30 s; two missed answers drop the session). One session per cluster: a new
connection replaces the old one (so a half-open connection never locks an
agent out; a stolen token is answered by rotating).

**Agent mode.** `kwerft agent --console-url https://<console>
--agent-token-file /etc/kwerft-agent/token` plus the console's reconciler
flags: the same reconcilers as in the management cluster (not the Cluster
reconciler), the tunnel (only on the leader), health probes on `:8080`, no
store, no UI. The token file is re-read before every dial (a rotated token
needs no restart). Backoff 1 s → 1 min with jitter, 2 min after a refusal.

**Chart.** `mode: console | agent`; agent mode needs `agent.consoleURL` and
mounts `agent.tokenSecret` (default `kwerft-agent`, key `token`, written by
the installer so the token is never in Helm values). It leaves out the
console Service, the SQLite PVC and the data key. Same service account and
`kwerft-controller` ClusterRole as the console, so the agent has exactly the
console's local rights (impersonating only the console's users and groups —
the tests prove `system:masters` is refused through the tunnel).

**Installer flags (exact).**

    install.sh --agent --console https://<console> --cluster-token kwag_<cluster>_<secret> [--version V] [--platform P] [--private-iface IF] [--lite] [--yes]

(`KWERFT_CONSOLE`, `KWERFT_CLUSTER_TOKEN` in the environment work too.)
`--console` must be `https://<hostname>[:port]`; the token must be an agent
token; `--agent` refuses `--domain`, `--config` and `--join`. Stages:
Preflight … Observability as a normal install (k3s server, Cilium, Traefik,
cert-manager, metrics/logs stack — every cluster runs its own), then
**Kwerft agent** (CRDs, Secret `kwerft-agent` from stdin, `cluster-local-join`,
the chart with `mode=agent`), no Handoff and no setup token. Re-running is
idempotent and is how a rotated token is installed. A server keeps its mode:
`--agent` on a console server, or a plain install on an agent server, exits
2. The console's install command is `curl -fsSL
https://raw.githubusercontent.com/ehilzinger/kwerft-install/main/install.sh
| sudo bash -s -- --agent --console https://<console> --cluster-token <token>`
(plus `--version <console version>` for release builds).

**Cluster reconciler** (`internal/controllers/cluster_controller.go`,
management cluster only). Creates Cluster `local` (provider `local`, status
from the console's own API: versions, nodes; re-created if deleted). Remote
clusters get the finalizer `kwerft.dev/cluster-cleanup`; status from the
agent: `Connected` (lastSeen at most once a minute), `Disconnected` once it
was seen, otherwise `Provisioning` (hetzner-cloud) or `Pending` (adopted;
reason `NoToken` without a token hash), `Failed` for a name that is not a
DNS label of ≤ 40 characters. Each pass calls `DisconnectUnless(name,
hash)`, so a token rotated or removed by any means (also kubectl) ends the
old session. Deleting: disconnect, delete every NodePool with
`spec.cluster == name` and wait until they are gone (W2's finalizer deletes
the servers by label), delete `cluster-<name>-agent`/`-join`, release.

**For W2 (hetzner-cloud).** The reconciler server-side applies NodePool
`<cluster>-control-plane` (role `control-plane`, `serverType`/`location`
from `spec.hetznerCloud`, `count` = `controlPlanes`, owned by the Cluster,
labels `kwerft.dev/cluster=<name>`), and Secret
`kwerft-system/cluster-<name>-agent` with `token` and `consoleURL`
(`https://<active console hostname>`) while the cluster has never
connected. The first server of that pool runs
`install.sh --agent --console <consoleURL> --cluster-token <token> --yes`
(add `--platform cloud`, `--version`); servers 2 and 3 join with the join
material from `cluster-<name>-join` once the first is connected. The agent
Secret is deleted after the first connection (and when the token is rotated
before it).

**API** (`internal/server/api_clusters.go`, owners and admins, every write
impersonated and audited): `GET /api/v1/clusters`, `GET
/api/v1/clusters/{name}` (with the cluster's NodePools), `POST
/api/v1/clusters` `{name, displayName?, provider: hetzner-cloud|adopted,
hetznerCloud?: {location, serverType, controlPlanes: 1|3}}` (adopted:
answers `token` and `installCommand` once), `DELETE
/api/v1/clusters/{name}`, `POST /api/v1/clusters/{name}/token` (rotate:
new token once, old agent disconnected). Names `local` and `connect` are
reserved. `GET /api/v1/clusters/connect` is the agents' endpoint.

**Security decisions.**
- The token (`kwag_<cluster>_<32 random bytes>`) is shown once; only its
  SHA-256 is kept, as an annotation on the Cluster, written as the signed-in
  user. So no user request needs the console's own rights to Secrets (the
  only Secret Kwerft writes for clusters is the reconciler's, for cloud-init).
- The connect endpoint refuses any request with an `Origin` (browsers),
  takes no session, compares in constant time, rate-limits refusals per
  client address (20 per 15 min) and audits them (`cluster.agent_rejected`).
- Rotation never pushes the new token through the tunnel: the agent that is
  connected may be the one holding a leaked token.
- The tunnel reaches only the cluster's API server; the loopback end needs
  the per-session key; the agent's credentials never leave its cluster.

**Tests.** `internal/clusters/tunnel_test.go` (fake TLS + HTTP/2 API
server: log follow streaming, exec over WebSocket with client-go's
executor, a SPDY-style upgrade proven to reach the API server over
HTTP/1.1, session key enforcement, refused tokens and Origins, reconnect,
rotation, replacement); `tunnel_envtest_test.go` (two real API servers:
the agent's identity, impersonation as viewer/owner incl. RBAC denials and
`system:masters` refused, a project created through the console's
Impersonator lands in the remote cluster only, watches, disconnect);
`internal/controllers/cluster_controller_test.go` (local, adopted with join
material and rotation, hetzner-cloud pool/secret/delete, invalid names);
`internal/server/clusters_test.go` (API roles, validation, adopt → a real
agent connects → rotate → refused and audited, delete; connect endpoint
refuses sessions and Origins and rate-limits); bats for `--agent`.

**Contract with W4 (checked after W4 landed).**
- `RESTConfig(remote)` is a real `http://127.0.0.1:<port>` Host, so
  `rest.HTTPClientFor`, service-proxy paths and `/k8s/` paths work. Each
  agent session has its own port and key: a reconnect (or a newer
  connection replacing an older one) closes `Changed()` and yields a new
  Host, so "keep the connection while Connected and Host is unchanged" is
  safe; the old config gets connection refused. `clusters.Clients` keeps
  one controller-runtime client per cluster on that rule.
- The agent's identity is the chart's `kwerft-controller` ClusterRole
  (list/watch every kwerft.dev kind and namespaces, create Builds,
  impersonate the console's users and role groups) plus the Role
  `kwerft-observability-proxy` (get/create `services/proxy` in
  `kwerft-observability` only), bound to the console's service account in
  both modes. `tunnel_envtest_test.go` asks the remote API server through
  the tunnel for each of these, and that `services/proxy` elsewhere and
  `system:masters` are refused.
- Streaming and upgrades: watches, log follows (`FlushInterval: -1`),
  exec over WebSocket and the SPDY fallback (client-go's fallback executor
  against an SPDY-only endpoint) are tested through the tunnel.
- `List()` reads Cluster objects from the manager's cache: no API call.

**Mirroring notification channels and Git connections**
(`internal/controllers/cluster_mirror.go`). W4 keeps NotificationChannels
and GitConnections in the management cluster only; remote clusters still
need receivers and clone credentials. On every pass of a *connected* remote
cluster (each minute, on connect, and at once when a channel or connection
changes its spec or annotations — the console sets
`kwerft.dev/credentials-updated` after writing credentials) the Cluster
reconciler, through the tunnel with Kwerft's own identity:
- server-side applies (field manager `kwerft-mirror`) a copy of each
  NotificationChannel and GitConnection (spec only, no status), labelled
  `kwerft.dev/mirrored=true`;
- applies its Secret — `notify-<name>` in `kwerft-observability`,
  `git-<name>` in `kwerft-builds` — with the original's data, labelled
  `kwerft.dev/mirrored=true` and with the usual
  `kwerft.dev/notification-channel` / `kwerft.dev/git-connection` label,
  controlled by the copy (the remote reconcilers never adopt another
  owner's Secret);
- deletes copies (and their copied Secrets) whose original is gone;
- leaves an object of the same name that the remote cluster made itself
  alone, and says so in the Cluster's condition `Mirrored` (False with the
  names; True when all are in sync).
The remote reconcilers then render the Alertmanager configs and check the
credentials there. Webhooks stay the management console's: in agent mode
there is no console hostname, so no webhook URL. Secrets are read uncached;
a credential change without the annotation arrives within a minute.

**Limits and open points.**
- Logs through a *real* API server are not in the envtest (no kubelet);
  the fake API server covers streaming. Port-forward is the same upgrade
  path as exec but untested end to end.
- One console replica: sessions live in its memory (as SQLite already
  requires). Each console restart drops all agents for their backoff (≤ 1 min).
- Per-cluster throughput is one TCP connection (WebSocket through Traefik);
  fine for API traffic, not for bulk data.
- The agent runs the full reconciler set, including the DNS reconciler with
  no console hostname; per-cluster apps domains are W4's.

## As built (W4)

- **Seam.** `server.Config.Clusters` is the `clusters.Registry`;
  `BaseContext` bounds remote caches. `main.go` passes `clusters.Static`
  until W3's tunnel Registry replaces it. The local cluster is reached
  exactly as before (`Kube`, `KubeCache`, `System`, `SystemReader`,
  `Metrics`, `LogsURL`, `Hubble`); with no Registry or `Static` there are no
  lookups and no new requests (`internal/server/clusters.go`).
- **Following the Registry.** Synced lazily: a request that finds
  `Changed()` closed re-lists; a failed `List` or connect is retried after
  30 s. Per connected remote cluster the console builds, from
  `RESTConfig(name)`: an Impersonator (one HTTP client and RESTMapper per
  cluster), Kwerft's own client, an informer cache (started under
  `BaseContext`, stopped when the cluster disconnects), and VictoriaMetrics,
  VictoriaLogs and Alertmanager clients on service-proxy URLs
  (`observability.ServiceProxyURL`, `logs.NewWithHTTP`). A connection is
  kept while the cluster stays connected and its `Host` is unchanged.
- **Project → cluster.** `requireUser` resolves every `{project}` route to
  the project's cluster and serves it there, impersonated, so that
  cluster's RBAC decides. The index comes from each connected cluster's
  Projects (cache), is refreshed on a miss (at most once a second) and
  checked per request with a cache get. A name no cluster has goes to
  `local` (the API server's own 403/404, as before). A project last seen in
  a cluster that is now unreachable answers **503**
  `{"code":"clusterUnreachable","cluster":…}` with a sentence naming both;
  a name found in two clusters answers **409** `projectConflict` — the
  console never guesses.
- **Creating projects.** `POST /projects` takes `cluster` (owners and
  admins; others 403 on `cluster`; unknown 404; unreachable 503). The name
  check reads every connected cluster uncached and the last-known index for
  unreachable ones (422 on `name`), serialized by a mutex with the create.
  **Residual races:** a Project created past the console (kubectl, also
  through `/k8s`) in another cluster at the same moment, or in a cluster
  that is unreachable and was never indexed; and a second console replica
  (the mutex is per process — Kwerft runs one console). Either ends as a
  409 conflict on that name's routes, never as a request in the wrong
  cluster.
- **Lists** (`visitClusters`): projects, apps, tasks, schedules, volumes,
  domains, alerts, silences and alert rules visit every connected cluster,
  each with the user's project scope *there* (scope.go reads the cluster
  from the context; token restrictions apply per cluster), and every item
  carries `cluster`. `?cluster=` or `?project=` narrows a list to one
  cluster (503 when it is unreachable). A remote cluster that is
  unreachable or fails is left out and named in the
  `Kwerft-Unreachable-Clusters` header; the local cluster failing fails the
  list as before. Shell recordings carry `cluster` (older ones: `local`)
  and filter by `?cluster=`.
- **One cluster at a time** (`?cluster=`, or `?project=` naming its
  cluster; default `local`): metrics overview and explorer (responses carry
  `cluster`), log search and tail (`cluster`), traffic drops, settings
  (`GET`, apps, DNS check; the console hostname always `local`), every
  firewall route, alert rule update and delete, silence delete (without it,
  the Alertmanager that has the ID). A silence is created in the cluster
  named in its body, else the one whose Alertmanager has the fingerprint,
  else the cluster of its namespace matcher's project. A rule is created in
  `cluster`, else the cluster of the projects it watches (projects of two
  clusters: 422 on `scope`), else `local`.
- **Streams** go to the project's cluster: pod logs, shells (exec over
  WebSocket/SPDY through the agent), build logs (Kwerft's own clientset
  there, after the user's impersonated get of the Build), and log history.
- **Hubble** is read in the management cluster only: a remote project's
  traffic tab shows the counts its own reconciler wrote into the rules'
  status and says that live flows are not available; dropped connections of
  remote clusters are not shown (**open**).
- **Git**: webhook deliveries build matching Apps in every connected
  cluster (Builds created with Kwerft's identity there); "Build now" and
  app create/update read the Git connection in the management cluster as
  the user.
- **kubectl**: `/k8s/clusters/<name>/…` proxies to another cluster with
  the same rules; a project-limited token reaches a project's namespace only
  in the project's cluster. A kubeconfig has one context per cluster
  (`kwerft-<domain>-<name>`; the management cluster's stays current).
- Removing a console account takes it off the projects of every connected
  cluster (unreachable ones are logged). A remote cluster failing during a
  request (not an API status) answers 503 `clusterUnreachable`.
- `GET /api/v1/cluster-status` → `{"clusters":[{"name","connected"}]}` for
  every signed-in user (API tokens too): the UI's columns, filter, picker
  and banner.
- **UI**: a cluster badge in the apps, volumes, schedules, runs, alerts and
  alert rules lists; a cluster filter on Apps, Volumes and Jobs, remembered
  per user in the browser (`kwerft:cluster-filter:<user id>`); the cluster
  picker in "New project" (unreachable clusters disabled); the Deploy
  wizard names each project's cluster; a cluster picker on Monitoring ›
  Metrics and on Monitoring › Logs (without a project); a banner on every
  page while a cluster is unreachable. All of it renders nothing while only
  `local` exists. Not built: a cluster picker on Settings (apps domain) and
  Network › Firewall — their API takes `?cluster=`; W1 owns those pages
  this phase.
- **Tests**: `internal/server/multicluster_test.go` runs a second envtest
  cluster ("edge", same CRDs, RBAC and reconcilers) behind a test Registry
  with the edge's console identity, so remote connections, caches and
  impersonation are the production code. `TestProjectIsolationAcrossClusters`
  extends the isolation suite: a developer of a project in `local` cannot
  reach a Members project in the edge through lists (every filter), single
  objects, writes, streams, shells, metrics, log search, alerts, silences,
  rules, project access or kubectl (`/k8s/clusters/edge`), while her Team
  project in the edge works and owners and admins reach everything.
  `TestMultiClusterProjectsAndLists` covers creation, unique names,
  aggregation, an unreachable cluster (503, header, banner data) and its
  return, and name conflicts; `clusters_test.go` the Registry lifecycle.

### What W3's Registry must provide (beyond the interface)

- `RESTConfig(name)` usable with `rest.HTTPClientFor`: a `Host` URL (the
  service-proxy and kubectl paths are built on it) and a transport that
  dials through the cluster's *current* tunnel session; the console keeps a
  connection while `List` says Connected and `Host` stays the same, so a
  reconnect must either keep the config valid or show up as a disconnect
  and connect (`Changed()` closed in between).
- The identity it carries needs, in the remote cluster, what the console's
  service account has locally (impersonate the console's users and role
  groups; list/watch the kinds of the lists and Namespaces; the
  reconcilers' reads) **plus** `get`/`create` on `services/proxy` in
  `kwerft-observability` (VictoriaMetrics, VictoriaLogs, Alertmanager), and
  create on `kwerft.dev` Builds (webhook builds).
- The tunnel must carry streaming responses (watches, log follow, the
  VictoriaLogs tail) and protocol upgrades (exec over WebSocket, SPDY
  fallback) to the remote API server.
- `List` should be cheap (it runs on every Registry change) and name every
  managed cluster, connected or not, so lists can say which are
  unreachable.

## As built (W5)

- **Harness** `hack/e2e` (Go, `package main`): `run` (create, install,
  check, destroy), `sweep`, and `previous`/`latest` (pick versions from the
  release tags). Its own minimal Hetzner Cloud client and fake
  (`hcloud.go`, `hcloud_fake_test.go`) — servers, SSH keys, server types,
  images — deliberately not `internal/hetzner`, so it neither depends on nor
  conflicts with W2's server and SSH key files there; it can move onto W2's
  client later.
- **Server**: the first available of `cx33`, `cx43` (8 GB: the platform
  plus a Git build do not fit 4 GB; `cx23`, the `cx22` successor, is one
  variable away) in `nbg1`, `fsn1`, `hel1`; `ubuntu-26.04`, else `24.04`.
  Labels `kwerft-e2e=true`, `run`, `created`. A per-run ed25519 key is
  uploaded and deleted; cloud-init installs a per-run host key that the
  harness pins (no trust on first use).
- **Installer**: the published `kwerft-install/main/v<version>/install.sh`,
  downloaded on the server, with `--domain <ip>.sslip.io --acme-server
  staging --yes`. `--acme-server URL|staging` (env `KWERFT_ACME_SERVER`) is
  new in `install.sh`: it sets the chart's `acme.server`; exit codes
  unchanged. Releases without it (≤ v0.4.0) get their ClusterIssuer patched
  to staging and the Gateway's Certificates re-requested right after the
  installer, so upgrade runs from them stay off the production rate limit.
- **Owner**: through the setup token (`/etc/kwerft/setup-token` over SSH →
  `/api/v1/setup/verify` → `/setup/owner`), not `--config`: the console does
  not read the `kwerft-bootstrap` Secret yet, and `--config` suppresses the
  setup token, so an install with `--config` has no way to create the owner
  (README documents `owner:` in the config). Works on every release, and is
  the path users take.
- **Checks** (`run.go`): console on HTTPS with a real ACME certificate
  (staging accepted only with a `(STAGING)` issuer and a matching name),
  version, owner, sign-in; then, side by side so each is timed from what it
  waits for: image App on HTTPS (fresh server → app, 10 min) → Task
  succeeds and restarts it → log search finds its output; Git build of
  `traefik/whoami` deploys and answers (3 min); metrics explorer series;
  crash-looping alert in `/api/v1/alerts` (2 min). Budgets mark a check
  *slow*, deadlines fail it. Upgrade: owner and an App before, version /
  sign-in / App after, then the same checks.
- **Cleanup**: deferred, on its own context (survives timeout, SIGINT,
  SIGTERM); deletes server and key and waits for the server to be gone; a
  failed deletion fails the run. The workflow adds `sweep -run <id>` with
  `if: always()`, and a scheduled sweeper (every 3 h and nightly) deletes
  `kwerft-e2e=true` resources older than 3 h.
- **Workflow** `e2e.yml`: `workflow_call` (from `release.yml`'s new `e2e`
  job after `release` and `install-repo`; `!cancelled()`, so a skipped
  install-repo shows up as "Installer not published"), nightly, dispatch;
  skips with a notice without `HCLOUD_TOKEN`; a resolve job checks that the
  installers exist before paying for servers; matrix fresh/upgrade, job-level
  concurrency per version and scenario (never cancelled mid-run), 100 min
  job timeout over the harness's 80. Results only in the run summary.
- **Not verified on Hetzner yet** — the fakes cover the flow, failures,
  timeouts, cancellation, fallbacks and cleanup; the first real run is the
  coordinator's (RELEASING.md › e2e install runs).

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
