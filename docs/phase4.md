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

### As built (W1)

- RBAC: `kwerft:developer`/`kwerft:viewer` (cluster-wide) list only the
  cluster-scoped kinds by name — never `"*"`, which a ClusterRoleBinding
  would apply to every namespace. Namespaced rights moved to
  `kwerft:project-developer` / `kwerft:project-viewer` (apps, domains,
  tasks, schedules, volumes, trafficrules write; builds create/patch; `"*"`
  read), bound per project with `kwerft:pods-read`/`-exec` by
  `controllers.ProjectBindings` — the one function the reconciler, the
  access matrix tests and the isolation suite share. RoleBinding subjects
  are atomic, so a removed member or a switch to Members takes old subjects
  away on the next apply. Owners and admins get pods/exec through their
  groups in every project, Team or Members.
- Upgrade: a Project without `spec.access` is Team; the reconciler adds the
  two new bindings on its startup resync. Between the chart upgrade and that
  resync (seconds) developers and viewers lack namespaced access.
- Members are console accounts, stored with the account's own spelling of
  the address (Kubernetes compares `kwerft:<email>` byte for byte); owners
  and admins cannot be listed (they reach everything). Switching to Team
  clears the list, so a later switch back never revives stale grants.
  Removing a console account takes it off every project (as the remover).
- `projectScope` (`internal/server/scope.go`): projects reached (from the
  spec, as the reconciler binds), their namespaces only when labelled by the
  reconciler for that project, a role per project, a platform flag for
  owners and admins, and `restrict(projects)` for project-limited API
  tokens (W4 wires it at merge). Used by: project list, apps, tasks,
  schedules, volumes, domains (cache-backed, filtered; without a cache:
  per-namespace lists as the user), metrics overview/explorer, log search
  and tail, alerts, silences, alert rules, recordings. Developers write
  alert rules only for projects where their role allows it (a rule without
  scope needs every project) and see only rules about projects they reach;
  silencing follows the role in the alert's project.
- "Require two-factor sign-in" is an owner-only setting in SQLite
  (`settings` table, migration after W4's). It is enforced per request in
  `requireUser` (any session, however it signed in: password, passkey,
  SSO): without a factor only session, account enrolment and sign-out
  routes answer (`403 code=enrolSecondFactor`), and the UI routes to the
  Account page. Turning it on needs a factor of one's own; with it on,
  nobody removes their last factor.
- Behaviour change: a developer writing into a namespace they do not reach
  (also a missing project) now gets 403 instead of 404.

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
- Hetzner Cloud Firewall sync for Cloud nodes (label/selector-based firewall
  named after the cluster) when a Cloud API token is set; dedicated nodes
  get host rules only.
- **Never apply firewall changes to a real server during development.** The
  coordinator tries it on the test server with a console session open and a
  way back (Hetzner Cloud console) ready.

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
