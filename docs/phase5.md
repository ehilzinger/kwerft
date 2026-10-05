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
- As built (W3): every installer run on a k3s server writes
  `cluster-local-join` in its own cluster's `kwerft-system`
  (`write_join_secret`); in the management cluster that *is*
  `cluster-<local>-join`, in a remote one the agent reports it and the
  Cluster reconciler copies it to `cluster-<name>-join` (owned by the
  Cluster). The token hash lives on the Cluster (annotation
  `kwerft.dev/agent-token-hash`), not in the Secret, and
  `cluster-<name>-agent` exists only for a hetzner-cloud cluster until its
  agent first connects (keys `token`, `consoleURL`); see As built (W3).

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
