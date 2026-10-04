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
