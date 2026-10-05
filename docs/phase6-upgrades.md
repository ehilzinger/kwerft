# Phase 6 — Upgrades from the console: design and work split

Decided 2026-10-05:
- Owners upgrade Kwerft from **Settings › Updates**. The console runs the
  release's own `install.sh` on the host and rolls back by itself when the
  upgrade fails.
- Kubernetes (k3s) upgrades are offered in the console too, **patch and
  minor**, as a separate step with its own confirmation and no automatic
  rollback.
- Update policy **Notify** by default; **AutoPatch** (install patch releases
  in a maintenance window) is opt-in. Minor releases always need a click.

Exit criteria:
1. A server on release N-1 upgrades to N from Settings › Updates, apps
   serving throughout (the console itself restarts for a few seconds). Also
   on a 3-node cluster (one control-plane server, two workers) with an agent
   cluster attached, which is upgraded after the console.
2. A forced failure after the Kwerft stage ends in `RolledBack`: the console
   is back on N-1 and apps were never touched.
3. A k3s patch and a k3s minor upgrade run from the console on the 3-node
   cluster, node by node, joined nodes included.
4. AutoPatch installs a patch release inside its window and never outside.

## What re-running the installer upgrades today

Re-running the installer of a newer release is the only upgrade path today,
and the e2e upgrade run tests it. It converges the forced stages
(`install/install.sh:2065-2089`): preflight, firewall, registry mirror,
network (Cilium), Hetzner Cloud, ingress & TLS, observability, Kwerft and
handoff. It does **not** re-run the stages recorded as done:

- **Kubernetes**: k3s keeps the version it was first installed with, on
  every node. Joined nodes (`stage_join`) are never upgraded either.
- **System**: packages and sysctls added by a later release are not applied.
- **Helm**: the helm binary keeps its first version.

The header of `install.sh`, the blueprint and `RELEASING.md` say a re-run
"upgrades"; that holds for Kwerft and the platform charts, not for k3s. W2
fixes the wording along with the stages.

## Architecture

- **The release's own installer does the work.** The console does not
  reimplement the installer's Helm steps in Go: that would be a second source
  of truth that drifts, and the installer re-run is already the tested path.
  The console decides *when* (preflight, policy, window), runs the installer
  on the host, verifies and rolls back.
- **The watcher is the old version.** Upgrading replaces the console pod, and
  a broken new console cannot roll itself back. A separate **runner** pod,
  running the *current* console image (`kwerft upgrade-runner`), drives the
  run from backup to verification and performs the rollback. It uses the host
  network, so a broken Cilium upgrade does not cut it off.
- **The installer runs as a transient systemd unit on the host**
  (`systemd-run --unit kwerft-upgrade-<name>`). It outlives the runner pod,
  the console pod and k3s restarts (`KillMode=process`).
- **Kubernetes is the source of truth.** An `Upgrade` object holds the
  request, the backup references and the progress. The controller and the
  runner resume from it after any restart.
- **k3s moves through system-upgrade-controller (SUC)**, one node at a time.
  The installer on one host cannot reach joined nodes, and SUC can.

```
Settings › Updates ──► API (as the user) creates Upgrade kwerft-0.6.0-x7k2p
                          │
 Upgrade controller ──► Preflight ─► Backup (SQLite) ─► runner Job
                                                           │
 runner (old image) ──► etcd snapshot, Helm revisions ─► systemd-run install.sh
                        tails progress.jsonl ─► status.steps
                        exit 0 ─► Verifying ─► Succeeded
                        exit ≠0 / verify fails ─► RollingBack ─► RolledBack | Failed
```

## The `Upgrade` resource

`api/v1alpha1/upgrade_types.go`, cluster-scoped, one per attempt
(`generateName: <component>-<version>-`). At most one is active per cluster.
Others wait in `Queued`, and NodePool changes wait for it too.

```yaml
apiVersion: kwerft.dev/v1alpha1
kind: Upgrade
metadata:
  name: kwerft-0.6.0-x7k2p
  annotations:
    kwerft.dev/requested-by: alice@example.com   # set by the API; "auto-update" for the policy
spec:
  component: Kwerft            # Kwerft | Kubernetes
  version: 0.6.0               # a Kwerft release, or a k3s version (v1.38.1+k3s1)
  acceptDataRollback: false    # required for releases with rollbackSafe: false (below)
status:
  phase: Running               # Queued Preflight Backup Running Verifying Succeeded
                               # RollingBack RolledBack Failed Cancelled
  from: { kwerft: 0.5.0, kubernetes: v1.37.1+k3s1 }
  preflight:                   # each check with its result; failed checks block
    - { check: ImagePullable, ok: true }
  backup:
    etcdSnapshot: pre-kwerft-0.6.0-x7k2p
    database: /data/backups/pre-kwerft-0.6.0-x7k2p.db
    helmRevisions: { kube-system/cilium: 3, kwerft-system/kwerft: 12 }
  steps:                       # one per installer stage, its summary line as printed
    - { id: network, label: Network, state: Done, detail: "Cilium 1.20.3 · kube-proxy replacement · WireGuard", at: … }
  nodes:                       # Kubernetes only: per node, from SUC
    - { name: kwerft-1, version: v1.38.1+k3s1, state: Done }
  reason: Kwerft               # Usage Preflight Network Kubernetes Platform Kwerft (exit codes 2–50) | Verify | Timeout
  message: "…"
  startedAt: …
  finishedAt: …
```

The last 64 KiB of `/var/log/kwerft/install.log` are kept in ConfigMap
`kwerft-system/<upgrade>-log`. The newest 20 Upgrades are kept; older ones
are deleted with their log ConfigMaps.

## Releases: manifest and discovery

The release workflow (W1) also writes to `kwerft-install`:

- `v<ver>/manifest.json`, generated by `hack/release.sh` from the pinned
  block of the stamped `install.sh`:

  ```json
  {
    "version": "0.6.0",
    "channel": "stable",
    "published": "2026-11-02T10:00:00Z",
    "upgradeFrom": "0.4.0",
    "rollbackSafe": true,
    "kubernetes": { "pinned": "v1.38.1+k3s1", "supported": ["1.37", "1.38"] },
    "components": { "cilium": "1.20.3", "helm": "v4.3.0", "certManager": "v1.21.2", "…": "…" },
    "image": "ghcr.io/ehilzinger/kwerft@sha256:…",
    "chart": { "ref": "oci://ghcr.io/ehilzinger/charts/kwerft", "version": "0.6.0", "digest": "sha256:…" }
  }
  ```

