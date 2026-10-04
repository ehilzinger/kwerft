# Kwerft implementation plan

Kwerft turns a rented Hetzner server (Cloud or dedicated) into a Kubernetes
platform with a web console, installed by one bash script. This document is
the working plan; `docs/blueprint.html` holds the clickable UI mockups and
the same plan in long form.

## Decisions (2026-10-04)

| Topic | Decision | Consequence |
|---|---|---|
| Hosting target | **Hetzner Cloud and dedicated, both first-class** | Installer detects the platform (Cloud metadata service). Storage, private networking and firewall defaults differ per platform. CI covers both. |
| Users | **One team** | Single organization with projects and roles; no tenant isolation in v1. Records carry an organization ID so tenancy can be added later without a migration. |
| Sources | **Registry images and Git repositories in v1** | Adds Phase 2: the `Build` resource, rootless BuildKit, Railpack, an in-cluster zot registry. |
| License | **To be decided before the public beta** | All dependencies chosen so far are Apache-2.0 or MIT, so every option stays open. No LICENSE file until then. |
| Jobs | **One-off and scheduled jobs in v1** (`Task`, `Schedule`) | Added to Phase 1. Real workloads are more than long-running services: the first pilot (hatchure, 2026-10-04) has eight cron jobs and a dozen jobs started by hand next to its six services. |
| Local development | **Kwerft for Mac, a native app on Apple's `container`, after the beta** (Phase 7) | Runs the same Kwerft binary and resources in a local Kubernetes VM, so a Project developed on a Mac can be pushed to a Hetzner instance unchanged except for per-target overrides. Apple silicon and macOS 26+ only. |
| Local cluster for Kwerft for Mac | **k3s in an Apple `container machine`, set up by `install.sh`, on Kwerft's own kernel** (spike, 2026-10-04: `docs/spike-mac.md`) | The machine survives restarts with its volumes; `container k8s` (kind) cannot come back after a stop. The production stack runs unchanged through the same installer. Neither kernel `container` offers runs Cilium, so the app ships a kernel built from Apple's config plus `hack/spike-mac/kernel/kwerft.config`. |
| Name & hosting | **Kwerft, under the personal GitHub account `ehilzinger`** | Module `github.com/ehilzinger/kwerft`, image `ghcr.io/ehilzinger/kwerft`, chart `oci://ghcr.io/ehilzinger/charts/kwerft`. Can move to an organisation later. The installer is served from GitHub raw until Kwerft has its own domain (then `get.kwerft.dev`). |
| Distribution (2026-10-04) | **Code private, container packages public** | Image and chart on GHCR are public so servers install without credentials; the install script is published to a small public companion repo (e.g. `ehilzinger/kwerft-install`), because raw files of a private repo are not reachable. |
| Members (2026-10-04) | **Invites pulled forward into Phase 1** | Owner/admin invite by email with a role via a single-use link (no email sending yet); member list, role changes, removal, last-owner protection. Per-project roles stay in Phase 4. |
| DNS records (2026-10-04) | **Kwerft keeps A/AAAA records for the console hostname and `*.<appsDomain>` only (Settings switch, `--config dns.records`)** | One wildcard record covers every app, whatever the certificate method, so app deploys never write DNS and developers cannot place records in the zone. RRsets Kwerft creates carry `kwerft.dev/managed-by` and `kwerft.dev/instance` (kube-system UID) labels; records without them are never changed (reported as conflicts), a reinstalled server takes over the old installation's records, and the old one yields. Turning it off leaves the records. Per-Domain records for custom hostnames: later, opt-in. |
| DNS-01 provider (2026-10-04) | **Hetzner DNS through the Cloud API, official `hetzner/cert-manager-webhook-hetzner` (0.9.0)** | The old DNS Console API (dns.hetzner.com) was shut down in May 2026; zones now live in the Cloud API and use a Cloud project token. The webhook's chart may read every Secret; the installer narrows it to the one token Secret. |
| Test servers (2026-10-04) | **Hetzner Cloud now, a dedicated server later** | The e2e harness runs on Cloud servers with a test-project API token stored as a GitHub secret; the dedicated-server run follows when one is available. Deferred to Phase 4, then to Phase 5 with the rest of the Hetzner Cloud API work (2026-10-04): until then releases are checked by hand on the test server. |
| Build registry (2026-10-04) | **zot (`zot-minimal`, no extensions) in the Kwerft chart; nodes pull `registry.kwerft.internal:5000` through a k3s mirror to a fixed ClusterIP (`10.43.0.50`)** | One Deployment, PVC, Service and policy are simpler than a second release, and zot upgrades with Kwerft. containerd on the host cannot resolve cluster DNS, so `registries.yaml` names the ClusterIP, which Cilium's socket load balancer serves to host processes on every node. Not a NodePort: Cilium answers NodePorts in eBPF before the nftables host firewall, which would publish an unauthenticated registry. Plain HTTP inside the cluster (WireGuard between nodes); a CiliumNetworkPolicy admits only build pods, the console and the nodes (`host`, `remote-node`), which a Kubernetes NetworkPolicy cannot name. Retention per App repository: `buildcache`, the newest 20 tags and every tag pulled within 90 days. The installer restarts k3s only when `registries.yaml` changed and never edits a file it did not write. |
| Build tools (2026-10-04) | **Rootless BuildKit (`moby/buildkit:<v>-rootless`) and the Railpack frontend image (`ghcr.io/railwayapp/railpack-frontend`, which carries the `railpack` CLI at `/railpack`)**, pinned in `install.sh` | Builds run in `kwerft-builds`, the only namespace with Pod Security `privileged` (rootless BuildKit needs seccomp and AppArmor `Unconfined`); a LimitRange and ResourceQuota cap them, and a NetworkPolicy allows DNS, zot and the internet (no private ranges, no metadata service, nothing in the cluster). |

