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

### As built (W2)

- **Policies** (`internal/controllers/traffic_render.go`), all
  CiliumNetworkPolicies, all additive:
  - Project `default-deny.project` when isolated: every pod denies ingress
    unless another policy allows it (as the old NetworkPolicy did).
  - App `<app>`: on the App's ports, from the nodes (`host`,
    `remote-node`: Traefik on the host network, kubelet probes), from
    namespaces labelled `kwerft.dev/system`, from `allowFrom` apps and their
    Tasks (`kwerft.dev/as-app`); egress none/https/all exactly as before
    (DNS + any pod; https adds TCP 443 to 0.0.0.0/0 minus RFC 1918 and
    169.254/16). Same-project apps still need `allowFrom` or a rule (the
    blueprint's "deny new apps until a rule allows them").
  - **Isolation now means something for apps**: `spec.isolated: false` adds
    "every project namespace's pods" to each App's policy and drops the
    project's default deny. Isolated projects (the default) behave exactly
    as before; the old NetworkPolicy ignored the flag for apps.
  - TrafficRule `<rule>.traffic-in` (destinations in this project; sources
    anything: an app or `project/app` with its Tasks, a whole project,
    the internet = `host`/`remote-node`/`world`, a CIDR) and
    `<rule>.traffic-out` (sources must be this project's apps; to the
    internet = 0.0.0.0/0 minus private ranges, a CIDR, another project's
    app or project). `enableDefaultDeny: {ingress: false, egress: false}`,
    so a rule never cuts an App off from what its own policy allows. A
    Project peer as destination selects its apps (`kwerft.dev/app`), not
    Task pods. One Cilium rule per peer (Cilium refuses mixing L3 selector
    kinds in one rule).
- **Consent across projects**: a rule in A never writes in B. Its egress
  side is applied in A; B's apps accept only if B allows it (an App's
  `allowFrom: [A/x]`, a TrafficRule in B from A's app or project, or B not
  isolated). Until then A's rule is `Ready=False AwaitingPeer` with what B
  has to add; the reconciler re-checks when B's rules, apps or isolation
  change. Other ready states: `Applied`, `Disabled` (policies removed),
  `AppNotFound` (this project's app missing; applied anyway), `InvalidRule`
  (policies removed; `ValidateTrafficRule` names the field — the API uses
  the same function), `CiliumMissing`.
- **Migration**: the App and Project reconcilers apply the new policy, then
  delete the NetworkPolicy of the old name if (and only if) Kwerft's
  object controls it. Every App and Project reconciles on start, so an
  upgrade migrates everything; hand-made NetworkPolicies stay. Task
  policies stay Kubernetes NetworkPolicies (deny ingress + egress), which
  Cilium enforces alongside.
- **Hubble source: the relay's gRPC observer API**, not Hubble metrics.
  Hubble's Prometheus metrics carry workloads and verdicts but neither
  policy names nor ports, so they cannot attribute a connection to a rule
  or say which port a drop wanted. The console (`internal/hubble`) follows
  `Observer/GetFlows` (drops and policy verdicts, from an hour back on
  connect, resuming after the newest flow) over h2c with net/http and
  protowire — no Cilium Go module (it would pin Kubernetes libraries).
  It keeps a one-hour ring of per-minute buckets: allowed connections per
  policy (`ingress_allowed_by`/`egress_allowed_by`), and policy drops
  (reasons 133/181) by source/destination namespace+app (or reserved
  identity + address outside the cluster), port and protocol, at most 2000
  distinct per minute. Counts start when the console starts (`since`).
  Per rule: allowed = verdicts naming its two policies; dropped = policy
  drops from its sources to its destinations on any port (a drop names no
  policy, so this is "traffic this rule is about but did not let through").
- **Confinement**: `GET /projects/{p}/traffic` lists the rules as the user
  first (Kubernetes decides), then shows only flows with a side in `p`;
  `GET /traffic/drops` uses `trafficScope` (owners/admins everything, else
  the projects they can list) — W1's `projectScope` replaces it. Users see
  the other side as namespace/app, never pod names or pod IPs.
- **Relay exposure**: the relay answers anyone about every flow, so the
  chart's `kwerft-hubble-relay` CiliumNetworkPolicy (kube-system) admits
  only the console and the nodes. The installer pins
  `hubble.relay.tls.server.enabled=false` (Cilium's default) and passes
  `hubble.enabled` (false with `--lite`) to the chart, which then passes
  `--hubble-relay=` (off) to the console.
- **API** (`internal/server/api_traffic.go`, impersonated, audited
  `trafficrule.*`, `project.isolation`): `GET /projects/{p}/traffic`
  (isolation, Hubble state, rules with live counts, drops with a suggested
  rule), `POST /projects/{p}/trafficrules`, `PUT|DELETE
  /projects/{p}/trafficrules/{r}`, `PUT /projects/{p}/isolation`
  (owners/admins via RBAC), `GET /traffic/drops`. Suggestions go to the
  project that must allow: the destination's for traffic into an app, the
  source's for traffic out to an address.
- **RBAC**: developers write `trafficrules` (added to the developer rule in
  `roles.yaml`); viewers read. The Access matrix (`internal/access`, W1)
  still needs a row "Edit traffic rules: developer within project".
- **Plain HTTP** (`domain_controller.go`): the HTTP listener admits routes
  only from the Gateway's namespace (`From: Same`): Kwerft's catch-all
  `kwerft-http-redirect` (301 to HTTPS for every hostname), the console's
  routes, and cert-manager's HTTP-01 solver routes, which gateway-shim
  creates there (its Certificates live next to the Gateway). Apps no longer
  render `<port>-redirect` routes; old ones are deleted as undesired.

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
