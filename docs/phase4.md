# Phase 4 — Network & access: work split and contracts

Exit criteria:
1. A developer in one project cannot read another project's pods, logs,
   secrets, metrics, alerts, builds or traffic — proven by an automated test
   suite against a real API server.
2. Every release tag passes a fresh install and an upgrade from the previous
   release on a new Hetzner Cloud server.

Five workers build this in parallel, each in its own branch. This file is
the contract between them. Shared, already on main: `api/v1alpha1`
(`ProjectSpec.Access` and `Members`, `TrafficRule`, `FirewallRule`).

## Ownership

| Worker | Owns | Touches lightly |
|---|---|---|
| W1 project access | `project_controller.go` RoleBindings, `charts/kwerft/templates/roles.yaml`, `internal/access`, a `projectScope` helper in `internal/server` that every list/search uses, project members API + UI, the isolation test suite (`internal/server/isolation_test.go`), admin reset of a member's second factors, a "require 2FA" setting | `api_workloads.go`, `api_metrics.go`, `api_logsearch.go`, `api_alerts.go`, `api_builds.go` (only to call `projectScope`), Access/Projects pages |
| W2 traffic rules | `internal/controllers/traffic_*.go` (TrafficRule → CiliumNetworkPolicy; App and Project policies move to CiliumNetworkPolicy — `TODO(phase-4)` in `app_render.go`), Hubble flows client (`internal/hubble/`), `internal/server/api_traffic.go`; UI: Network › Traffic rules tab, "create allow rule" from drops | `app_render.go`, `project_controller.go` (policy only), `domain_controller.go` (plain-HTTP TODO), `install.sh` (Hubble relay/metrics if needed) |
| W3 server firewall | `internal/controllers/firewall_*.go`, the node agent that applies host rules (`cmd/kwerft` subcommand or separate DaemonSet in the chart), Hetzner Cloud Firewall sync (`internal/hetzner` cloud firewall client), the Hetzner Cloud API token setting (write-only Secret `kwerft-hcloud-token`, like the DNS token), `internal/server/api_firewall.go`; UI: Network › Server firewall tab, Settings › Hetzner Cloud API card | `install.sh` (base rules become FirewallRules; the agent's nftables table), `roles.yaml` |
| W4 identity | OIDC SSO (`internal/auth/oidc*`), API tokens (SQLite, hashed, scoped, expiring; `Authorization: Bearer` on `/api`), scoped kubeconfig download (a Kubernetes API proxy in the console that authenticates API tokens and impersonates the user), trusting `X-Real-IP` only from Traefik (`TODO(phase-4)` in `api.go`), data-key rotation; UI: Account › API tokens, Settings › Single sign-on | `api.go`, `server.go`, `internal/store` (new tables + migrations), Login page |
| W5 e2e install runs | `.github/workflows/e2e.yml`, `hack/e2e/` (harness: create a Cloud server through the API, install, assert, upgrade, destroy; sweeper), `RELEASING.md` | `release.yml` (call e2e after publish), `install.sh` only if a flag is missing (e.g. Let's Encrypt staging) |

## Project access (W1)

- Global roles stay: owner, admin, developer, viewer. Owners and admins
  reach every project (cluster-wide bindings as today).
- `Project.spec.access` Team (default, today's behaviour: every console
  user with their global role) or Members (only `spec.members`, each with a
  per-project role developer|viewer; the global role does not apply there).
- Kubernetes enforces it: developers and viewers lose the cluster-wide
  binding for namespaced kwerft.dev resources and pods; the Project
  reconciler binds, per project namespace, the groups `kwerft:role:developer`
  / `viewer` (Team) or the users `kwerft:<email>` (Members) to the namespaced
  ClusterRoles. Cluster-scoped reads (projects list, Git connections, alert
  rules, settings) stay cluster-wide; the console filters the project list.
- `projectScope(ctx, principal)` returns the project namespaces the user
  reaches (and whether platform namespaces are included); metrics, logs,
  alerts, builds, apps and jobs lists all use it.
- Members are managed by owners and admins (impersonated update of the
  Project, audited). Changing a project from Team to Members asks who keeps
  access.

## Traffic (W2)

- Cilium is the CNI (kube-proxy replacement); policies move to
  `CiliumNetworkPolicy` so ingress from Traefik (host network, entity
  `host`/`remote-node`) is matched precisely.
- A TrafficRule in project P renders CiliumNetworkPolicies in P (ingress to
  P's apps) and, for egress rules, on P's source apps. Apps from another
  project or that project: `k8s:io.kubernetes.pod.namespace` + app label.
- Hubble: hit and drop counts per rule (by policy name in flow verdicts) and
  a "dropped in the last hour" list per project, through the Hubble relay
  (the installer enables Hubble; check relay availability and its auth) or
  Hubble metrics in VictoriaMetrics — decide and document. Confined to the
  user's projects like metrics.

## Server firewall (W3)

- The installer's nftables base rules stay the safety net; Kwerft manages
  only its own table/chain on each node through a node agent and never
  removes SSH (22) or HTTP(S) access wholesale.
- Lock-out protection: a change that would block SSH or HTTPS from the
  client's current IP is refused; an applied change rolls back automatically
  unless the console confirms within 60 s (the browser that made the change
  confirms by calling back).
- ~~Hetzner Cloud Firewall sync for Cloud nodes (label/selector-based
  firewall named after the cluster) when a Cloud API token is set; dedicated
  nodes get host rules only.~~ **Moved to Phase 5 (2026-10-04)**, together
  with the Hetzner Cloud API token setting (write-only Secret
  `kwerft-hcloud-token`, Settings › Hetzner Cloud API) and the Cloud
  Firewall client and fake. `FirewallRuleStatus.cloudFirewall` stays unused
  until then. Phase 4 ships host rules on every node, Cloud and dedicated.
- **Never apply firewall changes to a real server during development.** The
  coordinator tries it on the test server with a console session open and a
  way back (Hetzner Cloud console) ready.

### As built (W3)

**On the host.** `install.sh` (stage Firewall, also in join mode) writes
`/etc/nftables.d/kwerft.nft`: table `inet kwerft` with two empty chains
Kwerft owns, `managed_ssh` and `managed_open`, and the base chain `input`
(policy drop) in this order: established, loopback, ICMP, Cilium interfaces,
pods (10.42.0.0/16), the private network, WireGuard (udp/51871), **then**
`tcp dport 22 jump managed_ssh`, the accept for 22/80/443, and `jump
managed_open` last. The file declares the managed chains (which does not
flush them) and then flushes and refills only `input`, all in one `nft -f`
transaction: re-running the installer neither opens a window without a
firewall nor drops the console's rules (the old `nft delete table` is gone).
At boot `nftables.service` loads the baseline with empty managed chains; the
agent refills them. Consequences: Kwerft can narrow *public* SSH and open
ports; it cannot touch HTTP(S), the cluster's traffic, or SSH from the
private network, and an empty or missing managed chain is exactly the
baseline. `install.sh --reset-firewall` deletes the table **and** writes
`/var/lib/kwerft/firewall/paused`, which stops the agent from putting the
console's rules back on that node (delete the file to resume); uninstall
removes everything under `/var/lib/kwerft`.

**Node agent** (`kwerft node-agent`, DaemonSet `kwerft-node-agent` in the
chart, `firewall.agent.enabled`): host network, root with only `NET_ADMIN`
and `SYS_CHROOT`, read-only root filesystem. The image is distroless, so it
runs the host's own `nft` chrooted into an emptyDir that has the host's
`/usr` and `/etc/ld.so.cache` mounted read-only (the agent adds the
merged-/usr `lib`/`bin` links). State in hostPath `/var/lib/kwerft/firewall`.
Kubernetes access: `get` on ConfigMap `kwerft-firewall`, `get`/`patch` on
`kwerft-firewall-status` (resourceNames, nothing else). Why a ConfigMap and
not a FirewallRule watch: the agent then needs no access to kwerft.dev at
all, and the controller does validation, node scoping and the revision once;
the agent re-validates everything anyway (only checked CIDRs, ports and rule
names ever reach nft) and renders the script itself, so a hand-edited
ConfigMap cannot inject nft statements.

**State machine** (`internal/firewall/agent.go`, ticks every second, reads
the desired rules every 2 s): a node's rules that take nothing away from
its confirmed rules (`firewall.Permits`: opening ports, adding SSH sources,
lifting the narrowing) are applied at once; anything else is applied as
*pending* and rolled back to the confirmed rules 60 s after it was applied
unless the desired document says it is confirmed. The timer runs in the
agent from its own state file, so the rollback happens with the console,
the controller or the API server down, and after an agent restart. A
rolled-back revision is never re-applied (not even by a late confirmation);
"Apply again" makes a new revision. nft runs `--check` first; a refused
change is dropped and the confirmed rules stay. Status per node in
`kwerft-firewall-status` (`in-sync`, `pending` with deadline, `rolled-back`,
`error`, `paused`, `unprepared` = no table / older installer).

**Controller** (`internal/controllers/firewall_controller.go`): creates the
required rules (label `kwerft.dev/required`: `ssh`, `http`, `https`,
`wireguard`, `icmp` — port 1 because the type requires one —, and
`cluster-private` with the pod network and `--private-network`, which
install.sh passes as `firewall.privateNetwork`), puts their fixed specs back
if edited (only the SSH rule's sources may change) and recreates them when
deleted. It validates the other rules (no ICMP, no range over 22/80/443 or
udp/51871, no CIDRs with host bits, no etcd/API/kubelet port open to
everyone), renders `desired.json` (revision = hash of the rule set, not of
the node list, so joining nodes do not trigger confirmations) and marks a
revision confirmed when an owner/admin confirmed it (annotation
`kwerft.dev/firewall-confirmed` on the rule `ssh`, refused once a node rolled
it back) or every reporting agent took it without a pending change. The
confirmed rule set is snapshotted (`confirmed-rules.json`) for "Roll back
now". It watches only the status ConfigMap (an informer of its own), not all
ConfigMaps.

**API** (`internal/server/api_firewall.go`, owners and admins, impersonated,
audited): `GET /api/v1/firewall`, `POST|PUT|DELETE /api/v1/firewall/rules`,
`POST /api/v1/firewall/confirm {revision}`, `/rollback` (restore the
snapshot as the user), `/retry {revision}` (bump
`kwerft.dev/firewall-attempt`). Lock-out check: narrowing SSH (and retrying
a narrowing) is refused unless the client's address is inside the sources or
the private network, and refused outright when the address is loopback,
link-local or in the pod network (a proxy hid the client). The address comes
from `clientIP` (X-Real-IP); **W4** restricts trusting that header to
Traefik, and the check follows (one helper, `firewallAPI.clientAddr`).

**UI**: Network › Server firewall — rules table (required rules marked; SSH
shows "your IP is covered"), add/edit dialog, an SSH sources dialog with
"Add my address", the pending banner with a countdown, "Keep these rules"
and "Roll back now", a rolled-back banner with "Apply again"/"Discard the
change", and a nodes table with each agent's state.

**Deferred:** Cloud Firewall sync (Phase 5, above); narrowing HTTP(S) (never:
it is the way back); custom ICMP rules; IPv6-aware warnings when the browser
uses IPv6 but SSH goes over IPv4 (the check covers the address the console
sees); NodePorts are answered by Cilium in eBPF before nftables, so host
rules neither open nor close them.

**Trying it on the test server** (coordinator; the full plan is in the W3
report): with a root SSH session open and the Hetzner Cloud console (VNC)
ready, re-run the installer with the branch's image, check `nft list table
inet kwerft` shows the jumps and the agent pods report `in-sync`; open a
port (applies at once), then narrow SSH to your address (pending; keep it
after a new SSH session works), then try a range without your address — the
console refuses; finally let a pending change expire and watch it roll
back. Rescue: `install.sh
--reset-firewall` from the VNC console, or `nft flush chain inet kwerft
managed_ssh`.

## Identity (W4)

- OIDC: generic issuer + presets (Google, GitHub via OIDC-compatible
  endpoints where available, Microsoft Entra, Keycloak); users matched by
  verified email; optional auto-join with a default role; client secret in
  a write-only Secret.
- API tokens: `kwft_…`, shown once, stored as hashes, with a role cap
  (never above the user's), optional project restriction, expiry (default
  90 days, max 1 year), last used; revocable; audited. Bearer auth skips
  the same-origin check (no cookies) but not rate limits.
- Kubeconfig: a proxy at `/k8s/` in front of the Kubernetes API that
  accepts API tokens and impersonates the token's user (same groups as the
  console). The downloaded kubeconfig names the console URL and a token.

## e2e (W5)

- Secret `HCLOUD_TOKEN` (a separate Hetzner test project, set by the user)
  — without it the workflow skips with a notice.
- Per release tag (after publish) and nightly: fresh install of the tag
  and an upgrade from the previous release, on `cx22`-class servers labelled
  `kwerft-e2e=true`, sslip.io hostname, Let's Encrypt **staging**; assert:
  installer exit 0, console healthy, setup via `--config` owner, deploy an
  image App and a Git build of a public repo, HTTPS answers, a Task runs,
  metrics and logs arrive, an alert fires; then destroy. A sweeper deletes
  e2e servers older than 3 h.

## Verification

Each worker: `make check` green (and `GOTOOLCHAIN=go1.26.0 go vet ./...`),
envtest tests, fakes for external APIs (Hubble, Hetzner, OIDC provider).
Nobody touches the test server or any real infrastructure; the coordinator
merges and verifies.
