# Werft implementation plan

Werft turns a rented Hetzner server (Cloud or dedicated) into a Kubernetes
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
| Name & hosting | **Werft, under the personal GitHub account `ehilzinger`** | Module `github.com/ehilzinger/werft`, image `ghcr.io/ehilzinger/werft`, chart `oci://ghcr.io/ehilzinger/charts/werft`. Can move to an organisation later. The installer is served from GitHub raw until Werft has its own domain (then `get.werft.sh`). |

## Principles

1. **One command, zero follow-up.** The installer is idempotent and fully flag-driven; re-running repairs or upgrades. Cloud-init can call it unattended.
2. **Kubernetes is the database.** Apps, rules and domains are custom resources; the UI writes them and reconcilers render native objects. GitOps works for free.
3. **Lean on a 4 GB box.** Platform overhead around 2 GB of RAM; builds need 8 GB to run comfortably.
4. **Secure by default.** Projects isolated, Kubernetes API private, secrets encrypted, every shell session and change audited.
5. **Never a dead end.** YAML export, scoped kubeconfig, `kubectl` all keep working.

## Architecture

```
Clients        browser · werftctl · CI tokens · kubectl (scoped kubeconfig)
Edge :80/443   Traefik (hostNetwork DaemonSet, Gateway API) + cert-manager
Werft          one Go binary: REST/WebSocket API, reconcilers, embedded React UI, SQLite
Platform       VictoriaMetrics · VictoriaLogs + Vector · Hubble · Velero · BuildKit · zot
Kubernetes     k3s (embedded etcd, secrets encryption) · Cilium (kube-proxy replacement, WireGuard)
Host           Ubuntu 22.04/24.04/26.04 · nftables baseline · chrony · unattended-upgrades
Hetzner        Cloud API (servers, networks, firewalls, volumes, LBs) · Robot · Object Storage · DNS
```

A change flows: UI → API checks the role → API writes the custom resource
**impersonating the user** (Kubernetes RBAC is the final gate) → reconciler
renders Deployment/Service/HTTPRoute/CiliumNetworkPolicy/PVC with server-side
apply → status streams back over WebSocket from a shared informer cache.

Remote clusters run `werft-agent`, which dials out to the console over an
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

Memory budget on one node (validate in Phase 0): k3s 600 MB, Cilium 300 MB,
Traefik + cert-manager 180 MB, VictoriaMetrics stack 350 MB, VictoriaLogs +
Vector 200 MB, Werft 150 MB, zot 60 MB → **≈ 1.9 GB**, plus 1–2 GB per
running build.

## Installer (`install/install.sh`)

Stages, each idempotent and recorded in `/var/lib/werft/stages/`:

1. **Preflight** — platform detection, root, Ubuntu version, arch, RAM, disk, cgroup v2, ports 80/443, outbound HTTPS.
2. **System** — packages, kernel modules, sysctls, swap off, chrony, unattended-upgrades, optional SSH hardening.
3. **Firewall** — own `inet werft` nftables table: 22/80/443 public, cluster traffic only from the private network and pods.
4. **Kubernetes** — k3s server from `/etc/rancher/k3s/config.yaml` (no flannel, no kube-proxy, no bundled Traefik/servicelb).
5. **Helm**, **Network** (Cilium), **Ingress & TLS** (Gateway API CRDs, cert-manager, Traefik), **Observability**.
6. **Werft** — Helm chart from the checkout or the OCI registry; `--config` becomes the `werft-bootstrap` Secret.
7. **Handoff** — DNS check, single-use setup token (hash only in the cluster), summary.

Without `--domain` the console gets a temporary `<public-ip>.sslip.io` hostname
(public wildcard DNS) so a trial works with zero DNS setup; the installer warns
about it, and re-running with `--domain` switches over. The chosen hostname is
saved in `/var/lib/werft/domain`, so re-running without flags never changes it.
`--email` is optional.

Exit codes: 0 ok · 2 usage · 10 preflight · 20 network/DNS · 30 Kubernetes · 40 platform · 50 Werft.

## Resource model (`api/v1alpha1`)

