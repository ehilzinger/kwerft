# Phase 4 — Network & access: work split and contracts

Exit criterion: a developer in one project cannot read another project's
pods, logs, secrets, metrics, alerts, builds or traffic — proven by an
automated test suite against a real API server.

Moved to Phase 5 (2026-10-04), with the rest of the Hetzner Cloud API work:
the e2e install runs (formerly W5, exit criterion 2), the Hetzner Cloud
Firewall sync and the Cloud API token setting (formerly part of W3).

Four workers build this in parallel, each in its own branch. This file is
the contract between them. Shared, already on main: `api/v1alpha1`
(`ProjectSpec.Access` and `Members`, `TrafficRule`, `FirewallRule`).

## Ownership

| Worker | Owns | Touches lightly |
|---|---|---|
| W1 project access | `project_controller.go` RoleBindings, `charts/kwerft/templates/roles.yaml`, `internal/access`, a `projectScope` helper in `internal/server` that every list/search uses, project members API + UI, the isolation test suite (`internal/server/isolation_test.go`), admin reset of a member's second factors, a "require 2FA" setting | `api_workloads.go`, `api_metrics.go`, `api_logsearch.go`, `api_alerts.go`, `api_builds.go` (only to call `projectScope`), Access/Projects pages |
| W2 traffic rules | `internal/controllers/traffic_*.go` (TrafficRule → CiliumNetworkPolicy; App and Project policies move to CiliumNetworkPolicy — `TODO(phase-4)` in `app_render.go`), Hubble flows client (`internal/hubble/`), `internal/server/api_traffic.go`; UI: Network › Traffic rules tab, "create allow rule" from drops | `app_render.go`, `project_controller.go` (policy only), `domain_controller.go` (plain-HTTP TODO), `install.sh` (Hubble relay/metrics if needed) |
| W3 server firewall | `internal/controllers/firewall_*.go`, the node agent that applies host rules (`cmd/kwerft` subcommand or separate DaemonSet in the chart), `internal/server/api_firewall.go`; UI: Network › Server firewall tab | `install.sh` (base rules become FirewallRules; the agent's nftables table), `roles.yaml` |
| W4 identity | OIDC SSO (`internal/auth/oidc*`), API tokens (SQLite, hashed, scoped, expiring; `Authorization: Bearer` on `/api`), scoped kubeconfig download (a Kubernetes API proxy in the console that authenticates API tokens and impersonates the user), trusting `X-Real-IP` only from Traefik (`TODO(phase-4)` in `api.go`), data-key rotation; UI: Account › API tokens, Settings › Single sign-on | `api.go`, `server.go`, `internal/store` (new tables + migrations), Login page |

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
    169.254/16, and since 2026-10-06 TCP 443 to the entities `host` and
    `remote-node`: the cluster's own hostnames resolve to node addresses,
    which CIDR rules never match; see docs/repro-2026-10-06.md). Same-project apps still need `allowFrom` or a rule (the
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
  policies stayed Kubernetes NetworkPolicies (deny ingress + egress) until
  2026-10-06, when they became CiliumNetworkPolicies of the same name
  (`<task>.task`) to reach the nodes; a Task's old NetworkPolicy goes with
  the Task.
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

### As built (W4)

Code: `internal/auth/oidc.go` (+ `oidctest`, an in-process provider),
`internal/server/api_sso.go`, `api_tokens.go`, `kubeproxy.go`,
`clientip.go`, `api_datakey.go`; store migration 4 (`api_tokens`,
`user_identities`); `ConsoleSettings.spec.sso`; UI in Login, Account ›
API tokens, Settings › Single sign-on and Data key.

- **Single sign-on.** coreos/go-oidc + x/oauth2: code flow, PKCE S256, a
  state and a nonce per sign-in, kept server-side behind a SameSite=Lax
  `__Host-` cookie (10 min, single use). Presets: Google (fixed issuer),
  Microsoft Entra (one tenant ID; no `email_verified` claim, so the tenant
  is trusted and `preferred_username` stands in for a missing email),
  Keycloak and generic OIDC (`email_verified` must be true). GitHub is not
  OIDC for sign-in; put Keycloak or Dex in front of it. Linking: a known
  (issuer, subject) signs in as its user; otherwise the verified email of a
  user links it (one subject per user and issuer — another account with the
  same email is refused), or an open invite is accepted, or — with
  auto-join, which requires allowed domains — a developer/viewer account is
  made. Allowed domains apply to every sign-in. SSO-made accounts have no
  password; a sign-in within 10 minutes stands in for it (to set a password
  or a factor). Settings: owners and admins, written as the user; the
  discovery document is fetched before saving; the client secret goes to
  `kwerft-oidc-client` (Role `kwerft:oidc-client`: patch only), which the
  Domain reconciler creates empty next to the DNS token and the console
  reads with its own identity. Changing issuer or client ID needs the
  secret again.
- **2FA and "require 2FA" (W1).** A provider sign-in replaces the password,
  not the member's own factors: `pendingSecondFactor` (api_mfa.go) parks
  SSO sign-ins exactly like password sign-ins and the Login page continues
  at `?second-factor=…`. W1's "require 2FA" check belongs in that same
  place (or in `startSession`) so it covers both paths.
- **API tokens.** 256-bit `kwft_…`, SHA-256 stored, hint shown in lists,
  ≤ 50 active per user, 90 days default / 365 max, expired ones listed 30
  days then deleted. Effective role = min(cap, the user's role now), copied
  into the principal, so `requireRole` and impersonation need no changes.
  Tokens may not use account/tokens/members/invites/SSO/data-key routes or
  shells. Bearer requests skip the same-origin check, never fall back to
  cookies, and are rate limited (20 failures per IP per 15 min block the
  IP; 1200 requests per token per minute). Audited: `token.created`,
  `token.revoked`, `token.rejected`, `token.denied`.
- **Project restriction.** Impersonated groups cannot express it, so it is
  enforced in front of Kubernetes: console routes must carry an allowed
  `{project}` (plus `GET /api/v1/session`, `GET /api/v1/roles`); the proxy
  allows only namespaced paths in those namespaces, the Project objects,
  discovery and self-reviews. **For W1:** list routes without `{project}`
  (apps, tasks, schedules, volumes, domains, logs, metrics, alerts) are
  refused for restricted tokens today; once `projectScope` intersects with
  `principal.token.Projects`, add them to `tokenProjectless` in
  api_tokens.go.
- **Kubernetes proxy `/k8s/`.** Bearer tokens only; strips Authorization,
  Cookie, Impersonate-*, X-Forwarded-*, X-Remote-*; refuses requests that
  carry Impersonate-* (`kubectl --as`), exec/attach/portforward/proxy
  subresources, any Upgrade, encoded or unclean paths, the legacy `/watch/`
  prefix, and **all Secrets** (a patch answers with the object, so the
  "patch, never get" Secrets — DNS token, OIDC secret, Git and channel
  credentials — would be readable through kubectl). Watches and
  `logs -f` stream. Writes audited as `kube.write`, refusals as
  `kube.denied`. Exec stays in the console, recorded.
- **Client IP.** `ClientIP(r)` (W3's lock-out check) believes `X-Real-Ip`
  only from loopback or a node address: Node InternalIP/ExternalIP and
  CiliumNode `spec.addresses` (the CiliumInternalIP of cilium_host, which
  is the source Traefik's connections from the host network carry),
  refreshed every 30 s by `NodePeers` (new RBAC: ciliumnodes get/list).
  To check on the server: `kubectl get ciliumnodes -o yaml` and compare
  with the peer in the console log (`ip` of an audit entry from a direct
  pod curl vs. a browser request).
- **Data key.** Sealed values are now `v2:<key ID>:…` (0.1's `v1:` still
  opens and is re-sealed at start-up). Settings › Data key (owners,
  password): new key → Secret (`key` new, `previous` old) → re-seal in one
  transaction → `previous` removed. The Secret is written with the
  console's own identity on purpose: a patch right would let owners read
  the key. Chart: `KWERFT_DATA_KEY_PREVIOUS` (optional) and
  `KWERFT_DATA_KEY_SECRET` env, datakey.yaml keeps `previous`.
- **Try SSO:** create an OAuth client (Google: Web application; Entra:
  single-tenant app registration + client secret; Keycloak: confidential
  client, standard flow) with the redirect URI
  `https://<console>/api/v1/sso/callback` (shown in Settings), then fill in
  Settings › Single sign-on.

## e2e install runs (Phase 5)

Moved to Phase 5. The design stays: secret `HCLOUD_TOKEN` (a separate
Hetzner test project); per release tag and nightly a fresh install and an
upgrade from the previous release on `kwerft-e2e=true` Cloud servers with an
sslip.io hostname and Let's Encrypt staging; assert install, console, an
App on HTTPS, a Task, a Git build, metrics, logs and an alert; destroy; a
sweeper removes servers older than 3 h.

## Verification

Each worker: `make check` green (and `GOTOOLCHAIN=go1.26.0 go vet ./...`),
envtest tests, fakes for external APIs (Hubble, OIDC provider).
Nobody touches the test server or any real infrastructure; the coordinator
merges and verifies.
