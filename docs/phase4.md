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