## Principles

1. **One command, zero follow-up.** The installer is idempotent and fully flag-driven; re-running repairs or upgrades. Cloud-init can call it unattended.
2. **Kubernetes is the database.** Apps, rules and domains are custom resources; the UI writes them and reconcilers render native objects. GitOps works for free.
3. **Fits an 8 GB box.** The platform uses about 2.5 GB of RAM (measured), so an 8 GB server is the recommended size; 4 GB works with `--lite` and little else.
4. **Secure by default.** Projects isolated, Kubernetes API private, secrets encrypted, every shell session and change audited.
5. **Never a dead end.** YAML export, scoped kubeconfig, `kubectl` all keep working.

## Architecture

```
Clients        browser · kwerftctl · CI tokens · kubectl (scoped kubeconfig)
Edge :80/443   Traefik (hostNetwork DaemonSet, Gateway API) + cert-manager
Kwerft          one Go binary: REST/WebSocket API, reconcilers, embedded React UI, SQLite
Platform       VictoriaMetrics · VictoriaLogs + Vector · Hubble · Velero · BuildKit · zot
Kubernetes     k3s (embedded etcd, secrets encryption) · Cilium (kube-proxy replacement, WireGuard)
Host           Ubuntu 22.04/24.04/26.04 · nftables baseline · chrony · unattended-upgrades
Hetzner        Cloud API (servers, networks, firewalls, volumes, LBs) · Robot · Object Storage · DNS
```

A change flows: UI → API checks the role → API writes the custom resource
**impersonating the user** (Kubernetes RBAC is the final gate) → reconciler
renders Deployment/Service/HTTPRoute/CiliumNetworkPolicy/PVC with server-side
apply → status streams back over WebSocket from a shared informer cache.

Remote clusters run `kwerft-agent`, which dials out to the console over an
mTLS WebSocket tunnel, so they need no public Kubernetes API.

## Technology choices

| Layer | Choice | Rejected |
|---|---|---|
| Kubernetes | k3s | kubeadm (ops), RKE2 (heavier), Talos (needs OS reinstall) |
| Networking | Cilium — eBPF policies, Hubble flows, WireGuard | flannel (no flow visibility), Calico |
| Ingress | Traefik via Gateway API | ingress-nginx (retired), Envoy Gateway |
| TLS | cert-manager; HTTP-01, DNS-01 via Hetzner DNS for wildcards | Traefik ACME |
| Storage | local-path (NVMe) → hcloud CSI on cloud nodes; Longhorn opt-in on dedicated | Rook/Ceph |
| Metrics / logs | VictoriaMetrics, VictoriaLogs + Vector | kube-prometheus-stack, Loki |
| Builds | rootless BuildKit Jobs, Dockerfile or Railpack, zot registry | Kaniko (archived), Cloud Native Buildpacks |
| Backups | etcd snapshots + Velero (Kopia) → Hetzner Object Storage | custom scripts |
| Backend | Go, controller-runtime | Node, Rust |
| Frontend | React, TypeScript, Vite, TanStack Query/Router | SvelteKit, Vue |
| Platform DB | SQLite (users, sessions, tokens, audit) + Litestream | Postgres (later, for HA) |
| Identity | argon2id, passkeys/TOTP, OIDC | embedded Dex |

Memory, measured 2026-10-04 on an idle Hetzner Cloud server (8 GB, Ubuntu
26.04, k3s v1.37.1, one demo app):

| Component | Estimate | Measured |
|---|---|---|
| k3s process (API server, embedded etcd, kubelet, containerd) | 600 MB | **1.3 GB** |
| Cilium agent, operator, Envoy, Hubble relay | 300 MB | 300 MB |
| Traefik, cert-manager | 180 MB | 125 MB |
| VictoriaMetrics stack, VictoriaLogs, Vector, node-exporter | 550 MB | 625 MB |
| CoreDNS, metrics-server, local-path | — | 45 MB |
| Kwerft | 150 MB | 13 MB |
| zot registry (Phase 2) | 60 MB | 55–60 MB RSS (`zot-minimal` v2.1.21 measured locally, idle and after pushes; limit 512 MiB) |
| **Platform total** | **≈ 1.9 GB** | **≈ 2.4 GB** (host: 3.0 GB used incl. OS) |

Plus 1–2 GB per running build. Recommended server: 8 GB. k3s dominates;
`--lite` defaults should target the observability stack (the next-largest
item) — e.g. drop vmalert/Alertmanager and shorten retention.

## Installer (`install/install.sh`)

Stages, each idempotent and recorded in `/var/lib/kwerft/stages/`:

1. **Preflight** — platform detection, root, Ubuntu version, arch, RAM, disk, cgroup v2, ports 80/443, outbound HTTPS.
2. **System** — packages, kernel modules, sysctls, swap off, chrony, unattended-upgrades, optional SSH hardening.
3. **Firewall** — own `inet kwerft` nftables table: 22/80/443 public, cluster traffic only from the private network and pods.
4. **Kubernetes** — k3s server from `/etc/rancher/k3s/config.yaml` (no flannel, no kube-proxy, no bundled Traefik/servicelb).
   **Registry mirror** — `/etc/rancher/k3s/registries.yaml` maps `registry.kwerft.internal:5000` to zot's ClusterIP; k3s restarts only when the file changed (also on joined nodes).
5. **Helm**, **Network** (Cilium), **Ingress & TLS** (Gateway API CRDs, cert-manager, Traefik), **Observability**.
6. **Kwerft** — Helm chart from the checkout or the OCI registry; `--config` becomes the `kwerft-bootstrap` Secret.
7. **Handoff** — DNS check, single-use setup token (hash only in the cluster), summary.

Without `--domain` the console gets a temporary `<public-ip>.sslip.io` hostname
(public wildcard DNS) so a trial works with zero DNS setup; the installer warns
about it, and re-running with `--domain` switches over. The chosen hostname is
saved in `/var/lib/kwerft/domain`, so re-running without flags never changes it.
`--email` is optional.

Exit codes: 0 ok · 2 usage · 10 preflight · 20 network/DNS · 30 Kubernetes · 40 platform · 50 Kwerft.

## Resource model (`api/v1alpha1`)

