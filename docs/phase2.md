# Phase 2 — Builds from Git: work split and contracts

Exit criterion: a push to main is live as a new revision in under 3 minutes
for a typical Go or Node app, with the build log in the UI and a check on the
commit.

Four workers build this in parallel, each in its own branch. This file is the
contract between them; change it only together with the code on both sides.
Shared, already on main: `api/v1alpha1` (`Build`, `BuildSource`,
`GitConnection`, `AppRevision.Commit`) and `internal/builds` (namespace,
registry names, credential keys, `builds.New`).

## Flow

1. A push webhook (W3) or "Build now" (W3 API) creates a `Build` in the App's
   project namespace with `builds.New` — the controller's own identity for
   webhooks, the user's (impersonated) for "Build now".
2. The Build reconciler (W2) numbers it, queues it (at most
   `--max-concurrent-builds`, default 1), and runs a Job in `kwerft-builds`:
   clone the commit with the connection's credentials, build with rootless
   BuildKit (Dockerfile or Railpack), push `builds.ImageRef(...)` to zot with
   the layer cache at `builds.CacheRef(...)`. Status: phase, number, pod,
   image, digest, times, message.
3. The App reconciler (W2) runs, for a Git app, `spec.source.git.pinnedImage`
   if set, else the image of the App's newest `Succeeded` Build with
   `spec.deploy` (by completion time). A changed image is a new revision; the
   revision records `build` and `commit`. It watches Builds. Builds a running
   revision uses are never pruned.
4. The commit status reporter (W3) watches Builds and reports pending /
   success / failure to the provider (token and GitHub App connections).
5. The console (W4) lists Builds, streams the build pod's log, cancels, and
   shows commits in revisions.

## Ownership (who edits what)

| Worker | Owns | Touches lightly |
|---|---|---|
| W1 registry & infrastructure | `install/`, `charts/kwerft/templates/` (new files: registry, builds namespace, policies), `hack/dev-server.sh` | `docs/plan.md` memory table |
| W2 build engine | `internal/controllers/build_*.go`, `app_controller.go` image resolution, `task_render.go`, new `cmd/kwerft` subcommands (if any), build flags in `main.go` | `roles.yaml` + `internal/access` (developers create/patch Builds) |
| W3 Git connections, webhooks, checks | `internal/git/` (providers), `internal/controllers/gitconnection_*.go`, `internal/controllers/commitstatus_*.go`, `internal/server/api_git.go` + hooks | `roles.yaml` + `internal/access` (GitConnections), `main.go` registration |
| W4 console | `web/`, `internal/server/api_builds.go` | `internal/server/server.go` route registration |

Everyone may add constants to `internal/builds`; keep additions small and
mention them in the commit message.

## Names and flags

- Builds run in namespace `kwerft-builds` (PSA label chosen by W1/W2 together:
  rootless BuildKit needs `seccompProfile: Unconfined` and AppArmor
  `unconfined`, so `privileged` on that namespace only). Ingress denied;
  egress to the internet (clone, base images) and the registry.
- Registry: zot, Service `kwerft-registry` in `kwerft-system`, port 5000,
  plain HTTP. Image names use `registry.kwerft.internal:5000` (k3s
  `/etc/rancher/k3s/registries.yaml` mirror on every node, written by the
  installer and join script). Ingress to zot only from `kwerft-builds` pods,
  the console's own pod and the nodes. The Service has the fixed ClusterIP
  `builds.RegistryClusterIP` (10.43.0.50), which the nodes' mirror points
  at; cluster DNS does not know `registry.kwerft.internal`, so build pods map
  it with `hostAliases` to that address (BuildKit pushes to the name in the
  image reference; its registry config needs `http = true` for it). zot keeps
  `buildcache`, the newest 20 tags and tags pulled within 90 days per
  repository.
- Build namespace (chart): Pod Security `privileged`, a LimitRange (default
  request 500m / 1Gi, limit 3Gi, max 6Gi per container) and a ResourceQuota;
  a NetworkPolicy denies all ingress and allows egress to DNS, zot (5000)
  and the internet only.
- Labels: Build Jobs and pods carry `kwerft.dev/build=<build name>` and
  `kwerft.dev/project=<namespace>`.
- Controller flags (W2 implements, W1 passes from the chart's values, pinned
  at the top of `install/install.sh`): `--buildkit-image`,
  `--railpack-image`, `--max-concurrent-builds`, `--build-timeout`
  (default 30m).
- Cancel: annotation `kwerft.dev/cancel-requested` on the Build (as for
  Tasks); the reconciler deletes the Job and sets phase `Cancelled`.
- Credentials: Secret `git-<connection>` in `kwerft-builds`, keys in
  `internal/builds`. Write-only for owners and admins (patch, never get),
  like the DNS token.

## Console API

W3 (Git connections, checks, triggering):

```
GET    /api/v1/git/connections                       → [Connection]
POST   /api/v1/git/connections                       ← ConnectionInput → {connection, webhookSecret}
PUT    /api/v1/git/connections/{name}                ← ConnectionInput (secrets optional) → Connection
DELETE /api/v1/git/connections/{name}
POST   /api/v1/git/connections/{name}/webhook-secret → {webhookSecret}   (rotate; shown once)
POST   /api/v1/git/check   ← {repository, branch?, connection?, path?, dockerfile?}
                           → {ok, message, defaultBranch?, head?: Commit, dockerfile?: bool, connection?: string}
POST   /api/v1/projects/{p}/apps/{a}/builds  ← {commit?}  (empty: branch head) → Build
POST   /api/v1/hooks/git/{connection}        (public; provider signature checked)

Connection = {name, provider, url, auth, owner?, projects[], account?, ready, message?,
              webhookURL?, webhookAutomatic: bool, lastDelivery?, githubApp?: {appID, installationID, slug?}}
ConnectionInput = {name, provider, url, auth, owner?, projects?, token?, sshPrivateKey?, knownHosts?,
                   githubApp?: {appID, installationID, privateKey?}}
Commit = {sha, message, author, time?}
```

W4 (reading builds):

```
GET  /api/v1/projects/{p}/apps/{a}/builds            → [Build]   newest first
GET  /api/v1/projects/{p}/builds/{name}              → Build
GET  /api/v1/projects/{p}/builds/{name}/logs         SSE, like Task logs; follows while running
POST /api/v1/projects/{p}/builds/{name}/cancel

Build = {name, number, app, commit, branch?, message?, author?, trigger, requestedBy?, pullRequest?,
         deploy, phase, image?, digest?, started?, finished?, durationSeconds?, message?, deployedRevision?}
```

App JSON gains for Git apps: `source: {type: "git", repository, branch, path,
builder, dockerfile, connection?, autoDeploy, pinnedImage?}`, `latestBuild?:
Build`, and revisions gain `build?`, `commit?`.

Authorisation: every read and write is impersonated (`get builds` in the
project). Build logs come from the pod in `kwerft-builds`, which users cannot
read; the console reads it with its own identity only after the impersonated
`get` of the Build succeeded.

## Verification

Each worker: `make check` green (Go 1.26 for CI too), envtest tests for
reconcilers, fakes for provider APIs (like `internal/hetzner/hetznertest`).
The coordinator merges, deploys to the test server and runs the exit
criterion with a real repository.
