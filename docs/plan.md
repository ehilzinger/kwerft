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
| Name & hosting | **Kwerft, under the personal GitHub account `ehilzinger`** | Module `github.com/ehilzinger/kwerft`, image `ghcr.io/ehilzinger/kwerft`, chart `oci://ghcr.io/ehilzinger/charts/kwerft`. Can move to an organisation later. The installer is served from GitHub raw until Kwerft has its own domain (then `get.kwerft.dev`). |

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
| zot registry (Phase 2) | 60 MB | — |
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
| `Project` (cluster-scoped) | Namespace, quota, Pod Security level, default-deny policy; RoleBindings in Phase 4 | reconciler ✔ |
| `App` | Deployment, or StatefulSet when it has volumes; Service, HTTPRoute per public port, NetworkPolicy; source = image **or** Git | reconciler ✔ (HPA later) |
| `Build` | Job running rootless BuildKit; pushes to zot; success creates an App revision | types ✔ |
| `GitConnection` | GitHub App / GitLab / Gitea / deploy key credentials and webhooks | Phase 2 |
| `Domain` | Gateway listener, Certificate, DNS record | Phase 1 |
| `TrafficRule` | CiliumNetworkPolicy, with Hubble hit/drop counts | Phase 4 |
| `FirewallRule` | Cilium host policy + Hetzner Cloud Firewall | Phase 4 |
| `NodePool`, `Cluster` | Hetzner Cloud servers + cloud-init join; agent for remote clusters | Phase 5 |
| `BackupPlan`, `AlertRule` | Velero Schedule; VMRule + Alertmanager route | Phases 3, 6 |

## Security model

- Console unusable until the setup token from the server's disk is presented; no default passwords.
- Kwerft roles → ClusterRoles bound per project namespace; the API impersonates the user.
- Kubernetes API on the private network only; external `kubectl` through Kwerft's proxy with short-lived scoped kubeconfigs.
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
- [x] Dev deploy without Docker or a registry: `make dev-server HOST=root@<ip>` (ko image + `--image-archive`)
- [x] Project and App reconcilers with envtest integration tests (started early from Phase 1)
- [x] First install on a Hetzner Cloud server (2026-10-04): all stages pass, Let's Encrypt certificate issued, a demo App reachable publicly, network isolation verified
- [ ] Same on a dedicated server
- [x] Measure the memory budget (see table above)
- [ ] Decide `--lite` defaults
- [ ] e2e harness: create Cloud server via API, install, assert, destroy (+ nightly sweeper)
- [ ] Publish the image and chart to GHCR (`ghcr.io/ehilzinger`) from CI

## Risks

- **Single node is a single point of failure** — say so in the UI; etcd snapshots to Object Storage from day one.
- **Overhead on small servers** — measured at ≈ 2.4 GB; hold it in CI; `--lite` profile for 4 GB servers.
- **Builds compete with apps for memory** — Jobs with limits, one at a time on small nodes, optional build node pool.
- **Cloud vs. dedicated asymmetry** — Robot cannot create servers on demand; vSwitch ↔ Cloud Network coupling is per network zone; otherwise WireGuard over public IPs (requires the console to open node ports per joiner — Phase 5).
- **Firewall lock-out** — caller-IP check, auto-revert timer, `install.sh --reset-firewall`.
- **Upstream churn** — pinned release manifest; upgrade tests from N-1 and N-2.