- `v<ver>/NOTES.md`: the release notes. The GitHub Release lives in the
  private repository, which consoles cannot read.
- `releases.json` at the top level: every release, newest first, with
  `version`, `channel` and `published`. `LATEST` stays for the installer.

`upgradeFrom` defaults to two minor releases back, which is what e2e tests.
`rollbackSafe` defaults to true. A release overrides both with trailers in its
tag message (`Upgrade-From: 0.5.0`, `Rollback-Safe: no`), so nothing is
bumped in the source tree.

**Discovery.** With a policy other than `Off`, the controller fetches
`releases.json` every 6 h and on **Check now**. It filters by channel (the
installer's `--channel` seeds it), reads the manifests of newer releases and
writes `ConsoleSettings.status.updates`:

```yaml
status:
  updates:
    checkedAt: …
    current: { kwerft: 0.5.0, kubernetes: v1.37.1+k3s1 }
    available:
      - { component: Kwerft, version: 0.5.1, kind: Patch, notes: "…", allowed: true }
      - { component: Kwerft, version: 0.6.0, kind: Minor, allowed: true }
      - { component: Kubernetes, version: v1.37.2+k3s1, kind: Patch, allowed: true }
    error: ""
```

Only the console's backend talks to GitHub; the browser gets notes from the
API (CSP stays `'self'`). `Off` makes no outbound requests at all.

**Which targets are offered.**
- **Kwerft:** every newer release on the channel. If `upgradeFrom` excludes
  the current version, the newest release that allows it is offered as the
  intermediate step.
- **Kubernetes:** k3s versions come **only from Kwerft release manifests**,
  never from k3s's own channel server, so every k3s version a console offers
  was pinned and e2e-tested by some Kwerft release. A target is the installed
  release's `kubernetes.pinned`, and is offered when it is newer than the
  running version and at most one minor ahead.
- **Order:** a Kwerft release supports its pinned k3s minor and the one
  before (`kubernetes.supported`). Kwerft goes first, then k3s catches up. A
  Kwerft upgrade whose `supported` range excludes the running k3s is not
  allowed ("upgrade Kubernetes first").

## Kwerft upgrade

1. **Preflight** (controller; nothing changes yet). Failed checks block the
   upgrade and are shown in the dialog:
   - the target is allowed (above), on the channel, and newer
   - the image and chart are pullable anonymously (the installer's exit-50
     check)
   - every node is Ready
   - free disk ≥ 5 GiB on the node that ran the installer and in the
     console's data volume
   - no other Upgrade or NodePool operation is running
   - the install is not a `--image` dev install. Those are refused and
     pointed at `make dev-server`.
   - agent clusters are at most one minor behind the target (see Agent
     clusters)
   - `rollbackSafe: false` requires `spec.acceptDataRollback`
2. **Backup.**
   - The controller writes `VACUUM INTO` of the SQLite store to
     `/data/backups/` (the newest 3 are kept). The data key Secret already
     survives upgrades.
   - Then it creates the runner Job.
   - The runner takes `k3s etcd-snapshot save --name pre-<upgrade>` and
     records the revision of every Helm release (`helm list -A`).
3. **Run.**
   - The runner downloads `install.sh`, `SHA256SUMS` and `manifest.json` from
     `kwerft-install/v<ver>/` to `/var/lib/kwerft/upgrade/<upgrade>/`, and
     verifies the checksum (plus cosign once scripts are signed).
   - It starts `systemd-run --unit kwerft-upgrade-<upgrade> install.sh
     --version <ver> --yes --progress <dir>/progress.jsonl`. Every other flag
     comes from `install.env` (W2).
   - It tails the progress file into `status.steps`.
4. **Verify** (runner, within 10 min of the installer's exit 0):
   - the console Deployment is ready and `GET /api/v1/version` reports the
     target
   - the CRDs are established
   - the node agent has finished rolling out
   - `https://<consoleDomain>/` answers with a valid certificate
   - no App has fewer Ready replicas than before the upgrade
   - the Hubble relay is ready (when enabled)
5. **Rollback** on a non-zero exit or a failed check. A failing console is
   caught by the installer's `helm --wait` (10 min), so detection takes at
   most that long.
   - The runner runs `helm rollback <release> <revision> --wait` for every
     release whose revision changed, in reverse stage order (Kwerft first,
     Cilium last), as a host unit.
   - It then checks the console again on the old version.
   - Success is `RolledBack`; otherwise `Failed`, with the manual steps in
     `status.message`.
   - **Not rolled back:**
     - **CRDs**, which must only gain fields (Compatibility).
     - **The SQLite store**, whose migrations stay readable by N-1. For a
       release with `rollbackSafe: false`, rollback also restores the
       database copy, which loses writes made since the backup.
       `acceptDataRollback` is that acknowledgement.
     - **Host changes** (sysctls, nftables, `registries.yaml`), which are
       idempotent and tolerated by N-1.

The console is down for the seconds its pod is replaced (one replica). The
Updates page shows "Reconnecting…" and picks the Upgrade up again.

## Kubernetes upgrade

- **SUC** is installed by the installer (pinned,
  `SYSTEM_UPGRADE_CONTROLLER_VERSION`). The Upgrade controller writes two
  Plans:
  - `k3s-server`: control-plane nodes, concurrency 1, cordon
  - `k3s-agent`: workers, concurrency 1, drain respecting PodDisruptionBudgets
    with a 10 min timeout, waiting for the server Plan

  Both use `spec.version`. A pool with one node is only cordoned: draining it
  would evict pods with nowhere to go, and k3s restarts leave containers
  running.
- **Preflight:**
  - one minor at a time, never downward
  - every node is Ready and on the same version
  - etcd is healthy, with quorum in HA (3 servers)
  - no deprecated APIs in use that the target removes (the API server's
    `apiserver_requested_deprecated_apis` with `removed_release` ≤ target).
    This blocks a minor upgrade and warns on a patch.
  - free disk
  - no NodePool operation is running
  - every node passes the Kwerft target's `kubernetes.supported`
- **Backup:** `k3s etcd-snapshot save`.
- **Progress:** per node in `status.nodes`, from the Plans' Jobs and the
  nodes' `kubeletVersion`.
- **Verify:**
  - every node is on the target and Ready
  - Cilium, CoreDNS and Traefik are ready
  - the Gateway API CRDs (packaged with k3s ≥ 1.37) are established and the
    console's route is `Accepted`
  - App Ready replicas are back within 10 min
- **No automatic rollback.** k3s does not support downgrades, and restoring
  etcd resets the cluster. On failure the controller deletes the Plans so no
  further node is touched (kubelets one minor behind are within the skew
  policy) and sets `Failed`, naming the node and the snapshot. The restore
  procedure (`k3s server --cluster-reset --cluster-reset-restore-path=…`) is
  documented on the page and in the docs, never run by Kwerft.
- **Confirmation:** a patch upgrade asks for the password. A minor upgrade
  also asks the owner to type the target version, under "this cannot be
  rolled back automatically".
- Node pools created after an upgrade install the cluster's *running* k3s
  version, not the pin: cloud-init passes `--k3s-version` (W2) from the
  Cluster's status.

## Agent clusters

- **Kwerft:** the console upgrades itself first, then each connected cluster.
  It creates the `Upgrade` in the remote cluster through the tunnel, as the
  user. The agent's controller and a runner on that cluster's installer node
  do the rest. The console watches it there; the Updates page lists every
  cluster with its versions (`AgentVersion`, `KubernetesVersion`).
  **Upgrade all** queues them one after another.
- **Compatibility:** console N works with agents of the same minor and the
  one before (tunnel protocol, CRDs, API). A console upgrade is not allowed
  while a connected agent would fall further behind.
- **Disconnected clusters** are skipped with a warning and offered again when
  they reconnect (or upgraded by re-running the installer there).
- **Kubernetes** upgrades are per cluster and are never queued across
  clusters.

## Update policy

Management only, in `ConsoleSettings.spec.updates` of `local`, and it applies
to every cluster:

```yaml
spec:
  updates:
    channel: stable          # stable | edge
    policy: Notify           # Off | Notify | AutoPatch
    kubernetesPatches: false # AutoPatch also installs k3s patch versions
    window: { days: [Sun], start: "03:00", duration: 2h, timeZone: Europe/Berlin }
```

- **Off:** no discovery and no outbound requests.
- **Notify** (default): discovery, the notification "Kwerft 0.5.1 available"
  (blueprint) and a dot on Settings › Updates.
- **AutoPatch:** additionally, the controller creates the Upgrade for the
  newest *patch* release of the running minor when a window opens. It is
  created under the controller's own identity and annotated
  `kwerft.dev/requested-by: auto-update`, and the change of policy is
  audited.
  - A run that has not reached `Running` by the end of the window waits for
    the next one.
  - An auto-update that ends `RolledBack` or `Failed` pauses AutoPatch until
    an owner resumes it.
  - k3s patches only when `kubernetesPatches` is on; k3s minors never.

## API and UI

Owners start, cancel and change the policy; owners and admins read. The roles
matrix in `internal/access` gets `upgrades` and the chart's ClusterRoles
follow: `kwerft:owner` create/delete, `kwerft:admin` get/list/watch.

| Endpoint | |
|---|---|
| `GET /api/v1/updates` | current versions per cluster, available targets with notes and `allowed`, policy, last check |
| `POST /api/v1/updates/check` | discovery now |
| `PUT /api/v1/settings/updates` | policy, channel, window (owners) |
| `POST /api/v1/upgrades` | `{cluster, component, version, password, confirmVersion?, acceptDataRollback?}`: runs preflight synchronously, then creates the Upgrade as the user; audited `upgrade.start` |
| `GET /api/v1/upgrades?cluster=` | history |
| `GET /api/v1/upgrades/{name}` · `/events` (SSE) · `/log` | detail, live status, installer log tail |
| `DELETE /api/v1/upgrades/{name}` | cancel, only before `Running`; audited |

**Settings › Updates** (`web/src/pages/SettingsUpdates.tsx`) has:
- a versions table (component × cluster: running, available)
- the policy form
- **Check now**
- release notes
- **Upgrade…**, which opens a dialog with live preflight results, the
  password and the typed confirmation for k3s minors
- a progress view styled like the installer output (✓ / spinner per stage,
  per node for k3s)
- the history

The notification links to this page. Failed and rolled-back upgrades also go
to notification channels that subscribe to platform events (W5 decides with
the alerting owner whether that is a new event type or an `AlertRule`).

## Installer changes (W2)

- **Remember flags.** Every run writes the effective non-secret settings to
  `/var/lib/kwerft/install.env` (0600). Precedence: flag, then `KWERFT_*`
  environment variable, then `install.env`, then the default.
  - Stored: `--email`, `--acme-server`, `--platform`, `--private-iface`,
    `--lite`, `--harden-ssh`, `--channel`, the mode, and `--console` (agent).
  - Not stored: the domain (the cluster setting is the record), and tokens.
    In agent mode the token is read back from Secret
    `kwerft-system/kwerft-agent`.
  - This removes "give `--acme-server` on every run".
- **`--progress FILE`**: one JSON line per stage,
  `{"id","label","state":"ok|skip|fail","detail","at"}`, then
  `{"exit":<code>}`. Written from `run_stage`, `ok`, `skip` and `die`.
- **Stages that converge:** `system` and `helm` become forced; both are
  idempotent already. `kubernetes` and `join` stay once: k3s belongs to the
  Kubernetes upgrade. When the running k3s is older than the pin, the summary
  says so ("k3s v1.37.1 is older than this release's v1.37.2: upgrade it in
  Settings › Updates").
- **New forced stage `upgrades`** after Helm: installs SUC at its pin.
- The server that runs the installer gets the node label
  `kwerft.dev/installer=true`, where `/var/lib/kwerft/stages` lives. The
  runner is scheduled there.
- `--k3s-version V` for joins and agent bootstraps (node pools pass the
  cluster's running version).
- Wording: the header comment, `RELEASING.md` and the blueprint say what a
  re-run upgrades and what it does not.
- Exit codes are unchanged.

## Compatibility rules

These are written down in `CONTRIBUTING`-style notes in `docs/plan.md` and
enforced where they can be:

- **CRDs only gain optional fields** within `v1alpha1`: no removals, renames,
  new required fields or narrowed enums. `hack/crdcompat` compares the chart's
  CRDs with the previous release tag in the release workflow and fails unless
  the tag says `Rollback-Safe: no`.
- **SQLite migrations stay readable by N-1:** expand in one release, contract
  (drop) at the earliest in the next. A release that cannot do that says
  `Rollback-Safe: no`.
- **Console ↔ agent** (tunnel, info endpoint, CRDs): one minor apart works.
- **Kwerft ↔ k3s:** each release supports its pinned minor and the one
  before.

## Security

- **Only owners can start an upgrade**, as the user (impersonation), after
  their password. The runner and the SUC Plans are created by the controller
  under its own identity, as every reconciler does with what it is asked for.
- **The runner is root on the host** (systemd-run, chroot into the host's
  `/usr` like the node agent, `/var/lib/kwerft` read-write, host network).
  Only the Upgrade controller creates it, in `kwerft-system`, from the
  running console image pinned by digest. Its service account may read
  nodes, workloads and CRDs, and patch `upgrades/status`; the host's helm
  uses the host kubeconfig, as the installer does.
- **What runs is the published `install.sh`, checksum-verified.** Until
  scripts are signed (cosign `sign-blob`, follow-up), that protects against
  corruption, not against a compromised install repository. That is the same
  trust as today's `curl | sudo bash`.
- **Discovery reads only `kwerft-install`** and sends nothing beyond a plain
  GET (no version or instance identifiers in the request).

## Ownership

Contracts first: W3 lands `upgrade_types.go` with the generated files, and W2
lands the `install.env` and `--progress` formats, before the rest branch off.

| Worker | Owns | Touches lightly |
|---|---|---|
| W1 Release manifest | `hack/release.sh` and `release.yml`: `manifest.json`, `NOTES.md`, `releases.json` in `kwerft-install`; tag trailers; `hack/crdcompat` | `RELEASING.md` |
| W2 Installer | everything under "Installer changes"; SUC pin and stage; bats tests | `nodepool_cloudinit.go` (`--k3s-version`) |
| W3 Upgrade core | `Upgrade` type, controller (discovery, preflight, SQLite backup, queueing, policy and windows), `kwerft upgrade-runner` (download, verify, etcd snapshot, systemd unit, progress, verify, rollback), chart RBAC and runner Job | `ConsoleSettings` type, `internal/store` (backup) |
| W4 Kubernetes upgrades | SUC Plans, per-node progress, k3s preflight (deprecated APIs, etcd, skew), failure handling, restore docs | NodePool controller (version from the Cluster) |
| W5 API and UI | endpoints, roles matrix, audit, Settings › Updates, notification, platform-event notifications | `web/src/settings.ts`, router |
| W6 Agent clusters | remote Upgrades through the tunnel, Upgrade all, version gating | `internal/clusters`, Clusters pages |
| W7 e2e | `hack/e2e --via-console`: install N-1, upgrade through the API, assert; forced-failure run (runner fault injection, honoured only with chart value `e2e.faults=true`); k3s patch and minor on 3 nodes (installer `KWERFT_K3S_VERSION` override, dev only) | `e2e.yml` |

## Verification

Workers use fakes and envtest. The real runs need Hetzner Cloud servers from
the test project, and **the coordinator asks before creating any**:
- a single server, N-1 → N through the console, plus the forced failure
- a 3-node cluster with an agent cluster: Kwerft, then a k3s patch, then a
  k3s minor
- AutoPatch with a window that opens during the run

The production server (`46.224.139.73`) and `kwerft-dedi-1` are not
upgraded by these tests.

## Open questions

- **Host configuration of joined nodes.** The upgrade re-runs the installer
  on one server only; a release that changes host prerequisites (packages,
  sysctls) on every node needs a node-level step. One candidate is a third
  SUC Plan running `install.sh --join --refresh`.
- **A second console replica** would make the console's own restart
  invisible; it waits for the HA store (`deployment.yaml` TODO).
- **Signing `install.sh`** (above).

## As built (U1): release manifest, trailers, crdcompat

Files: `hack/release.sh` (new commands `trailers`, `crdcompat`, `manifest`;
`notes`, `install-repo` and `dry-run` extended), `.github/workflows/release.yml`,
`hack/crdcompat/` (Go tool, tests, fixtures in `testdata/`),
`install/test/release.bats` (11 new tests), `RELEASING.md` (sections "The
release manifest", "Upgrade path and rollback safety", "CRD compatibility").

**What lands in `kwerft-install`** (written by the `install-repo` job, which
needs `INSTALL_REPO_TOKEN`):

| Path | Content |
|---|---|
| `v<ver>/install.sh`, `join.sh`, `SHA256SUMS` | as before |
| `v<ver>/manifest.json` | the format above; also attached to the GitHub Release |
| `v<ver>/NOTES.md` | the GitHub Release's body: hand-written notes from the tag message (if any), the Install section with an "Upgrades from" row and a "Not rollback-safe" paragraph when it applies, then GitHub's generated "What's changed" |
| `releases.json` | a **JSON array** (not an object) of `{version, channel, published}`, newest *version* first in semver order (`0.6.0`, `0.6.0-rc.1`, `0.5.0`, `0.4.3`); a patch of an older line published later sorts by version, not date |
| `LATEST`, top-level scripts | unchanged |

URLs: `https://raw.githubusercontent.com/ehilzinger/kwerft-install/main/releases.json`
and `…/main/v<ver>/manifest.json` / `NOTES.md`. kwerft.dev redirects only
`/install.sh` and `/:version/install.sh` (`kwerft-homepage/netlify.toml`), so
discovery should read raw.githubusercontent.com directly unless someone adds
redirects there.

**Manifest details other workers rely on:**
- `channel`: `edge` for any prerelease (`-rc.N`), `stable` otherwise. Nothing
  else sets it; there is no `Channel:` trailer.
- `published`: the tag's date in UTC (`%Y-%m-%dT%H:%M:%SZ`), so re-runs give
  the same value.
- `upgradeFrom`: the `Upgrade-From` trailer (validated, ≤ the release), else
  `<line>.0` of the second-newest minor line *below* the release's own, over
  all release tags including prereleases (0.6.x → `0.4.0`; 1.0.0 after 0.9 →
  `0.8.0`). It is an inclusive lower bound and may name a version that was
  only ever an rc line's `.0`. Older consoles must take an intermediate step
  (W3).
- `rollbackSafe`: false only with `Rollback-Safe: no` (also `false`; any case).
- `kubernetes.supported`: `["1.<m-1>", "1.<m>"]` from `K3S_VERSION`, which
  must keep the form `v1.X.Y+k3sN`.
- `components`: every `NAME_VERSION="…"` line in the pinned block (before
  the first `readonly`) except `K3S_VERSION`, keyed in camelCase without
  `_VERSION`: `cilium`, `certManager`, `traefikChart`, `vmStackChart`,
  `hcloudCsiChart`, `zot`, … U2/B1's new pins appear automatically
  (`SYSTEM_UPGRADE_CONTROLLER_VERSION` → `systemUpgradeController`,
  `VELERO_CHART_VERSION` → `veleroChart`, `VELERO_PLUGIN_AWS_VERSION` →
  `veleroPluginAws`), **as long as they are `NAME="value"` lines above the
  `readonly EXIT_…` line**. Non-version pins (`HCLOUD_LOCATIONS`, the repos)
  are not components.
- `image`: `ghcr.io/ehilzinger/kwerft@sha256:<index digest>` (multi-arch).
  `chart.digest`: what `helm push` reported. `install-repo` refuses a
  manifest without both (or of another version).
- `SHA256SUMS` still covers only `install.sh` and `join.sh`; the runner
  verifies the installer against it, not the manifest.
- `make release-dry-run` writes a **draft** manifest (image by tag,
  `chart.digest: null`) and previews the install repository in
  `dist/release/install-repo/` (a scratch git repo).

**crdcompat.** `hack/release.sh crdcompat <ver>` extracts `charts/kwerft/crds`
from the newest stable tag below the release and, if newer, the newest
prerelease below it (`git archive`), and runs `go run ./hack/crdcompat -name
v<old> OLD NEW` (or `$CRDCOMPAT`). Exit 0/1/2 = compatible/incompatible/error.
Incompatible: removed CRD, served version, short name, status/scale
subresource; changed scope, kind, list kind; removed field (with a "renamed
to …?" hint); new required field or field becoming required (except inside
a new optional parent); narrowed or newly added enum; changed type; lost
int-or-string, nullable or preserve-unknown-fields; lowered max*/raised
min*/new pattern or format; closed maps; removed item schemas. Warnings only
(printed, never fail): new or rewritten CEL rules, changed (not new)
pattern or format. The workflow runs it right after the trailers, before
`make web`; `Rollback-Safe: no` turns findings into a notice. Against v0.4.0
and v0.5.0-rc.3 the foundation's CRDs pass with 8 warnings (the CEL rules
that came with Secret-file volumes). Run on v0.1.0 → v0.4.0 it would have
flagged `builds .spec.source: new required field`.

**Workflow changes.** The `release` job checks out with `fetch-depth: 0` and
re-fetches the pushed tag (`git fetch --force … refs/tags/<tag>`), because
checkout can replace an annotated tag with a lightweight one and lose the
trailers; a lightweight tag gets the defaults and a warning. New steps:
`trailers` (outputs `upgrade_from`, `rollback_safe`, `channel`), `crdcompat`,
and "Write manifest.json" after the pushes; `manifest.json` is a release
asset. The `install-repo` job downloads it with the scripts and writes the
release body to `NOTES.md`. jq is required (on the runners; macOS ships it).

**Not done / for others.** No Go type for the manifest or `releases.json`:
W3 defines its own from the format above. Releases up to 0.5.x have no
manifest and are not in `releases.json` (no backfill). Nothing was published
or tagged; the workflow changes are untested on GitHub (verified: YAML
parses, the full `make release-dry-run RELEASE_VERSION=0.6.0` locally, bats
and Go tests).

## As built (U2)

Installer changes W2, on the Phase 6 foundation. Exit codes are unchanged.

**Stages** (`--dry-run` order, install and agent mode):
`preflight system firewall kubernetes registry helm upgrades network hcloud
ingress observability kwerft|kwerft-agent handoff`. Join mode:
`preflight system firewall join registry`.
- Forced (run every time): everything but `kubernetes` and `join`; `system`
  and `helm` are forced now.
- Once: `kubernetes`, `join`. When skipped, their summary is the running k3s
  (`k3s --version`): `k3s v1.37.1+k3s1 · installed`, or `k3s v1.36.4+k3s1 is
  older than this release's v1.37.1+k3s1: upgrade it in Settings › Updates`.
- New forced stage **`upgrades`** (label `Upgrades`, after `helm`, before
  `network`; exit 40 on failure). It applies `crd.yaml` (CRD
  `plans.upgrade.cattle.io`, waited for `Established`), then
  `system-upgrade-controller.yaml` of the release pinned in
  `SYSTEM_UPGRADE_CONTROLLER_VERSION="v0.20.2"` (2026-10-05), server-side,
  from `github.com/rancher/system-upgrade-controller/releases/download/<v>/`.
  Upstream manifests, unchanged: namespace `system-upgrade`, Deployment
  `system-upgrade-controller` (control-plane nodes only), ServiceAccount
  `system-upgrade`, ConfigMap `default-controller-env` with the Job defaults
  (e.g. `SYSTEM_UPGRADE_JOB_KUBECTL_IMAGE`). The rollout is waited for
  (5 min) only when this server's node is Ready: on a first install Cilium
  comes next and the pod starts with it. Summary:
  `system-upgrade-controller v0.20.2 (ready|starts with the network) · installer node <node>`.
- The same stage labels this server's node `kwerft.dev/installer=true` (found
  by host name, else by its InternalIP) and removes the label from every
  other node: exactly one node carries it. Agent clusters get both too;
  joined nodes run neither.

**`/var/lib/kwerft/install.env`** (0600, replaced atomically through
`install.env.kwerft-new`), written after the `preflight` stage passed, on
every run that is not `--dry-run` (install, agent and join mode; never by
`--uninstall`/`--reset-firewall`; `--uninstall` deletes it with the state
directory). Format: `#` comment lines, then exactly these keys, one
`KEY=value` per line, the value unquoted and verbatim to the end of the line,
empty meaning "not set". The keys are the environment variables' names:

```
# Kwerft installer 0.6.0, 2026-10-05T10:00:00Z: settings later runs reuse.
# Flags and KWERFT_* environment variables win over this file.
KWERFT_MODE=install
KWERFT_EMAIL=ops@example.com
KWERFT_ACME_SERVER=https://acme-staging-v02.api.letsencrypt.org/directory
KWERFT_PLATFORM=cloud
KWERFT_PRIVATE_IFACE=
KWERFT_LITE=0
KWERFT_HARDEN_SSH=0
KWERFT_CHANNEL=stable
KWERFT_CONSOLE=https://ops.example.com
```

| Key | Value |
|---|---|
| `KWERFT_MODE` | `install`, `agent` or `join` |
| `KWERFT_EMAIL` | `--email`, or the email of `--config` |
| `KWERFT_ACME_SERVER` | `--acme-server` as a URL (`staging` is stored resolved) |
| `KWERFT_PLATFORM` | `cloud` or `dedicated`, as detected or given (never `auto`) |
| `KWERFT_PRIVATE_IFACE` | `--private-iface` as given; empty when detected |
| `KWERFT_LITE`, `KWERFT_HARDEN_SSH` | `0` or `1` |
| `KWERFT_CHANNEL` | `stable` or `edge` |
| `KWERFT_CONSOLE` | agent mode only |

Reading (`apply_install_env`, in `parse_args` after the flags): the file is
parsed, never sourced; unknown keys and other lines are ignored, CRLF is
tolerated, values are then validated like flags (a bad value is exit 2). A
key fills its setting only when neither a flag nor a non-empty `KWERFT_*`
variable gave one (flag > env > file > default); an email in this run's
`--config` also wins over the file. New environment variables:
`KWERFT_LITE`, `KWERFT_HARDEN_SSH` (1/true/yes, 0/false/no; `KWERFT_LITE=0`
is how a remembered `--lite` is turned off), `KWERFT_K3S_VERSION`,
`KWERFT_PROGRESS`.
- **Mode**: applied only when no flag chose one (`--join`/`KWERFT_JOIN_URL`,
  `--agent`, `--uninstall`, `--reset-firewall`). Without `KWERFT_MODE` (an
  install from before this file) the stage markers decide: `kwerft-agent` →
  agent, `join` → join, `kubernetes` → install.
- **Agent mode**: `--console` comes from the file; the token, when not
  given, is read back from Secret `kwerft-system/kwerft-agent` (key
  `token`). On an agent cluster's installer node `install.sh --version V
  --yes` needs nothing else.
- **Join mode**: a node whose `join` stage is done re-runs without `--join`
  and `--token` (converging `system`, `firewall`, `registry`); one that has
  not joined yet is refused (exit 2, "run the join command again").
- Not stored: the console hostname (ConsoleSettings is the record), tokens,
  `--config`, `--version`, `--k3s-version`, `--image*`, the join URL, role,
  labels and taints.

**`--progress FILE`** (`KWERFT_PROGRESS`): the directory must exist (else
exit 2). The file is truncated (0600) right after the arguments are parsed,
then gets one JSON object per line:

```
{"id":"preflight","label":"Preflight","state":"ok","detail":"Ubuntu 24.04 · x86_64 · …","at":"2026-10-05T10:00:00Z"}
{"id":"kubernetes","label":"Kubernetes","state":"skip","detail":"k3s v1.37.1+k3s1 · installed","at":"2026-10-05T10:00:41Z"}
{"id":"kwerft","label":"Kwerft","state":"fail","detail":"Kwerft installation failed (chart: oci://…)","at":"2026-10-05T10:09:12Z"}
{"exit":50}
```

- One line per stage that ran (`ok`), was skipped (`skip`; detail
  `already done` or the k3s note above) or failed (`fail`). `detail` is the
  stage's summary line as printed (for `status.steps[].detail`); for a
  failure, `die`'s message, or `exit <rc> at line <n>; see
  /var/log/kwerft/install.log` for an unexpected error. A failed stage gets
  exactly one `fail` line.
- `id` is the stage id above (the `/var/lib/kwerft/stages/<id>.done` name),
  `label` what the terminal shows, `at` UTC (`%FT%TZ`).
- The last line is always `{"exit":<code>}`, written by the EXIT trap of the
  top-level shell: also for usage errors (`{"exit":2}` alone, nothing ran)
  and `--dry-run` (`{"exit":0}`, no stage lines).
- Strings are JSON-escaped (`\\`, `\"`, `\t`, `\n`, `\r`); other control
  characters are dropped. Text is UTF-8 (`·`, `›`).

**`--k3s-version V`** (`KWERFT_K3S_VERSION`): must match
`^v[0-9]+\.[0-9]+\.[0-9]+(-rc[0-9]+)?\+k3s[0-9]+$`, else exit 2. Used by the
`kubernetes` stage (install and agent mode) and by `join`, on their first
run only: a running cluster never changes. The NodePool reconciler passes
the Cluster's `status.kubernetesVersion` when it is a k3s version
(`k3sVersion()` in `nodepool_cloudinit.go`, the same pattern; a bats test
keeps them equal) to joins and agent bootstraps (still empty for a new
cluster, so the pin applies). The installer cloud-init downloads is the
console's own release, so it knows the flag.

**Wording**: the header of `install.sh`, `--help`, the closing summary line,
`RELEASING.md` and the blueprint (install terminal, "One command", Stages)
say a re-run converges everything but k3s. The published blueprint artifact
was not republished from this branch: the coordinator does that after the
merge.

**For U3 (runner)**: run `install.sh --version <v> --yes --progress
<dir>/progress.jsonl`; everything else comes from `install.env`. Schedule the
runner with `nodeSelector: {kwerft.dev/installer: "true"}`. Treat a missing
`{"exit":…}` line after the unit ended as a crash and use the unit's exit
status. A release without W2 never wrote `install.env`, so the first
console upgrade starts from the first release that has it.

**For B1**: the main flow changed in three places (`remember_settings` after
`preflight`, `force` on `system` and `helm`, the `upgrades` stage after
`helm`) plus `trap 'progress_exit $?' EXIT` and `progress_start` at the top
of `main`. New stages need nothing for `--progress` (`run_stage`, `ok`,
`skip` and `die` report), and a new exit code needs nothing either. To
remember a new setting, add its key to `install_env`, `apply_install_env`
and `given` in `parse_args`.

**Open**: SUC's manifests are applied from GitHub without a checksum (like
the Gateway API CRDs). The console's Plans (W4) rely on the upstream
namespace and ServiceAccount names above.

## As built (U3): Upgrade core

Files: `internal/upgrades/` (versions, the install repository and
manifests, maintenance windows, progress lines, the anonymous registry
check, the database copy and restore, and the runner: `runner.go` state
machine, `host.go` systemd host, `kube.go` Kubernetes side),
`internal/controllers/upgrade_{controller,preflight,runner}.go` and
`updates_controller.go`, `cmd/kwerft/{upgrades,upgraderunner}.go`, chart
`templates/upgrade-runner.yaml` and `values.yaml` (`upgrades.installBaseURL`,
`e2e.faults`), a gate in `nodepool_controller.go`, constants in
`api/v1alpha1/annotations.go`. No type changes.

**Where it runs.** `UpgradeReconciler` runs in every cluster (console and
agent mode; registered in `newControllers`), `UpdatesReconciler` (discovery
and AutoPatch) in the console only. Controller flags: `--install-base-url`
(chart `upgrades.installBaseURL`; empty means
`https://raw.githubusercontent.com/ehilzinger/kwerft-install/main`, where U1
publishes) and `--upgrade-faults` (chart `e2e.faults`). The chart now sets
`POD_NAME` on the console: the controller reads its own pod to pin the
runner image (`status.containerStatuses[].imageID`, so `repo@sha256:…`; a
locally imported image has no registry digest, which only dev installs hit
and preflight refuses anyway).

**Database copy: in-process.** The controller and the console are one
process in one pod (`replicas: 1`, `Recreate`, leader election hands over on
shutdown), so the Upgrade controller calls `store.Store.Snapshot` directly
(`upgrades.Snapshotter`); there is no internal endpoint. The copy is
`<data-dir>/backups/pre-<upgrade>.db`, i.e. `/var/lib/kwerft/backups/…` in
the console pod (the design's `/data/backups`), made once per Upgrade
(resumable); the newest 3 are kept. Agent mode has no database and skips
it. Should the console ever run more than one replica, the copy must follow
the store behind the leader (the existing TODO in `deployment.yaml`).
For `rollbackSafe: false` with `acceptDataRollback`, the runner annotates the
Upgrade `kwerft.dev/restore-database: <old version>` before it rolls the
Kwerft release back; the console of exactly that version
(`upgrades.RestorePendingDatabase`, called in `main` before `store.Open`)
copies `status.backup.database` (only from `<data-dir>/backups/`) over
`kwerft.db`, removes `-wal`/`-shm` and sets the annotation to `done`.

**Lifecycle and who writes what.**

| Phase | Writer | What happens |
|---|---|---|
| `""` → `Queued` | controller | another Upgrade is active, an older one waits, or (auto-update) the window is closed; `status.message` says which |
| `Preflight` | controller | `startedAt`, `from` (running Kwerft, oldest kubelet), `preflight[]`; a blocking failure → `Failed`/`Preflight` |
| `Backup` | controller, then runner | controller: finalizer `kwerft.dev/upgrade`, database copy, runner Job. Runner: `install.env` present, ≥ 5 GiB in `/var/lib`, download + SHA256SUMS + manifest version, App baseline, `k3s etcd-snapshot save --name pre-<upgrade>` (`backup.etcdSnapshot`), `helm list -A` (`backup.helmRevisions`) |
| `Running` | runner | `systemd-run --unit kwerft-upgrade-<upgrade> … install.sh --version V --yes --progress …`; `steps[]` from the progress file; the log ConfigMap once per stage |
| `Verifying` | runner | up to 10 min; `message` shows what is still missing |
| `RollingBack` | runner | `helm rollback` of changed releases, Kwerft first, Cilium last; verify the old version |
| `Succeeded` `RolledBack` `Failed` `Cancelled` | runner or controller | `finishedAt`; the runner writes the log ConfigMap and removes the unit; the controller drops the finalizer, pauses AutoPatch, keeps the newest 20 |

`status.reason`: `Usage Preflight Network Kubernetes Platform Kwerft`
(installer exit codes), `Installer` (another exit code, or the unit vanished
without an exit line), `Verify`, `Timeout` (installer > 90 min, or the Job's
3 h deadline), `Backup` (database copy, etcd snapshot or `helm list` failed:
nothing changed), `Runner` (the Job gave up after 4 pod failures),
`Cancelled`. A runner Job deleted by hand mid-run is created again and
resumes from the status. The finalizer holds a Kwerft Upgrade from `Backup`
until it finished: deleting it then waits for the end.

**For U5 (API and UI).**
- Create: as the user (impersonation), `generateName:
  controllers.GenerateName(component, version)` (`kwerft-0.6.0-`,
  `kubernetes-v1.38.1-k3s1-`), annotation `kwerft.dev/requested-by: <email>`,
  `spec.{component,version,acceptDataRollback}`. Synchronous preflight:
  build a `controllers.UpgradeChecks` in `main` as `setupUpgrades` does
  (factor it out) and call `.Kwerft(ctx, spec, "")`; refuse when
  `controllers.Blocked(checks)` is non-empty, show warnings. The controller
  runs the same checks again.
- Cancel (`DELETE …/upgrades/{name}`): patch the annotation
  `kwerft.dev/cancel-requested: <email>` (keeps the record; the controller
  and the runner honour it until the installer starts, and the phase's
  optimistic lock decides a race). Owners need `patch` on `upgrades` for
  that; deleting the object also works but loses the record.
- Live status: watch the Upgrade (phase, `steps[]`, `message`). Log:
  ConfigMap `kwerft-system/<upgrade>-log`, key `install.log` (the 64 KiB tail
  of `/var/log/kwerft/install.log`), updated once per stage and at the end.
- Check now: annotate ConsoleSettings `local`
  `kwerft.dev/check-updates-requested: <RFC 3339 now>`. A check runs when it
  is newer than `status.updates.checkedAt`.
- Resume AutoPatch: annotate `kwerft.dev/resume-autopatch:
  <status.updates.autoPatchPausedBy>`. Policy, channel, window:
  `spec.updates`.
- Notifications: an auto-update that ends `RolledBack`/`Failed` gets the
  condition `AutoPatchPaused` and sets `status.updates.autoPatchPausedBy`;
  otherwise the phase changes are the events (watch `upgrades`).

**For U4 (Kubernetes, agents).**
- `UpgradeReconciler.Kubernetes` takes a `controllers.KubernetesUpgrades`
  (`Reconcile(ctx, *Upgrade) (ctrl.Result, error)`): it is called for the
  active Kubernetes Upgrade from `Preflight` on and changes `u.Status`; the
  controller writes it. Queueing, cancel before `Preflight`, the AutoPatch
  pause and retention stay here. Kubernetes Upgrades get no finalizer and no
  runner. Without a driver they fail at preflight.
- Every active Upgrade (either component) holds NodePool changes of its
  cluster: the NodePool reconciler lists Upgrades through the pool's cluster
  client and waits ("Waiting for upgrade X to finish.").
- Agent clusters run the same controller and runner (no database copy;
  verification reads the agent's image tag instead of `/api/v1/version`).
  `UpgradeChecks.Cluster` names the cluster for NodePools.
- AutoPatch creates Kubernetes patch Upgrades only with `kubernetesPatches`,
  after Kwerft patches, never minors.

**Runner contract** (`kwerft upgrade-runner`, `internal/upgrades/runner.go`).
- Job `<upgrade>-runner` in `kwerft-system`, controller-owned by the
  Upgrade, label `kwerft.dev/upgrade`, service account
  `kwerft-upgrade-runner`, node selector `kwerft.dev/installer=true`,
  tolerates everything, host network, `ClusterFirstWithHostNet`, root with
  only `CAP_SYS_CHROOT`, read-only root file system, the host's `/`
  read-only at `/host` (`HostToContainer`), `/var/lib/kwerft/upgrade` at
  `/work`. Backoff 4, deadline 3 h, TTL 7 days. It talks to the API at
  `https://127.0.0.1:6443` (`--api-server`), not through the Service that
  Cilium carries.
- Only `systemd-run` and `systemctl` run in the chroot; `k3s`, `helm`
  (`KUBECONFIG=/etc/rancher/k3s/k3s.yaml`, `HOME=/root`) and `install.sh`
  run as transient host units. The installer's unit
  (`kwerft-upgrade-<upgrade>`) has `RemainAfterExit=yes` and
  `KillMode=process`; its exit code is the progress file's `{"exit":N}`,
  else `ExecMainStatus` (U2: a missing exit line is a crash).
- Files: `/var/lib/kwerft/upgrade/<upgrade>/{install.sh,SHA256SUMS,
  manifest.json,progress.jsonl,apps-before.json,started}`.
- Verification: console Deployment ready and `GET http://<Service
  ClusterIP>/api/v1/version` = target (agents: the image tag); every
  `kwerft.dev` CRD established; node agent rolled out; `https://<console
  domain>/` answers with a valid certificate (the chain is not checked when
  the ClusterIssuer `letsencrypt` uses an ACME staging server); no App has
  fewer ready replicas than before; `kube-system/hubble-relay` ready when
  present.
- Rollback: releases whose revision changed, in reverse stage order;
  releases new in the target are left installed (named in the message);
  CRDs, host changes and SUC are not rolled back. A failed rollback ends
  `Failed` with the `helm rollback` commands to run by hand and the names of
  the snapshot and the database copy.
- Fault injection (E1): with chart `e2e.faults=true`, an Upgrade annotated
  `kwerft.dev/e2e-fault: install` (the installer counts as failed with exit
  50 after it succeeded) or `verify` (verification fails) passes it on as
  `--fault`; otherwise the annotation is ignored.

**Discovery as built.** `releases.json` is read as U1 writes it (a list; an
object with `releases` is accepted too). Up to 12 newer releases on the
channel (edge sees everything, stable only stable), each with its manifest;
a newer release without a manifest is skipped. Kubernetes targets come only
from the running release's manifest (`kubernetes.pinned`, newer and at most
one minor ahead); none while the running release has no manifest
(≤ 0.5.x). Notes from `NOTES.md`, at most 8 KiB each. A failed check keeps
the last `available` and sets `error`. Off clears `available` and makes no
request. A check also runs when the running versions differ from
`status.updates.current` (after an upgrade).

**AutoPatch as built.** Inside the window, with no unfinished Upgrade: the
newest allowed Kwerft patch of the running minor (no pre-releases), else
with `kubernetesPatches` an allowed k3s patch. One attempt per release and
window (a cancelled one is not retried in the same window). An auto-update
in `Queued` or `Preflight` waits for the next window once this one closed.
Any `Failed` or `RolledBack` auto-update pauses AutoPatch, preflight
failures included (nothing changed then, but an owner should look).

**Tests.** `internal/upgrades`: the runner state machine against a fake
host and cluster (success; installer exit 50 → rollback in order; failed
verification; both injected faults; failed rollback → `Failed` with manual
steps; bad checksum; disk; missing `install.env`; failed snapshot; cancel;
resume of a running installer; vanished unit; hung installer; database
restore request), discovery against a fake repository and an HTTP server,
windows, progress parsing, the registry check against a fake registry with
a Bearer challenge, `systemctl show` parsing, verification on a fake API,
the database copy (real SQLite) and restore. `internal/controllers`: the
preflight against a fake API (every blocking check), and envtest for the
queue, the runner Job, finalizer and deletion, cancel, a runner that gave
up, retention, AutoPatch windows, pause and resume, discovery with Check
now and the interval, Off, and the NodePool gate. Nothing ran on a host.

**Open.**
- Free disk on the installer node is only known to the runner (statfs); the
  controller's preflight sees `DiskPressure` only. A number in the dialog
  needs node-exporter or the kubelet's summary API.
- Cosign verification of `install.sh` (design follow-up).
- The first upgrade from a release without U2 (`install.env`, the installer
  label) is refused with a hint to re-run the installer once; e2e (E1)
  should start from a release with U2.
- Owners' `patch` on `upgrades` (cancel) belongs to U5's roles matrix.
