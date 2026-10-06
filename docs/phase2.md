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
  repository. Pulls need no credential; pushes need the project's own
  (since 2026-10-06, see Registry credentials below).
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
  like the DNS token. The GitConnection reconciler creates the Secret (with
  a generated `webhook-secret`, owned by the connection) and keeps the Role
  `kwerft:git-credentials` in `kwerft-builds` that grants owners and admins
  `patch` on exactly the existing connections' Secrets; the console patches
  credentials as the user. A Secret left by a deleted connection of the
  same name is never adopted (its credentials may be for another host).
- Commit checks: context `kwerft/<project>/<app>`, target URL
  `https://<console>/apps/<project>/<app>?build=<build>`. The reporter
  records the reported phase in the Build annotation
  `kwerft.dev/reported-phase` (and a GitHub check run in
  `kwerft.dev/check-run`).
- Webhook Builds are named deterministically per App, trigger, pull request
  and commit (redeliveries build nothing new); pull requests from forks are
  never built.

## Console API

W3 (Git connections, checks, triggering):

```
GET    /api/v1/git/connections                       → [Connection]
POST   /api/v1/git/connections                       ← ConnectionInput → {connection, webhookSecret, warning?}
PUT    /api/v1/git/connections/{name}                ← ConnectionInput (secrets optional) → Connection (+ warning?)
DELETE /api/v1/git/connections/{name}                (409 {error, apps[]} while Apps use it)
POST   /api/v1/git/connections/{name}/webhook-secret → {webhookSecret}   (rotate; shown once)
POST   /api/v1/git/check   ← {repository, branch?, connection?, path?, dockerfile?, project?}
                           → {ok, message, defaultBranch?, head?: Commit, dockerfile?: bool, connection?: string}
POST   /api/v1/projects/{p}/apps/{a}/builds  ← {commit?}  (empty: branch head) → Build
POST   /api/v1/hooks/git/{connection}        (public; provider signature checked)
                           → {event, builds[], existing[]?, ignored?}

Connection = {name, provider, url, auth, owner?, projects[], account?, ready, message?,
              webhookURL?, webhookAutomatic: bool, webhooks: [{repository, automatic, message?}],
              lastDelivery?, githubApp?: {appID, installationID, slug?}}
ConnectionInput = {name, provider, url, auth, owner?, projects?, token?, sshPrivateKey?, knownHosts?,
                   githubApp?: {appID, installationID, privateKey?}}
Commit = {sha, message, author, time?}
```

Notes (W3): field errors are `400 {error, field}` with the ConnectionInput
field names (`githubApp.appID` etc.). A PUT that changes the host or the
kind of sign-in needs the new credentials (they never follow to another
host); credentials of another kind are removed. `/git/check` without
`connection` picks a ready connection that covers the repository (and
`project`, if given), else tries anonymously; owners, admins and developers
may call it. `knownHosts` is optional: the reconciler records the host key
of the connection URL's host (port 22) on first use.

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

## Registry credentials (as built, 2026-10-06)

Until v0.6 zot had no authentication: a build of project A could push tags
into project B's repository (running Apps were safe only through digest
pinning). Now every push needs a credential, and a project's credential
pushes to that project's repositories and nowhere else.

**Who is who in zot** (`internal/controllers/registry_auth.go`,
`RegistryAuthReconciler`, flag `--registry-auth`, set by the chart when
`registry.enabled`):