| Resource | Becomes | Status |
|---|---|---|
| `Project` (cluster-scoped) | Namespace, quota, Pod Security level, RoleBindings, default-deny policy | types ✔ |
| `App` | Deployment/StatefulSet, Service, HTTPRoute, PVCs, HPA, CiliumNetworkPolicy; source = image **or** Git | types ✔ |
| `Build` | Job running rootless BuildKit; pushes to zot; success creates an App revision | types ✔ |
| `GitConnection` | GitHub App / GitLab / Gitea / deploy key credentials and webhooks | Phase 2 |
| `Domain` | Gateway listener, Certificate, DNS record | Phase 1 |
| `TrafficRule` | CiliumNetworkPolicy, with Hubble hit/drop counts | Phase 4 |
| `FirewallRule` | Cilium host policy + Hetzner Cloud Firewall | Phase 4 |
| `NodePool`, `Cluster` | Hetzner Cloud servers + cloud-init join; agent for remote clusters | Phase 5 |
| `BackupPlan`, `AlertRule` | Velero Schedule; VMRule + Alertmanager route | Phases 3, 6 |

## Security model

- Console unusable until the setup token from the server's disk is presented; no default passwords.
- Werft roles → ClusterRoles bound per project namespace; the API impersonates the user.
- Kubernetes API on the private network only; external `kubectl` through Werft's proxy with short-lived scoped kubeconfigs.
- k3s secrets encryption; developers write but cannot read secrets unless granted.
- Exec sessions role-gated, time-limited and recorded.
- Default-deny between projects, WireGuard between nodes, host firewall with lock-out protection.
- API tokens scoped, expiring, stored hashed. Signed images, pinned digests, SBOMs.

## Roadmap (~28 weeks, 1–2 engineers)

| Phase | Weeks | Scope | Exit criterion |
|---|---|---|---|
| 0 Foundations | 1–2 | Repo, CI, chart, CRDs, installer stages 1–4 on Cloud **and** dedicated, memory budget | Nightly CI installs on a fresh Cloud server; weekly on a dedicated test server |
| 1 Installer & deploy MVP | 3–8 | Full installer, setup wizard, auth, Project/App/Domain reconcilers, apps UI, logs, shell, rollback | Fresh server → app on HTTPS in < 10 min |
| 2 Builds from Git | 9–12 | GitConnection, webhooks, Build reconciler, BuildKit, Railpack, zot, auto-deploy, commit checks | Push to main live in < 3 min with build log and commit check |
| 3 Monitoring & logs | 13–15 | VictoriaMetrics/Logs, charts, log search, alerts, notification channels | Crash loop alerts in Slack in < 2 min — **usable by the team** |
| 4 Network & access | 16–19 | TrafficRules + Hubble, server firewall + Cloud Firewall sync, members, SSO, tokens, audit | Automated RBAC suite proves project isolation |
| 5 Nodes & clusters | 20–24 | Cloud API nodes, join script, hcloud CSI/LB, vSwitch coupling, build node pool, HA, agent | Mixed cluster survives losing a node; second cluster managed |
| 6 Backups, upgrades, beta | 25–28 | Velero to Object Storage, upgrades with rollback, Compose import, templates, docs, license | Full restore onto a new server — **public beta** |

### Phase 0 checklist

- [x] Repository layout, Makefile, CI workflow, Dockerfile
- [x] Installer skeleton with all stages, dry-run, join mode, uninstall, firewall rescue
- [x] Helm chart: Deployment, RBAC, Gateway, console HTTPRoute, ClusterIssuer
- [x] `Project`, `App`, `Build` types with generated CRDs
- [x] Go server: health, version, SPA serving, security headers
- [x] React console shell with design tokens from the blueprint
- [ ] Run the installer on a Hetzner Cloud server and a dedicated server; fix chart values against the pinned versions
- [ ] Measure the memory budget; decide on `--lite` defaults
- [ ] e2e harness: create Cloud server via API, install, assert, destroy (+ nightly sweeper)
- [ ] Publish the image and chart to GHCR (`ghcr.io/ehilzinger`) from CI

## Risks

- **Single node is a single point of failure** — say so in the UI; etcd snapshots to Object Storage from day one.
- **Overhead on small servers** — hold the budget in CI; `--lite` profile.
- **Builds compete with apps for memory** — Jobs with limits, one at a time on small nodes, optional build node pool.
- **Cloud vs. dedicated asymmetry** — Robot cannot create servers on demand; vSwitch ↔ Cloud Network coupling is per network zone; otherwise WireGuard over public IPs (requires the console to open node ports per joiner — Phase 5).
- **Firewall lock-out** — caller-IP check, auto-revert timer, `install.sh --reset-firewall`.
- **Upstream churn** — pinned release manifest; upgrade tests from N-1 and N-2.
