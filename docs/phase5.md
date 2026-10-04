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