| User | Secret | May | Used by |
|---|---|---|---|
| `project-<project>` | `registry-<project>` in `kwerft-builds` (type `kubernetes.io/dockerconfigjson`, owned by the Project) | read, create, update in `<project>/**` | that project's build pods (BuildKit push and cache) |
| `kwerft` | `kwerft-registry-admin` in `kwerft-system` | read, create, update, delete everywhere (zot's `adminPolicy`) | the App reconciler's `RegistryKeeper` (keep tags and their removal) |
| anonymous | — | read everywhere | the nodes' containerd through the k3s mirror, `RegistryKeeper` reads |

Passwords are 256 random bits; the Secrets also hold the user's htpasswd line
(bcrypt, the minimum cost, since zot checks it on every authenticated
request and no cost factor makes 256 random bits harder to guess), so zot's
files only change when a credential does. A Secret that is missing or
inconsistent is replaced with a new credential; a deleted project's Secret is
garbage-collected and removed by the reconciler, and its user leaves zot.

**Who can read the credentials.** No console role reaches Secrets in
`kwerft-builds` or `kwerft-system` (`roles.yaml`; the isolation tests hold
it), so no user reads any registry credential, not even their own project's
(nobody needs to: pushes happen in builds). A build pod mounts only its own
project's Secret (`/kwerft/registry/config.json`, `DOCKER_CONFIG`), in the
build container only; the clone container never sees it. The repository's
own code runs in that container under BuildKit and may be able to read it:
that is acceptable, because the credential only lets it do what any build
of the project does anyway — push to the project's own repositories.

**Access control** (rendered into zot's `http.accessControl`; zot picks the
longest repository pattern that matches, then the admin policy):
`"**"` — anonymous read, nothing else; `"<project>/**"` — anonymous read, and
read/create/update for `project-<project>`. Unit tests check the rendered
config with zot's decision rule (`TestRegistryConfigIsolatesProjects`:
project A's user is refused B's repositories, a project name that starts
like another's, and anything outside a project). Checked against
zot-minimal v2.1.21 itself (podman, 2026-10-06): a push with project A's
credential to B's repository is refused (403, also by BuildKit
v0.33.1-rootless with the build pod's `DOCKER_CONFIG`), anonymous pushes
and deletes are refused (401), A's own pushes, cache export/import and
re-tags work, anonymous pulls by tag and digest work, and only `kwerft`
writes and deletes keep tags.

**How zot gets it.** The chart's ConfigMap `kwerft-registry` stays the base
(storage, retention, gc, listen address). The reconciler adds `http.auth`
(htpasswd at `/etc/zot/htpasswd`) and `http.accessControl` and writes
`config.json` and `htpasswd` to the Secret `kwerft-registry-auth`, which the
chart mounts into zot instead of the ConfigMap. zot 2.1 reloads both files
when they change (its config hot-reloader and htpasswd watcher poll the
files every second, which catches the kubelet's symlink swap), so a new
project needs no restart; checked with a kubelet-style volume. The kubelet
updates a Secret volume up to about a minute after the Secret changed, so
a new credential is not live at once: the reconciler asks zot (an
authenticated read in the project's repositories: 404 once zot knows the
user, 401/403 before) and then marks the project's Secret with
`kwerft.dev/registry-active` (a fingerprint of the credential). The Build
reconciler starts a build only when its project's credential is active
("Waiting for the project's registry credentials" until then).

**Reads stay anonymous (decided 2026-10-06).** The nodes' containerd pulls
through the k3s mirror; giving it a credential means `configs` with auth in
`/etc/rancher/k3s/registries.yaml` on every node, joined nodes and agent
clusters included (the join API would have to hand it out), and a k3s
restart on each when it changes. Not worth it for this change, so
`anonymousPolicy: ["read"]` stays. Consequence: anything that reaches zot
can pull any project's images — the nodes, the console, and **build pods**
(a project's build can `FROM` or `curl` another project's image). Images
built from private repositories contain their source, so this is a real,
remaining cross-project read. Apps' pods cannot reach zot (CiliumNetworkPolicy).
The next step, when it matters: a node-level read credential in
`registries.yaml` (installer + join API + agent hand-over), then
`anonymousPolicy: []` and per-project read for the builds. Authenticated
users are already confined: project A's user is refused B's repositories
even for reads (403), so only anonymous reads are open.

**Upgrade from an open registry.** Nothing changes on the nodes:
`registries.yaml` carries no credential, so the installer leaves it and k3s
alone (bats test), and running Apps keep pulling their digest-pinned images
anonymously. The chart switches zot from the ConfigMap to the Secret, so zot
restarts once (as on any config change; `Recreate`) and its new pod waits
until the new controller has written `kwerft-registry-auth` (seconds after
it wins leader election), for which `helm --wait` waits. Existing images and
tags stay where they are. Builds that were running during the upgrade were
started without a credential: while such a Job (no
`kwerft.dev/registry-auth` label) runs, its own repository
(`<project>/**/<app>`, a pattern that is always longer than
`<project>/**`) also takes anonymous pushes, and the exception ends with
the Job; a push that fell into zot's restart fails as before. A rollback to
the previous chart mounts the ConfigMap again (an open registry, as
before); the Secrets left behind are harmless.

**Tests.** Unit: rendering and zot's decision rule, credentials, the probe,
the legacy repository of a Job, `RegistryKeeper` with credentials, the
build Job (volume, mount, `DOCKER_CONFIG`, label). envtest: credentials per
project (types, owners, labels, distinct passwords, htpasswd, admin),
stable across reconciles, a broken one replaced, removed with the project;
legacy builds' anonymous push and its end; a build waits until the registry
accepts the credential. Chart: `make helm-lint` checks zot mounts the Secret
and `--registry-auth`. e2e: "Registry refuses other projects' pushes" asks
zot from the server, with the e2e project's credential, for its own
repository (200), a push into another project (403), an anonymous push
(401) and an anonymous pull (200).

**Manual check on a server** (as root on the console's node):

```sh
pw=$(k3s kubectl -n kwerft-builds get secret registry-<A> -o jsonpath='{.data.password}' | base64 -d)
curl -s -o /dev/null -w '%{http_code}\n' -u "project-<A>:$pw" -X POST http://10.43.0.50:5000/v2/<B>/x/blobs/uploads/   # 403
curl -s -o /dev/null -w '%{http_code}\n' -X POST http://10.43.0.50:5000/v2/<A>/x/blobs/uploads/                        # 401
curl -s -o /dev/null -w '%{http_code}\n' http://10.43.0.50:5000/v2/<A>/<app>/tags/list                               # 200
```

**Open.** Anonymous reads (above). Not yet tried on a real server: the
upgrade of an install with existing images and a build running across it,
and how long the kubelet takes to update zot's Secret volume there (builds
of a new project wait for it).