| Resource | Becomes | Status |
|---|---|---|
| `Project` (cluster-scoped) | Namespace, quota, Pod Security level, default-deny CiliumNetworkPolicy when isolated (`spec.isolated`, default; off: every project's pods reach its apps), RoleBindings for its access (Team or Members) | reconciler ✔ |
| `App` | Deployment, or StatefulSet when it has disks of its own (shared Volumes keep it a Deployment); Service, HTTPRoute per public port, CiliumNetworkPolicy (the ingress via host/remote-node, platform namespaces, `allowFrom`; egress none, https or all); source = image **or** Git. Rollback runs an earlier revision's image again as a new revision (`GitSource.pinnedImage` for Git apps); restart via the `kwerft.dev/restarted-at` annotation, no new revision | reconciler ✔, API ✔ (HPA later) |
| `Volume` | PVC (local-path / hcloud-volumes) that Apps and Tasks of a project mount by name; deletion waits while mounted | reconciler ✔ |
| `Task` | Job (kwerft-batch priority, deny-ingress policy): a one-off run with App's shape, or `fromApp`; "run now" with `envOverrides` | reconciler ✔ |
| `Schedule` | Tasks on a cron schedule, scheduled by the reconciler (a CronJob could not create Tasks without RBAC in pods) | reconciler ✔ |
| `Build` | Job running rootless BuildKit; pushes to zot; success creates an App revision | types ✔ |
| `GitConnection` (cluster-scoped) | GitHub App / GitLab / Gitea / deploy key credentials and webhooks | types ✔, Phase 2 |
| `Domain` | Gateway listener + certificate via cert-manager, or the shared apps wildcard listener for names one level below the apps domain; Apps create one per public port; the older claim wins; hostname fixed after creation; max 59 per-host listeners | reconciler ✔ (records: the apps wildcard record covers Domains under the apps domain; others stay manual) |
| `ConsoleSettings` (singleton `kwerft`) | Console hostname (with a staged move), apps domain, certificate method (HTTP-01 per host or DNS-01 wildcard via Hetzner), managed DNS records (`spec.dns.manageRecords`, written by the DNS reconciler into `status.dns`); the DNS token lives in the write-only Secret `kwerft-dns-token` | reconciler, API, UI ✔ |
| `TrafficRule` | CiliumNetworkPolicies `<rule>.traffic-in` (this project's apps receive) and `<rule>.traffic-out` (they send out), additive only; crossing projects needs a rule on the receiving side; Hubble allowed/dropped counts in status | reconciler ✔, API ✔, UI ✔ |
| `FirewallRule` (cluster-scoped) | Host rules through a node agent (Phase 4); Hetzner Cloud Firewall sync (Phase 5) | types ✔ |
| `NodePool`, `Cluster` | Hetzner Cloud servers + cloud-init join; agent for remote clusters | Phase 5 |
| `AlertRule`, `NotificationChannel` (cluster-scoped) | VMRule + Alertmanager route and receivers | types ✔, Phase 3 |
| `BackupPlan` | Velero Schedule | Phase 6 |
| `Environment` (name tentative) | Per-target overrides (hostnames, storage class, quota, replicas) applied when a Project is pushed to another instance | Phase 7 |

### Jobs (`Task`, `Schedule`)

- **Same shape as an App.** A Task takes the App spec's source (image or
  Git), command, env, volumes, resources and egress, minus ports, replicas
  and health checks, so the console form and the reconciler code are shared.
  It runs under the project's quota, Pod Security level and default-deny
  policy like any App.
- **Task** fields beyond that: `timeout` (`activeDeadlineSeconds`),
  `retries` (`backoffLimit`, default 0), `onSuccess.restart` (Apps in the
  same project to roll out once it succeeds, e.g. a server that has to
  reopen a file the job replaced). The reconciler does the restart, so a job
  never needs RBAC of its own. Finished Tasks are kept for their logs
  (`ttlSecondsAfterFinished`, default 7 days).
- **Schedule** fields: `schedule` (cron), `timeZone` (default the server's),
  `suspend`, `concurrency` (`Forbid` by default, `Replace`, `Allow`), and
  `history` (last N successful and failed Tasks). Each run is a Task, so a
  scheduled run and a manual one look the same in the console. Status
  carries `lastSuccessTime` and `lastFailureTime`, exported as metrics so an
  alert can fire on a stale schedule (Phase 3).
- **Run now:** the console and API create a Task from an App or a Schedule,
  with env overrides (`FORCE=1`, `DRY_RUN=1`), impersonating the user like
  every other write.
- **Console:** a Jobs tab per project listing Schedules (next run, last
  result, suspend toggle) and Tasks (status, duration, exit code, live log).
- **Batch priority:** Tasks run in a `kwerft-batch` PriorityClass below Apps,
  so under memory pressure the scheduler and kubelet pick a job before a
  service.
- **Shared volumes:** a `Volume` is a ReadWriteOnce disk that Apps and Tasks
  mount by name (`volumes: [{path, volume}]`); an entry with `size` stays a
  per-replica disk. Pods sharing a Volume prefer one node. Deleting a
  mounted Volume waits (finalizer, reason `InUse`).
- **As built (2026-10-04):** `fromApp` inherits the App's image, command,
  env, size, egress and shared Volumes (env order App → `env` →
  `envOverrides`), and its pods get the App's network identity, so whatever
  `allowFrom` admits the App also admits its Tasks. The Task spec is
  immutable; a finished Task without a Schedule is deleted after its TTL.
  Schedules default `timeZone` to the server's (UTC in the container) and
  have `startingDeadline` (default 1h): only the latest missed run within it
  starts, and with `Forbid` a due run waits for the active one until then.
  `history` keeps 3 succeeded and 3 failed by default. Verified on the test
  server: a writer Task fills a shared Volume and restarts an App, a reader
  Task sees the data, a Schedule fires on the minute.

## Security model

- Console unusable until the setup token from the server's disk is presented; no default passwords.
- **Identity:** the API reaches Kubernetes as user `kwerft:<email>` in groups `kwerft:role:<role>` and `system:authenticated`; the console's service account may impersonate only those groups (never `system:masters`). Writes and single-object reads go through impersonation; the polled list views (Projects, Apps, Tasks, Schedules, Volumes, Domains, which omit env) read the informer cache confined to the user's project scope (`internal/server/scope.go`), as do metrics, log search, alerts and recordings.
- **Roles and project access (Phase 4):** owner and admin manage everything in kwerft.dev in every project (cluster-wide bindings). Developers and viewers hold only cluster-scoped reads cluster-wide (projects, Git connections, alert rules, notification channels, settings, firewall rules; developers write alert rules); everything in a project namespace comes from the Project reconciler's RoleBindings there (`kwerft:project-developer`/`-viewer`, `kwerft:pods-read`/`-exec`): to the role groups for projects with access Team (the default, so existing installs keep working), to the listed users `kwerft:<email>` with their project role for access Members. No role reaches Secrets through the API. `internal/server/isolation_test.go` proves it against a real API server.
- **Sign-in:** argon2id passwords; TOTP, passkeys (also passwordless) and single-use recovery codes, which exist only while a TOTP app or passkey does. Optional per member unless an owner requires two-factor sign-in: members without a factor are then held to the enrolment endpoints after the password (never locked out), and nobody removes their last factor. Owners and admins (admins not for owners) reset a member's factors, which signs them out; audited. Adding a factor needs the password again. Sessions are `__Host-` HttpOnly cookies, 7 days idle / 30 days absolute; same-origin check on every write; rate limits on setup tokens, passwords and second factors. Client addresses (rate limits, audit, firewall lock-out check) come from `X-Real-Ip` only when the TCP peer is a node address — Traefik on the host network — and otherwise from the peer.
- **Single sign-on (Phase 4):** OpenID Connect code flow with PKCE, state and nonce (Google, Microsoft Entra single tenant, Keycloak, generic). Users are matched by a verified email once, then by the provider's subject; one link per provider and user. Only members and open invites sign in, unless auto-join (developer or viewer, allowed domains required) is on. A member's own passkey or authenticator app is still asked for after the provider. Settings in `ConsoleSettings.spec.sso`, the client secret in the write-only Secret `kwerft-oidc-client`.
- **API tokens and kubectl (Phase 4):** `kwft_` tokens (SHA-256 stored, shown once) with a role cap (never above the user's current role), an optional project restriction (developer or viewer), 90 days by default and a year at most; they manage no identity (accounts, tokens, members, sign-in settings) and open no shells. Downloaded kubeconfigs point at the console's proxy `/k8s/`, which impersonates the token's user and refuses exec/attach/port-forward/proxy, protocol upgrades, Secrets and `--as`.
- **Pods, logs, shells:** pods, logs and pod metrics are readable by every role; exec is for owner, admin and developer. Both are bound **per project namespace** by the Project reconciler (`kwerft:pods-read`, `kwerft:pods-exec`), never cluster-wide, so no role reaches `kube-system` or `kwerft-system` pods. Shell WebSockets require an Origin naming this console and a valid session before the upgrade; 15 min idle, 1 h maximum, 3 shells per user. Sessions are recorded as asciinema v2 (output only — keystrokes are not recorded because unechoed input is mostly passwords), 0600 on the data volume, kept 90 days, 64 MiB per session; owners and admins download them, audited. Logs stream over SSE with caps (16 KiB lines, 200 lines/s, 15 min idle, 2 h maximum). Verified on the test server (2026-10-04).
- **Secrets at rest:** TOTP seeds in SQLite are encrypted (AES-GCM) with `KWERFT_DATA_KEY` from the Secret `kwerft-data-key`, which the chart creates once and keeps across upgrades and uninstalls. Back it up together with the database. Sealed values name their key, so it can be rotated: Settings › Data key (owners, with their password) writes a new key to the Secret, re-seals everything and retires the old one. By hand: put the new key in `key` and the old one in `previous`, restart the console (it re-seals at start-up), then remove `previous`. Backups made before a rotation need the old key.
- Kubernetes API on the private network only; external `kubectl` through Kwerft's proxy with short-lived scoped kubeconfigs.
- k3s secrets encryption; developers write but cannot read secrets unless granted.
- Exec sessions role-gated, time-limited and recorded.
- Default-deny between projects, WireGuard between nodes, host firewall with lock-out protection.
- API tokens scoped, expiring, stored hashed. Signed images, pinned digests, SBOMs.

## Roadmap (~28 weeks to the public beta, then 8 for the Mac app; 1–2 engineers)

| Phase | Weeks | Scope | Exit criterion |
|---|---|---|---|
| 0 Foundations | 1–2 | Repo, CI, chart, CRDs, installer stages 1–4 on Cloud **and** dedicated, memory budget | All stages pass on a fresh Cloud server (automated runs: Phase 4) |
| 1 Installer & deploy MVP | 3–8 | Full installer, setup wizard, auth, Project/App/Domain/Task/Schedule reconcilers, apps and jobs UI, logs, shell, rollback | Fresh server → app on HTTPS in < 10 min; a scheduled job runs and restarts an app on success |
| 2 Builds from Git | 9–12 | GitConnection, webhooks, Build reconciler, BuildKit, Railpack, zot, auto-deploy, commit checks | Push to main live in < 3 min with build log and commit check |
| 3 Monitoring & logs | 13–15 | VictoriaMetrics/Logs, charts, log search, alerts, notification channels | Crash loop alerts in Slack in < 2 min — **usable by the team** |
| 4 Network & access | 16–19 | TrafficRules + Hubble, server firewall (host rules), per-project access, SSO, API tokens, scoped kubeconfig | Automated RBAC suite proves project isolation |
| 5 Nodes & clusters | 20–24 | Hetzner Cloud API token, Cloud API nodes, join script, hcloud CSI/LB, Cloud Firewall sync, vSwitch coupling, build node pool, HA, agent; e2e install runs on fresh Cloud servers (per release tag and nightly, upgrade from the previous release, Let's Encrypt staging, sweeper) | Mixed cluster survives losing a node; second cluster managed; every release tag passes a fresh install and an upgrade |
| 6 Backups, upgrades, beta | 25–28 | Velero to Object Storage, upgrades with rollback, Compose import, templates, docs, license | Full restore onto a new server — **public beta** |
| 7 Kwerft for Mac | 29–36 | Kernel build in CI, installer `--platform mac`, `local` profile, SwiftUI app around the console, push/pull Projects between instances (spike done 2026-10-04) | A Project runs on a Mac without a terminal and goes live on a Hetzner server with one push |

### Phase 1 checklist

- [x] Setup wizard: one-time token from the installer, owner account; installer renews an expired token and recognises a finished setup
- [x] Sign-in, sessions, audit log; TOTP, passkeys, recovery codes; account page
- [x] Domain reconciler: HTTPS listener and certificate per app hostname, HTTP→HTTPS redirect
- [x] Workload API acting as the user; Kubernetes roles for owner/admin/developer/viewer
- [x] Apps UI: list, detail, settings, deploy wizard, projects, restart, scale, rollback
- [x] Volume, Task and Schedule reconcilers; `kwerft-batch` PriorityClass
- [x] Jobs API and UI (Tasks, Schedules, Volumes; run now from an App or Schedule; cancel keeps the record)
- [x] Logs (live, SSE), shell (recorded), replicas table in App detail; Task logs in Task detail
- [x] Domains: read-only list in the API and the Network → Domains & TLS tab
- [x] Shells for running Tasks (incl. the debug toolbox for images without a shell); Access › Recordings with a built-in player (xterm.js — asciinema-player needs `wasm-unsafe-eval`, which the CSP does not allow)
- [x] Members: invites by single-use link (7 days, hashed, rate-limited), role changes effective immediately (also for Kubernetes and open shells/streams), removal ends sessions, last owner protected; admins manage everyone but owners. Access page: Members, Roles (matrix in `internal/access`, tested against the chart), Audit log (filters, paging)
- [x] Settings: console hostname move (new listener and certificate first, old name redirects for 24 h, refused while a member would be locked out by host-bound passkeys) and an apps domain with a DNS-01 wildcard via Hetzner — one shared listener, no per-app DNS record; per-host listeners now cap at 59
- [x] Managed DNS records via Hetzner DNS (Cloud API RRsets, labelled): console hostname incl. a move's new and previous name, `*.<appsDomain>`; a console move into a managed zone no longer waits for DNS
- [x] Managed records and the DNS-01 wildcard on the test server with kwerft.dev (2026-10-04): `*.apps.kwerft.dev` record created, wildcard certificate in ~1.5 min, an app moved onto it
- [x] Certificate secrets of hostnames no longer served are removed after 7 days unused (`kwerft.dev/unused-since`), so a returning hostname reuses its certificate instead of spending Let's Encrypt quota
- [x] Release pipeline: tag `v*` → multi-arch image (ko) and chart on GHCR, stamped install/join scripts to the public `kwerft-install` repo, GitHub Release; signing opt-in (`SIGN_RELEASES`); see RELEASING.md
- [x] Cut the first release and do the one-time GitHub setup (packages public, install repo, token): v0.1.0-rc.1/rc.2, then **v0.1.0** (2026-10-04, the first stable: top-level `install.sh` in kwerft-install, image `:latest`)
- [x] Exit criterion on a fresh Cloud server from the published release (rc.2, 2026-10-04): app on HTTPS in 7:06; a schedule restarted an app on success
- [ ] Not yet: shared storage for pending logins before running more than one replica (admin reset of second factors, "require 2FA" and data-key rotation: done in Phase 4)

### Phase 4 checklist

Work split and contracts: `docs/phase4.md`.

- [x] Types: `Project.spec.access`/`members`, `TrafficRule`, `FirewallRule`
- [x] W1 Project access: per-project RoleBindings (Team | Members), project scope in every list and search, members UI, isolation test suite, admin 2FA reset, "require 2FA" (envtest; on a server: with the exit criterion)
- [x] W2 Traffic rules: TrafficRule → CiliumNetworkPolicy, App/Project policies on Cilium (old NetworkPolicies removed on upgrade), Hubble counts and drops from the relay, "create allow rule", project isolation toggle, plain HTTP only from the Gateway's namespace
- [x] W3 Server firewall: FirewallRules through a node agent (`kwerft node-agent` DaemonSet filling two chains of the installer's nftables table) with lock-out protection and auto-rollback, required rules, Network › Server firewall (try on the test server: `docs/phase4.md` › As built). Hetzner Cloud Firewall sync and the Cloud API token setting moved to Phase 5 (2026-10-04)
- [x] W4 Identity: OIDC SSO (Google, Entra, Keycloak, generic; fake-issuer tests), API tokens (`kwft_`, hashed, role cap, project scope, expiry), scoped kubeconfig through the `/k8s/` proxy, X-Real-IP only from node addresses, data-key rotation (Settings or by hand). Notes in `docs/phase4.md` › As built
- [x] Exit criterion: the isolation suite (`internal/server/isolation_test.go`, in `make check`) proves a developer in one project cannot read another project's pods, logs, secrets, metrics, alerts, builds or traffic — through Kubernetes and through every console list, search and stream; also for project-limited API tokens
- [x] On the test server (2026-10-04): upgrade keeps Team projects working (per-project RoleBindings after the controller's resync), App and Project policies moved to Cilium, Hubble relay closed to app pods; firewall agent in sync, a rule that adds access applies at once, a removal rolls back after 60 s without confirmation, a confirmed one sticks
- [ ] Still to try in the browser: the firewall tab's "your address" and SSH narrowing (W3's plan in `docs/phase4.md`), single sign-on with a real provider, a downloaded kubeconfig
- Moved to Phase 5 (2026-10-04): e2e install runs on Hetzner Cloud, Hetzner Cloud Firewall sync and the Cloud API token setting

### Phase 3 checklist

Work split and contracts: `docs/phase3.md`.

- [x] Types: `AlertRule`, `NotificationChannel`; `internal/observability`; Monitoring page skeleton
- [x] W1 Metrics: Kwerft's own metrics, recording rules, Traefik metrics, metrics API confined to projects, overview and per-app charts (recording rules and confinement checked against VictoriaMetrics v1.153 locally; on a server: with the exit criterion)
- [x] W2 Logs: log search over VictoriaLogs confined to projects, history in the App Logs tab, Task/Build logs after their pod is gone
- [x] W3 Alerting: AlertRule → VMRule, channels (Slack, email, webhook, ntfy) → Alertmanager, default rules, alerts and silences API (one VMAlertmanagerConfig per channel; install.sh turns off the operator's namespace matcher; ntfy through its own templating, no adapter; test sends go straight to the destination)
- [x] W4 Alerting UI: alerts, rules, channels; "needs attention" on Overview
- [x] Exit criterion on the test server (2026-10-04, ntfy.sh instead of Slack — same vmalert → Alertmanager path, different receiver): a crash-looping app deployed at 17:43:20 alerted on the phone at 17:44:17 (57 s), linking to its Logs tab. Needed: crash loops also fire on two restarts within 3 minutes, since Kubernetes reports CrashLoopBackOff only once the back-off is long (first try: ~5 min). Slack itself is still to be tried.

### Phase 2 checklist

Work split and contracts: `docs/phase2.md`.

- [x] Types: `GitConnection`, `Build` source snapshot and numbering, revisions record build and commit; `internal/builds`
- [x] W1 Registry & infrastructure: zot, k3s registry mirror, `kwerft-builds` namespace and policies, installer/join, pins (on a server: with the exit criterion)
- [x] W2 Build engine: Build reconciler (BuildKit rootless, Dockerfile, Railpack, cache in zot, queue, cancel, timeout, retention); Git apps deploy their latest build
- [x] W3 Git connections (GitHub token/App, GitLab, Gitea, generic, deploy keys), webhooks, "Build now", commit checks
- [x] W4 Console: deploy wizard and settings for Git apps, Builds tab with live logs, Git connections page
- [x] First real build on the test server (2026-10-04): public GitHub repository → clone, rootless BuildKit (AppArmor profile `kwerft-buildkit`, since Ubuntu restricts user namespaces), push to zot, rollout pinned by digest through the k3s mirror, live on the apps wildcard in ~70 s; a rebuild reused the cache
- [x] Exit criterion on the test server with a real repository (2026-10-04, private ehilzinger/kwerft-demo through a fine-grained token connection): push → webhook in 3 s → build #2 in 39 s → revision 2 serving ~47 s after the push; build log in the Builds tab; GitHub commit status pending → success linking to the build
- [x] Railpack builder on the server (Go detected, 70 s first build) and pull-request builds (same-repo PR → one check build per App using the repository, not deployed, queued one at a time, checks on the PR)

### Phase 0 checklist

- [x] Repository layout, Makefile, CI workflow, Dockerfile
- [x] Installer skeleton with all stages, dry-run, join mode, uninstall, firewall rescue
- [x] Helm chart: Deployment, RBAC, Gateway, console HTTPRoute, ClusterIssuer
- [x] `Project`, `App`, `Build` types with generated CRDs
- [x] Go server: health, version, SPA serving, security headers
- [x] React console shell with design tokens from the blueprint
- [x] Dev deploy without Docker or a registry: `make dev-server HOST=root@<ip>` (ko image + `--image-archive`)
- [x] Project and App reconcilers with envtest integration tests (started early from Phase 1)
- [x] First install on a Hetzner Cloud server (2026-10-04): all stages pass, Let's Encrypt certificate issued, a demo App reachable publicly, network isolation verified
- [ ] Same on a dedicated server
- [x] Measure the memory budget (see table above)
- [ ] Decide `--lite` defaults
- [ ] ~~e2e harness~~ moved to Phase 4
- [ ] Publish the image and chart to GHCR (`ghcr.io/ehilzinger`) from CI

## Open follow-ups (collected 2026-10-04, after v0.4.0)

Things found while building Phases 1–4 that are not done yet. Pick them up
before the beta or move them into a phase.

**To try on the test server (need the user's browser or accounts)**
- Server firewall: "Your address" shows the real public IP, then SSH narrowing to it (W3's step-by-step plan and recovery in `docs/phase4.md` › As built (W3)); also checks that `X-Real-Ip` is trusted from Traefik's actual peer address (cilium_host) — compare audit-log IPs with the real client.
- Single sign-on with a real provider (redirect URI `https://<console>/api/v1/sso/callback`); an API token and a downloaded kubeconfig (`kubectl auth whoami`, exec/secrets/`--as` refused).
- Slack as a notification channel (only ntfy was tried); the Phase 3 exit criterion literally names Slack.
- Git connections by GitHub App and by SSH deploy key (only a fine-grained token was tried on the server).
- A resolved notification after an alert stops firing; silences from the Alerts tab.

**Small fixes**
- ntfy notifications: set the logs link as the tap target (ntfy `click`), not only in the text.
- Reconcilers log "object has been modified" conflicts as errors since status writes are guarded (`patchStatus`); retry them quietly instead.
- After an upgrade, developers and viewers have no project access for the seconds until the Project reconciler's resync writes the per-project RoleBindings; keep the old binding until then or have the installer wait for them.
- A few checks still use the console role instead of the project role (`POST /git/check`, the alert-rule write route, Jobs/Volumes/Schedule pages), so a console viewer who is a developer in a Members project cannot use them.
- `keep-` tags of a deleted App stay in the registry; webhooks Kwerft created on a Git host stay when the connection is deleted; GitHub App `slug` is never filled in.
- API tokens page: provider-friendly identity names, whether a token's projects still exist.
- The blueprint artifact still shows "Mirror to Hetzner Cloud Firewall" and a Phase 4 Hetzner card (moved to Phase 5).
- `TrafficRuleSpec`'s doc comment says Apps open their ports to their project; they do not (only to `allowFrom` and, when not isolated, every project).

**Design gaps to decide**
- The in-cluster registry has no authentication: a build in project A could push tags into project B's repository (running Apps are protected by digest pinning). Per-project registry credentials.
- Project names stay listable through Kubernetes for every role (cluster-scoped read); alert-rule and channel names are visible to all.
- Builds cannot reach private Git hosts on private networks (by design of the builds egress policy); an opt-in per connection.
- `VolumeFillingUp` never fires on local-path volumes (no kubelet volume stats).
- Hubble counts restart with the console and are per replica; the Traefik router-name format the HTTP metrics depend on is Traefik 3.7's.
- zot-minimal has no metrics endpoint.
- "Patch-only" Secrets are readable from a patch response at the API level; only the kubeconfig proxy closes that gap.

## Kwerft for Mac (Phase 7)

A native macOS app for running dev workloads locally and mirroring them to a
Kwerft instance. Feasibility checked and spiked 2026-10-04 against Apple
`container` 1.5.0 (1.0 shipped at WWDC26; macOS 26+, Apple silicon only, each
container in its own lightweight VM). Spike report: `docs/spike-mac.md`;
scripts: `hack/spike-mac/`.

- **Same Kwerft, not a second implementation.** The app runs the real
  Kwerft binary and CRDs in a local Kubernetes VM. Mapping App/Task/Schedule
  straight onto `container run` and launchd would be lighter, but it
  re-implements every semantic (policies, routes, restart-on-success) and
  drifts from production, which defeats mirroring.
- **Local cluster: k3s in a `container machine`** (decided by the spike).
  Both candidates ran Kwerft end to end; only the machine survives a restart
  (new IP, API back in 34 s, pods in ~2 min, volume intact), while a
  `container k8s` cluster cannot regenerate its certificates for the node's
  new IP and has to be re-created. The machine runs the production stack
  (Cilium with WireGuard and Hubble, Traefik, cert-manager, observability)
  through the unmodified installer in ~9 minutes and 2.5 GB.
- **Kwerft ships its own kernel.** `container`'s default (Kata) lacks
  `xt_socket`, so Cilium's iptables rules fail and Kwerft's default-deny
  policies drop kubelet probes and Traefik; Apple's containerization config
  lacks legacy iptables, the eBPF JIT and BTF. CI builds Apple's config plus
  `hack/spike-mac/kernel/kwerft.config` (under three minutes) and the app
  selects it per machine with `container machine create --kernel`.
- **Installer `--platform mac`:** accept built-in kernel modules, skip
  `swapoff` without swap support, install a boot unit for shared mounts,
  bpffs and a stable node address on a dummy interface (eth0's address
  changes on every start), pin k3s to that address, default to `--lite`, and
  let the Mac reach the API (or use Kwerft's API proxy).
- **`local` profile** in the chart and controllers: Domains get certificates
  from a local CA (the app adds it to the login keychain) and `*.kwerft.test`
  hostnames resolved by `container system dns`; the chart can name any
  ClusterIssuer; Hetzner-only resources (`FirewallRule`, `NodePool`) are
  disabled. With no issuer at all, the Domain reconciler must not create
  HTTPS listeners whose certificates nobody issues (today every route then
  returns 404).
- **The app** is SwiftUI around the existing console: menu bar, local
  cluster lifecycle and resource sizing, the React console in a WKWebView,
  remote instances with their API tokens in the Keychain. It drives Apple's
  `container` through its CLI and JSON output (the XPC API is not a public
  contract) and requires `container` to be installed. It checks container
  egress first: a connected VPN silently cuts containers off from the
  internet. `container machine run` re-parses its arguments through a shell,
  so the app passes scripts as files. Locally built images reach the cluster
  like the installer's own: `container image save`, then `k3s ctr images
  import` inside the machine.
- **Push and pull.** "Push to Kwerft…" exports a Project's Kwerft resources
  (not the rendered objects), strips status and UIDs, applies the target's
  `Environment` overrides, shows a diff, and server-side applies through the
  remote Kwerft API with the user's token, so impersonation still holds.
  Secrets travel by name and keys only; the app asks for values. Pull is the
  same flow in reverse.
- **Images.** Git-sourced Apps build on the server (Phase 2). Images built
  on the Mac with `container build` are arm64; pushing them needs a
  multi-platform build (Rosetta) and a registry the server can pull from —
  zot exposed through Kwerft with token auth, or GHCR.
- **Depends on** Phase 2 (builds, zot) and Phase 4 (API tokens); can start
  in parallel once those land.

## Risks

- **Single node is a single point of failure** — say so in the UI; etcd snapshots to Object Storage from day one.
- **Overhead on small servers** — measured at ≈ 2.4 GB; hold it in CI; `--lite` profile for 4 GB servers.
- **Builds and Tasks compete with apps for memory** — Jobs with limits, one build at a time on small nodes, Tasks in the lower `kwerft-batch` priority class, optional build node pool.
- **Registry retention vs. long-running revisions** — zot deletes a tag that is neither among the newest 20 nor pulled for 90 days, even if a pod still runs it (it was pulled when the pod started); a rescheduled pod then cannot pull. Values are in the chart (`registry.retention`); before the beta, tag images that a revision uses (e.g. a kept `rev-*` pattern) or have the App reconciler re-pull them.
- **Cloud vs. dedicated asymmetry** — Robot cannot create servers on demand; vSwitch ↔ Cloud Network coupling is per network zone; otherwise WireGuard over public IPs (requires the console to open node ports per joiner — Phase 5).
- **Firewall lock-out** — caller-IP check, auto-revert timer, `install.sh --reset-firewall`.
- **Apple `container` is young** — `container k8s` is experimental and cannot survive a restart, `container machine run` loses argument quoting, and the XPC API is not a public contract; pin the supported `container` version in the app, drive it only through the CLI's JSON output, and keep desired state outside the local cluster.
- **Owning a kernel** — Kwerft for Mac needs its own kernel build (Apple's config plus a fragment). Building is cheap; tracking Apple's config and kernel security releases is a standing cost. Upstream the missing options to apple/containerization to shrink the fragment.
- **Upstream churn** — pinned release manifest; upgrade tests from N-1 and N-2.
