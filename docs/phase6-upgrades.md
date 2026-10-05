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
Superseded: only after it reached `Running` (As built (U4) › AutoPatch).

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

## As built (U4): Kubernetes upgrades and agent clusters

Files: `internal/controllers/k3s_upgrade.go` (the driver),
`k3s_plans.go` (Plans, per-node progress), `k3s_preflight.go`,
`agent_upgrades.go` (fleets of agent clusters), `internal/upgrades/k3s.go`
(SUC names, the deprecated-API metric, the API server's etcd check, the
restore hint, `ReachedRunning`), `internal/upgrades/agents.go` (version
gating, `PlanFleet`), the runner's snapshot mode (`runner.go`), small
changes to `upgrade_controller.go`, `cmd/kwerft/upgrades.go` and `main.go`,
new annotation constants in `api/v1alpha1/annotations.go`, chart RBAC, and
SUC's Plan CRD v0.20.2 vendored for envtest
(`internal/controllers/testdata/crds/plans.upgrade.cattle.io.yaml`). No
type changes.

### Kubernetes upgrade

`controllers.KubernetesUpgrader` is the `KubernetesUpgrades` plug-in, set up
in every cluster (console and agents) by `setupUpgrades`. The Upgrade
controller still handles the queue, a cancel or a closed window in
Preflight (shared `beforePreflight`), the AutoPatch pause and retention.

| Phase | What happens |
|---|---|
| `Preflight` | `UpgradeChecks.Kubernetes` (below). Passed: the Apps' ready replicas go into the log ConfigMap `<upgrade>-log` (key `apps-before.json`). |
| `Backup` | The runner Job (same Job as a Kwerft upgrade, `RunnerJobName`) takes `k3s etcd-snapshot save --name pre-<upgrade>` on the installer node, after checking 2 GiB free in `/var/lib`, records `status.backup.etcdSnapshot` and exits. A cancel deletes the Job (`Cancelled`). A Job that gave up fails with reason `Backup`, "Nothing was changed". |
| `Running` | The Plans are created (below); `status.nodes` follows them every 10 s; the log gets a line per node state change. |
| `Verifying` | Every 10 s for up to 10 min (`VerifyTimeout`). |
| `Succeeded` / `Failed` | The Plans are deleted. |

No finalizer and no `RollingBack`. Conditions `NodesUpgraded` and `Verified`
carry the start times (lastTransitionTime) of Running and Verifying.
Running times out after 40 min per node + 10 min (reason `Timeout`).

**The runner image** for the snapshot is the console's image pinned by
digest, else (`KubernetesRunnerImage`, a development install's imported
image) the image as the Deployment names it: k3s upgrades also work on
`make dev-server` installs.

**Plans** (namespace `system-upgrade`, service account `system-upgrade`,
label `kwerft.dev/upgrade`, owned by the Upgrade; `spec.version` = the
target, `upgrade.image: rancher/k3s-upgrade`, `concurrency: 1`,
`tolerations: [{operator: Exists}]`):

| Plan | Nodes | Node handling | Job deadline |
|---|---|---|---|
| `k3s-server` | `node-role.kubernetes.io/control-plane In [true]` | `cordon: true` | 15 min |
| `k3s-agent` | no control-plane label, `kubernetes.io/hostname NotIn` the single-node pools | `drain: {timeout: 10m, ignoreDaemonSets, deleteEmptydirData, force}` (evictions, so PDBs hold it; a drain that times out fails the Job); `prepare: [prepare, k3s-server]` waits for the servers | 30 min |
| `k3s-agent-cordon` | workers alone in their pool (`kwerft.dev/pool`; workers of no pool count as one pool) | `cordon: true`, same `prepare` | 15 min |

The third Plan exists only when some pool has a single worker; `k3s-agent`
only when there are other workers. Plans are created once and never
updated (SUC would apply a changed one again); a Plan of the same name left
by another Upgrade is deleted first. `force: true` deletes pods without a
controller; Kwerft runs none of its own.

**Progress** (`status.nodes`, control-plane nodes first, then by name),
from the newest SUC Job per node (labels `upgrade.cattle.io/plan` and
`/node`) and its pod's init containers:

| State | When |
|---|---|
| `Waiting` | no Job yet ("waiting for its turn", "after the control plane"), or the pod's `prepare` init container runs |
| `Draining` | the `drain` init container runs |
| `Upgrading` | `cordon` or the `upgrade` container runs, or the kubelet is on the target but not Ready yet, or the Job completed and the kubelet does not report the target yet |
| `Done` | the kubelet reports the target and is Ready, no Job running |
| `Failed` | the node's Job failed (SUC's `Complete=False, reason JobFailed` on a Plan counts too, once the Job's TTL removed it) |

`status.message` says "N of M done" and what the current node does.

**Preflight** (`UpgradeChecks.Kubernetes(ctx, spec, self)`; U5 calls it
synchronously as for Kwerft). New field `UpgradeChecks.APIServer`
(`upgrades.APIServer` over the discovery REST client).

| Check | Blocks when |
|---|---|
| `Target` | not a k3s version (`v1.38.1+k3s1`); not newer than every node (downgrade, or already there); more than one minor ahead of the oldest node |
| `NodesReady` | a node is not Ready |
| `NodesSameVersion` | the kubelets differ (an unfinished upgrade) |
| `Etcd` | `/readyz/etcd` fails; etcd members (`node-role.kubernetes.io/etcd`, else control-plane) not Ready, quorum lost or not. Two members: passes with a warning (quorum is lost while either restarts) |
| `DeprecatedAPIs` | `apiserver_requested_deprecated_apis` (value 1) with `removed_release` ≤ the target's minor: blocks a minor, warns on a patch. An unreadable metric blocks a minor, warns on a patch. The metric covers requests since that API server started (in HA: the one that answered) |
| `DiskSpace` | a node has `DiskPressure` (the runner checks 2 GiB on the installer node) |
| `NoOtherOperation` | as for Kwerft |
| `KwerftSupports` | the running release's manifest does not list the target's minor in `kubernetes.supported`, or cannot be read ("upgrade Kwerft first"); a development build only warns |
| `UpgradeController` | Plan CRD missing or not established, `system-upgrade/system-upgrade-controller` missing or not ready ("re-run the installer") |
| `InstallerNode` | as for Kwerft (the snapshot is taken there) |

**Verification:** every node on the target, Ready and not cordoned;
DaemonSet `kube-system/cilium`; Deployment `kube-system/coredns`; Traefik in
`traefik` (DaemonSet, or a Deployment); CRDs `gatewayclasses`, `gateways`,
`httproutes.gateway.networking.k8s.io` established; HTTPRoute
`kwerft-system/kwerft-console-https` Accepted by every parent (skipped where
it does not exist: agents, consoles without a domain); no App with fewer
ready replicas than in the baseline.

**Failure** (a node's Job failed, timeout, verification): the Plans are
deleted (SUC deletes their Jobs and starts no further node), the node is
marked `Failed`, and `Failed` gets reason `Kubernetes`, `Timeout` or
`Verify` with a message that names the node, the nodes on the target and
those still on the old version, the snapshot, and the short restore
procedure (`upgrades.RestoreHint`), which links here.

**After a restore** the cluster has the Upgrade in `Backup` again (the
snapshot was taken there) without the snapshot recorded. The runner writes
`/var/lib/kwerft/upgrade/<upgrade>/snapshot-recorded` on the installer
node after recording the snapshot; finding it with no snapshot in the
status, it fails the Upgrade ("restored from this upgrade's etcd snapshot:
the upgrade is not repeated") instead of upgrading again.

### Restoring after a failed Kubernetes upgrade

Kwerft never does this. k3s cannot be downgraded, and restoring etcd resets
every Kubernetes object (Kwerft's included) to the moment of the snapshot;
data in volumes is not in etcd and stays as it is. Usually it is better to
fix the cause (the failed node's Job: `kubectl -n system-upgrade logs
job/<job>`) and upgrade again: kubelets one minor behind work within the
version skew policy. To go back:

1. On the installer node (`kwerft.dev/installer=true`), find the file:
   `k3s etcd-snapshot ls` lists `pre-<upgrade>-<node>-<timestamp>` under
   `/var/lib/rancher/k3s/server/db/snapshots/` (with S3 snapshots
   configured, a copy is in the backup bucket too).
2. Stop k3s everywhere: `systemctl stop k3s` on servers, `systemctl stop
   k3s-agent` on workers.
3. On every node that was upgraded, put the old binary back, e.g. for
   `v1.37.1+k3s1` on amd64: `curl -fLo /usr/local/bin/k3s
   https://github.com/k3s-io/k3s/releases/download/v1.37.1%2Bk3s1/k3s &&
   chmod 755 /usr/local/bin/k3s` (`k3s-arm64` on ARM).
4. On the installer node: `k3s server --cluster-reset
   --cluster-reset-restore-path=<file>`; when it says to restart without
   `--cluster-reset`, `systemctl start k3s`.
5. On every other server: `rm -rf /var/lib/rancher/k3s/server/db`, then
   `systemctl start k3s` (they join the restored member).
6. On the workers: `systemctl start k3s-agent`.
7. Check `kubectl get nodes`. The Upgrade ends `Failed` (above); start a
   new one when the cause is fixed.

### Agent clusters (W6)

Console N works with agents of the same minor and the one before
(`upgrades.AgentCompatible`). U3's `AgentSkew` check refuses a console
upgrade that would leave a connected agent two minors behind; an agent is
never upgraded past the console (`upgrades.AgentTargetAllowed(console,
agent, target)`).

**"Upgrade all" (fleets).** Every remote Upgrade is created by the API as
the owner (impersonated through the tunnel: `clusterConn.kube.For(email,
role)` of the agent's cluster); the console only releases or cancels them,
under its own identity there:

- The members: `controllers.FleetMember(fleet, after, order, version,
  email)` returns the Upgrade to create in an agent cluster
  (`generateName` as `GenerateName`, label `kwerft.dev/fleet: <fleet>`,
  annotations `kwerft.dev/hold: <message>`, `kwerft.dev/fleet-order`,
  `kwerft.dev/after-upgrade`, `kwerft.dev/requested-by`).
- The agent's Upgrade controller keeps a held Upgrade `Queued` with the
  hold as its message, and a held one does not hold the cluster's other
  Upgrades in line.
- `controllers.AgentUpgradesReconciler` (console only, `main.go`) lists the
  Upgrades with `kwerft.dev/fleet` in every Connected cluster, on any
  change of the console's Upgrades or Clusters and every 30 s while a fleet
  is unfinished. Per fleet: if `after-upgrade` names the console's Upgrade
  and it is not finished, wait; if it ended other than `Succeeded` or is
  gone, annotate the held members `kwerft.dev/cancel-requested: kwerft
  (the console's upgrade X ended RolledBack)`. Otherwise, while no
  released member is unfinished, release the next by order (remove the
  hold) after `AgentTargetAllowed` (else cancel it with the reason), and
  set the others' hold to "Waiting for its turn: <cluster> upgrades
  first.". A failed or rolled-back member does not stop the fleet.
- Disconnected clusters are not seen: their members wait and go in turn
  when the cluster is back. The console's Upgrade gets condition
  `AgentClusters` ("Agent clusters (edge-1: Running, edge-2: waiting).",
  True when all are finished).

**For U5.**
- *Upgrade all to V*: preflight the console's upgrade
  (`UpgradeChecks.Kwerft`), create it as the owner (name L), then
  `upgrades.PlanFleet(V, agents)` with `AgentCluster{Name, Connected:
  phase Connected, AgentVersion}` from the Cluster objects; for each queued
  agent, in order, create `FleetMember(L, L, i, V, email)` in its cluster
  as the owner; show `skipped` (warnings: disconnected or unknown version;
  others: already on V). A create that fails there: skip and warn.
- *The console already runs V* (only agents behind): fleet
  `upgrades.NewFleetID()`, `after` "": the members start one by one at
  once.
- *One agent cluster*: refuse unless `AgentTargetAllowed(console, agent,
  V)`; create a plain Upgrade there as the owner (no fleet). The
  synchronous preflight can run `UpgradeChecks{Reader: <the cluster's
  system client>, Releases, Registry, Version: <agent version>, Cluster:
  <name>}.Kwerft(…)` (no `Self`, no `DataDir`: those checks are skipped).
- *Kubernetes on an agent*: an Upgrade in that cluster as the owner, never
  queued across clusters. Its preflight: `UpgradeChecks{…, APIServer:
  &upgrades.APIServer{REST: <discovery client of that cluster's
  RESTConfig>.RESTClient()}}.Kubernetes(…)` (the agent's service account
  has the same `/metrics` and `/readyz/etcd` rights).
- Progress of a fleet: list `kwerft.dev/fleet=<L or id>` in each
  cluster as the user, or read L's `AgentClusters` condition. A Kubernetes
  Upgrade's `status.nodes` is the per-node view; its log ConfigMap
  (`install.log`) has a line per node state change.

### AutoPatch (coordinator decision, in U3's controller)

AutoPatch pauses only when an auto-update ended `RolledBack` or `Failed`
after it reached `Running` (`upgrades.ReachedRunning`: a Kwerft upgrade
records its Helm revisions in the same write that moves it to Running, and
its installer steps after; a Kubernetes one its nodes). A preflight or
backup failure (nothing changed) leaves AutoPatch on; it tries again in the
next window. This replaces "preflight failures included" in U3's notes.

### RBAC

`kwerft-controller` (console and agents) gains: `upgrade.cattle.io` plans
(all verbs), `apps` daemonsets (read), `apiextensions.k8s.io`
customresourcedefinitions (read), and `nonResourceURLs: [/metrics,
/readyz/etcd]` (get). SUC's Jobs and pods are read with the existing core
and batch rules. The runner's rights are unchanged.

### Tests

`internal/upgrades`: the deprecated-API metric parser and `RemovedBy`, k3s
versions, the restore hint, `ReachedRunning`, `AgentTargetAllowed`,
`PlanFleet`, the runner's snapshot mode (taken once, retried pod, failed
snapshot, disk, cancel, other phases, a restored cluster).
`internal/controllers`: the Kubernetes preflight against a fake API (every
check), envtest with SUC's Plan CRD for the whole flow (Plans validated by
the CRD, per-node progress through Draining to Done, verification,
success), a failed node (Plans deleted, message), a verification timeout,
a cancel in Backup, a blocked preflight; the held Upgrade in the queue;
AutoPatch not paused by a failed preflight; the fleet against fake
clusters (waits for the console, one at a time, a rolled-back member does
not stop it, cancelled when the console's upgrade failed or is gone,
disconnected clusters skipped, never past the console). Nothing ran on a
host or against SUC itself.

### Open

- SUC's real behaviour (the `prepare k3s-server` wait, drain timeouts,
  Job names, init container names) is taken from its v0.20.2 source, not
  run; E1's 3-node run is the first real test.
- The deprecated-API metric counts requests since the API server started:
  a server restarted shortly before the check knows little. A minor
  upgrade blocked by it needs the clients changed, not a retry.
- Upgrading Kubernetes on the console's own cluster restarts the API
  server under the console; the controller resumes from the status. With
  one server the API is gone for the restart (as for any k3s upgrade).
- The fleet's hold is an annotation the owner could remove by hand in an
  agent cluster; that only starts that member early.

## As built (U5): API and UI

Files: `internal/server/api_upgrades.go` (+ `upgrades_test.go`),
`server.Config.Upgrades` (`server.UpgradePreflight`), the wiring in
`cmd/kwerft/upgrades.go` (`newUpgradeChecks`, `upgradePreflight`; U3's
`setupUpgrades` now returns its `*controllers.UpgradeChecks`), roles
(`internal/access`, `charts/kwerft/templates/roles.yaml`), the alert
`UpgradeFailed` (`api/v1alpha1/alerting_types.go`, `internal/alerting`,
`internal/controllers/metrics.go`), and the UI: `web/src/updates.ts`
(+ `updates.test.ts`), `web/src/pages/SettingsUpdates.tsx`,
`web/src/styles/updates.css`, the route `/settings/updates`, Settings tabs
(General | Updates), the Settings dot in the sidebar and the Overview
notification.

**Roles.** Two rows in the matrix: `upgrades` (start and cancel upgrades,
change the policy: owners) and `updates` (read Settings › Updates and the
history: owners and admins). `kwerft:owner` keeps `kwerft.dev` `*`.
`kwerft:admin` no longer has `kwerft.dev` `*`: RBAC cannot subtract, so it
lists every kind with its `status` (and `apps/scale`) with all verbs, plus
`upgrades` get/list/watch. **A new CRD must be added to that list**;
`TestAdminsReachEveryKindButUpgrades` (internal/access) fails until it is.
The policy lives in ConsoleSettings, which admins may patch through
kubectl; the console API refuses them (owners only), RBAC cannot.

**Endpoints.** All JSON; every request as the signed-in user (impersonation)
in the cluster named by `?cluster=` (default `local`; for `POST /upgrades`
the body's `cluster`). Writes need same-origin (or a bearer token).

| Endpoint | Roles | Answer |
|---|---|---|
| `GET /api/v1/updates` | owner, admin | `updates` (below) |
| `POST /api/v1/updates/check` | owner, admin | 202 `{requestedAt}`; annotates ConsoleSettings `kwerft.dev/check-updates-requested`; 409 while the policy is Off |
| `POST /api/v1/updates/resume-autopatch` | owner | 202 `{resumed: <upgrade>}`; annotates `kwerft.dev/resume-autopatch: <autoPatchPausedBy>`; 409 when not paused; audited `updates.autopatch_resumed` |
| `PUT /api/v1/settings/updates` | owner | body and answer `policy` (below); merge-patches `spec.updates`; audited `updates.policy` with "before → after" |
| `GET /api/v1/upgrades[?cluster=]` | owner, admin | `[upgrade]`, newest first; without `cluster` every connected cluster (unreachable ones in `Kwerft-Unreachable-Clusters`) |
| `POST /api/v1/upgrades/preflight` | owner | `preflight` (below); the dialog's live checks; nothing is created |
| `POST /api/v1/upgrades` | owner | 201 `{upgrade, preflight}`; audited `upgrade.start` |
| `GET /api/v1/upgrades/{name}[?cluster=]` | owner, admin | `upgrade` |
| `GET /api/v1/upgrades/{name}/events[?cluster=]` | owner, admin | SSE (below) |
| `GET /api/v1/upgrades/{name}/log[?cluster=]` | owner, admin | `{log, truncated}` |
| `DELETE /api/v1/upgrades/{name}[?cluster=]` | owner | 202 `upgrade`; audited `upgrade.cancel` |

`POST /api/v1/upgrades` body:
`{cluster?, component: "Kwerft"|"Kubernetes", version, acceptDataRollback?, password, confirmVersion?}`.
In this order: component and version are checked (400 `field` `component`
/ `version`; a k3s version needs the `v`), the cluster resolved (404
unknown, 503 unreachable, 409 when upgrades of it cannot be started from
the console), for a Kubernetes **minor** (or an unknown running version)
`confirmVersion` must equal `version` (400 field `confirmVersion`), then
the password or a current authenticator code (`confirmIdentity`: 400 field
`password`, rate limited, a wrong one audited `account.confirm_failed`;
single-sign-on accounts: a sign-in within 10 minutes). One unfinished
Upgrade per component and cluster (409 `{error, upgrade}`). Then the
synchronous preflight (60 per user per 15 min, 429 beyond): blocking
checks answer 409 `{error, preflight}` and create nothing; warnings pass.
Last, the Upgrade is created as the user: `generateName:
controllers.GenerateName(component, version)`, annotation
`kwerft.dev/requested-by: <email>`, `spec {component, version,
acceptDataRollback}`. The audit detail is `Kwerft 0.5.0 → 0.6.0 on local`
(plus `; data rollback accepted`).

`DELETE` patches `kwerft.dev/cancel-requested: <email>` with the object's
resourceVersion (409 on a race), only while the phase is `""`, `Queued`,
`Preflight` or `Backup`; otherwise 409 ("can no longer be cancelled, and
rolls back by itself if it fails"). Asking twice answers 202 without a
second patch.

Shapes (JSON field names):

```
upgrade   {name, cluster, component, version, requestedBy?, auto, acceptDataRollback?,
           phase: Pending|Queued|Preflight|Backup|Running|Verifying|RollingBack|Succeeded|RolledBack|Failed|Cancelled,
           from?: {kwerft?, kubernetes?}, preflight: [check], steps: [{id, label, state, detail?, at?}],
           nodes: [{name, version?, state, message?}], backup?: {etcdSnapshot?, database?, helmRevisions?},
           reason?, message?, cancelRequestedBy?, cancellable, finished, createdAt, startedAt?, finishedAt?}
check     {check, ok, warning?, message?}
preflight {cluster, component, version, from?, kind: Patch|Minor, checks: [check], blocked,
           confirmVersion (type the version again), dataRollback (not rollback-safe: needs acceptDataRollback)}
policy    {policy: Off|Notify|AutoPatch, channel: stable|edge, kubernetesPatches,
           window?: {days: [Mon…Sun] (empty: every day), start: "HH:MM", duration: "2h"|"1h30m", timeZone?}}
updates   {policy, checkedAt?, checking, error?, autoPatchPausedBy?, nextWindow?, windowOpen,
           current: {kwerft, kubernetes}, available: [available], clusters: [cluster], canUpgrade}
available {component, version, kind, notes? (Markdown without "## Install"), allowed, reason?}
cluster   {name, connected, kwerft?, kubernetes?, available: [available], active?: upgrade, upgradable, message?}
```

`phase` `Pending` is an Upgrade the controller has not picked up yet
(empty status). Policy validation: policy and channel as listed; AutoPatch
needs a window; days are Mon…Sun in any case (all seven are stored as
none); start `HH:MM`; duration 30m–24h in whole minutes (default 2h); the
time zone an IANA name (empty: UTC). `available` of a remote cluster is
the console's own release when the agent is older; Kubernetes targets of
remote clusters are U4's.

**SSE** (`…/events`): `event: upgrade` with the whole `upgrade` at once
and whenever its resourceVersion changes (polled every 2 s as the user),
`event: end` `{phase}` once it finished (or `{reason: "session"}` when the
session ended), `event: gone` when it was deleted, `: ping` every 15 s. The
stream ends after an hour; at most 8 per user. While the console restarts
it breaks: the page reconnects with backoff and shows "Reconnecting…", and
after a Kwerft upgrade of `local` it compares `GET /api/v1/version` with
the version it was loaded from and offers a reload.

**Log**: ConfigMap `kwerft-system/<upgrade>-log` key `install.log`, read
with the console's own identity (`clusterConn.systemReader`) after the
user's read of the Upgrade succeeded: no console role reads
`kwerft-system`. `{log: "", truncated: false}` before the runner wrote it;
`truncated` when it is the full 64 KiB tail.

**Release notes**: `server.ReleaseNotes` drops the `## Install` section of
NOTES.md (up to the next `## ` heading) before the notes reach the browser;
the UI renders a small Markdown subset as text (headings, paragraphs,
lists, code; links show their text).

**Preflight wiring** (`server.UpgradePreflight`): `Supports(cluster)` and
`Preflight(ctx, cluster, spec)`. `cmd/kwerft` implements it with the
console's `controllers.UpgradeChecks.Kwerft(ctx, spec, "")` for `local`.
**For the coordinator (U4):** `upgradePreflight.kubernetes` is nil, so a
Kubernetes preflight answers one blocking check ("Kubernetes upgrades from
the console are not available in this release."); wire U4's k3s preflight
there. `Supports` is `local` only; remote clusters need U4's path (an
Upgrade created in that cluster as the user through the tunnel, preflight
with that cluster's reader and the agent's version). The API side already
works per cluster: `cluster` in the body, `?cluster=` everywhere else,
`clusters[].upgradable` from `Supports`, and the UI shows "Re-run the
installer of X on its server" where it is false. "Upgrade all" has no
endpoint yet; it fits as a server-side queue (one Upgrade per cluster
after the console's) that U4 adds, with the UI button gated on a new
`updates` field.

**Platform-event notifications.** As B2 did for backups: an alert
condition on Kwerft's own metric, a default rule, notification channels
added to that rule. `UpgradeFailed` (enum value added to
`AlertCondition`, kind `upgrade`, platform alert, no scope, severity
critical, window default 1 day, link `/settings/updates`), default rule
`upgrade-failed`. The metric
`kwerft_upgrade_failed_timestamp_seconds{component,version,upgrade,result}`
(`MetricsCollector`, every cluster) is `finishedAt` of the newest finished
Upgrade of each component when it ended `Failed` or `RolledBack`;
cancelled ones are passed over, and a later success removes the series.
Expression: `time() - max by (component, version, upgrade, result)
(kwerft_upgrade_failed_timestamp_seconds) < <window>`: it fires once per
failed Upgrade and resolves after the window or the next success. No new
event type: channels "subscribe" by being on the rule, as for every
platform alert (nodes, certificates, backups).

**UI.** Settings › Updates (owners and admins; the tab and the dot are
hidden from others): Versions (cluster × component, running, available,
Upgrade… / the active upgrade's phase, Check now with the last check),
the progress card (installer-style: ✓, spinner, ✗, – per console check,
backup, installer stage or node, verification, rollback; the closing line;
Cancel while cancellable; the installer log), Release notes (Kwerft
targets), Update policy (Off/Notify/AutoPatch, channel, Kubernetes patches,
window with days, start, duration, time zone defaulting to the browser's),
History (newest first, click to view). The Upgrade… dialog runs the
preflight on open and again when "Accept a data rollback" changes, picks
among the allowed versions, asks for the typed version on a k3s minor and
for the password. A paused AutoPatch shows a banner with Resume (owners).
The Overview's "Needs attention" lists "Kwerft X available" and
"Kubernetes vX available" (info, not counted in the banner) linking to
`/settings/updates`; the sidebar's Settings and the Updates tab get a dot.
New token `--term-bad` (`tokens.css` and the blueprint's `:root`) for ✗
on the terminal background.

**Tests.** `internal/server/upgrades_test.go` against envtest with the
chart's RBAC: roles (developers and viewers refused everywhere, admins
read and check but cannot start, preflight, set, resume or cancel, and
Kubernetes refuses admins create/patch/delete on upgrades), overview (notes
stripped, Check now, resume, Off), policy validation and audit, start
(validation never reaches the preflight, wrong password audited, unknown
cluster, k3s patch vs minor confirmation, blocked preflight creates
nothing, accepted data rollback, the created object and its audit,
duplicates, history and detail), cancel (Backup → annotation and audit,
Running and Succeeded → 409), SSE (first state, changes, end, a finished
Upgrade's stream) and the log; `TestReleaseNotes`, `TestNormalizeWindow`.
`internal/access`: the admin list against every CRD. `internal/alerting`
and `internal/controllers`: the expression, default rule and metric.
`web/src/updates.test.ts`: progress lines (queued, checks, backup, stages,
verify, rollback, failures, nodes), summary, phases, typed confirmation,
notices, window text, release notes, SSE parsing. The page was checked in
the browser (light, dark, phone width) against the homepage's mock API
wrapped with a simulated upgrade (dialog, wrong password, progress, the
console's restart with "Reconnecting…", the reload offer, policy save,
history, Overview notification and dots).

**Open.**
- Enabling AutoPatch does not ask for the password (audited only); it
  authorizes unattended patch upgrades.
- Admins can still change `spec.updates` with kubectl through the console's
  proxy (RBAC cannot see the field); the console's own API refuses them.
- The dialog does not repeat the release notes; they are in the page's
  Release notes card.

## As built (E1): e2e runs for upgrades and restore

W7 plus the backup exit criterion (`docs/phase6.md` › Full restore). Files:
`hack/e2e/` — `modes.go` (the four runs), `console_api.go` (upgrades and
their event stream, nodes and join commands, backups, volumes, secret
sets), `prober.go` (App availability), `s3.go` (the bucket prefix), networks
in `hcloud.go` and `sweep.go`, servers as a list in `run.go` (several per
run, created and deleted mid-run), the suite moved to `suite.go`;
`.github/workflows/e2e.yml`; `RELEASING.md` › e2e install runs. Nothing in
the product changed.

### The runs

| Flag | What it does | Servers | Takes | Costs (cx33, €0.0136/h) |
|---|---|---|---|---|
| `-from N-1 -via-console` | install N-1, owner, App `hello`; `POST /api/v1/upgrades` (Kwerft N, the owner's password), follow `/events` to `Succeeded`; then the console reports N, the owner signs in, `hello` answers and runs; then the usual checks | 1 | 35–45 min | 1 server-hour, ≈ €0.014 |
| `-from N-1 -fault` | as above with `e2e.faults`, the Upgrade annotated `kwerft.dev/e2e-fault: install`: it must end `RolledBack`, the console reports N-1 again, `hello` answers from the same pods with the same restart counts. No suite afterwards | 1 | 25–35 min | 1 server-hour |
| `-k3s [-k3s-from V] [-k3s-to V] [-workers 2]` | a Cloud Network and 3 servers; install N with `KWERFT_K3S_VERSION` one patch behind the release's `K3S_VERSION`; two workers join with join tokens from `POST /api/v1/clusters/local/join-command` and `install.sh --join`; App `hello` with 2 replicas; `POST /api/v1/upgrades` (Kubernetes, the pin), per-node progress from the events; all nodes on the target, Ready, schedulable; then the usual checks | 3 | 50–75 min | 3–6 server-hours, ≈ €0.04–0.08 |
| `-restore` | install N, owner; Settings › Backups with the run's bucket, prefix `<run id>`; data: Volume `data` with a marker file a Task wrote (served by App `files`), SecretSet `e2e` with a random value whose SHA-256 App `secret` answers; Back up now on plan `cluster`, `Completed`; delete the server; a new one; `install.sh --config kwerft.yaml --restore latest`; checks | 2, one after the other | 45–60 min | 2 server-hours, ≈ €0.03, plus a few MB in the bucket for an hour |

During both console upgrades the harness requests `https://hello.<ip>.sslip.io/`
every 2 s (`-probe-every`) and reports the longest time without an answer
as its own result, "App availability during the upgrade" (`-max-gap D`
fails the run beyond D; by default it only reports, and a gap over 30 s
gets a note).

**Following an upgrade.** `POST /api/v1/upgrades` is retried for 5 min on
5xx/429/network errors (a 409 naming the same version's Upgrade is taken as
the earlier attempt's); other 4xx fail with the console's message (a
blocked preflight shows its checks' messages). The event stream is read
with no client timeout; a broken stream (the console restarts) is opened
again after `-poll`, a `{reason: session}` end signs in again, `gone`
fails. The result shows the phases seen (`Pending → Preflight → Backup →
Running → Verifying → Succeeded`), the installer stages done, how often the
stream reconnected; a run that ends elsewhere than expected puts the tail
of `GET …/log` into the summary.

**Fault injection.** `e2e.faults` exists only as a chart value, so the run
sets it on the installed N-1 with `helm upgrade kwerft
oci://ghcr.io/ehilzinger/charts/kwerft --version N-1 --reuse-values --set
e2e.faults=true --wait` (`-chart` overrides the chart) and checks that the
console Deployment then carries `--upgrade-faults`; N-1's chart must know
the value (U3). The installer of N resets it; the runner of N-1 has the
flag by then. The annotation must be on the Upgrade before the controller
creates the runner Job, but the API creates the Upgrade without it, so the
run annotates it right after the POST over SSH and then makes sure the
Job's args carry `--fault=install`: a Job created before the annotation is
deleted (`--cascade=foreground`) and the controller creates it again
("created again and resumes from the status", U3). The same `helm
--reuse-values` step sets `upgrades.installBaseURL` when the run's
installers come from another install repository than the console's
default (a fork's `INSTALL_REPO`).

**Kubernetes run.** Joining needs a private network (`install.sh`
stage_join's TODO), so the run creates a Cloud Network (10.0.0.0/16,
subnet 10.0.0.0/24 in the zone of the first location; only locations of
that zone are tried) and attaches every server at creation; workers prefer
the control plane's location. Workers fetch their join material from the
console over HTTPS with curl, which does not trust the Let's Encrypt
staging certificate, so the run installs Let's Encrypt's staging roots
(`letsencrypt.org/certs/staging/letsencrypt-stg-root-x{1,2}.pem`,
`-staging-roots`) into each worker's trust store first. Every installer run
(server and workers) gets `KWERFT_K3S_VERSION`; the nodes must report it
before the upgrade (an installer that ignored it fails there). The default
versions come from the installer of N on the server (`K3S_VERSION`): the
target is the pin, the start its previous patch (`v1.37.1+k3s1` →
`v1.37.0+k3s1`; a pin with patch 0 needs `-k3s-from`). For a **minor**
upgrade give both (`-k3s-from v1.36.x+k3s1 -k3s-to v1.37.y+k3s1`): the
request carries `confirmVersion` always. Per-node assertions: every node
in `status.nodes` reaches `Done`, the control plane is the first to leave
`Waiting`; the states each node went through are in the result. Two nodes
seen draining or upgrading at once is a note, not a failure (a finished
node's kubelet may report the target a poll later).

**Restore run.** Before anything is written the prefix `<run id>/` must be
empty (otherwise the run fails and leaves it alone); from then on the
cleanup deletes every object under it, also after a failure, timeout or
cancel, retrying failed deletions (SSE-C objects need no key to be listed
or deleted). The recovery key from the `PUT /api/v1/settings/backups`
answer is masked in the log and kept in memory only. The new server gets
`/root/kwerft-e2e/{kwerft.yaml,s3.access,s3.secret,recovery.key}` (0600,
written through SSH's stdin, never in a command line); `kwerft.yaml` holds
only the `backups` block (endpoint, region if set, bucket, prefix and the
three files). The installer runs with `--version N --acme-server staging
--yes --config … --restore latest`, without `--domain`: the console keeps
the old server's `<old ip>.sslip.io` names, which still point at the
deleted server, so the checks afterwards connect to the new address with
the old names (as DNS would once moved; TLS checks the restored
certificates). Checks: version N, setup complete, the owner signs in with
the same password; Apps `files` and `secret` run; the marker file is back;
the set still has `E2E_SECRET` and `secret` answers the SHA-256 of the
value from before (neither value nor hash is printed); "Domain and DNS"
reports the names kept, where they point and the installer's DNS lines,
with a note for consoles on their own domain.

### Running them by hand

On GitHub: Actions → e2e → Run workflow (`workflow_dispatch` inputs
`version`, `fresh`, `upgrade`, `console_upgrade` auto|yes|no, `fault`,
`k3s`, `k3s_from`, `k3s_to`, `restore`), each run a job of its own:

```bash
gh workflow run e2e.yml -R ehilzinger/kwerft -f version=0.7.0 \
  -f fresh=false -f upgrade=false -f console_upgrade=yes -f fault=true
gh workflow run e2e.yml -R ehilzinger/kwerft -f version=0.7.0 -f fresh=false -f upgrade=false -f console_upgrade=no -f k3s=true
gh workflow run e2e.yml -R ehilzinger/kwerft -f version=0.7.0 -f fresh=false -f upgrade=false -f console_upgrade=no -f restore=true
```

Locally (real servers, deleted at the end; Ctrl-C deletes them too;
`-dry-run` prints the plan without a token):

```bash
HCLOUD_TOKEN=… go run ./hack/e2e run -version 0.7.0 -from 0.6.0 -via-console
HCLOUD_TOKEN=… go run ./hack/e2e run -version 0.7.0 -from 0.6.0 -fault
HCLOUD_TOKEN=… go run ./hack/e2e run -version 0.7.0 -k3s [-k3s-from v1.37.0+k3s1 -k3s-to v1.37.1+k3s1]
HCLOUD_TOKEN=… E2E_S3_ENDPOINT=https://fsn1.your-objectstorage.com E2E_S3_BUCKET=kwerft-e2e \
  E2E_S3_ACCESS_KEY=… E2E_S3_SECRET_KEY=… go run ./hack/e2e run -version 0.7.0 -restore
HCLOUD_TOKEN=… E2E_S3_…=… go run ./hack/e2e sweep -run <run id> -s3   # what a killed run left
```

**Needs:** `HCLOUD_TOKEN` (as before); the project's server limit must allow
3 servers for `-k3s`. For `-restore`: `E2E_S3_ACCESS_KEY` and
`E2E_S3_SECRET_KEY` (secrets), `E2E_S3_ENDPOINT` and `E2E_S3_BUCKET`
(secrets or variables), `E2E_S3_REGION` (optional; Hetzner endpoints imply
it): one bucket for all runs (RELEASING.md › One-time setup, step 5). The
console-upgrade runs need N-1 to be a release with U2 (`install.env`) and
U3/U5 (runner, API); the first possible pair is the Phase 6 release and the
one after it.

**Release and nightly runs** keep the fresh install and the installer
upgrade and add `-via-console` automatically once N-1's published installer
contains `INSTALL_ENV_FILE=` (the `resolve` job checks; a notice
otherwise). The forced failure, `-k3s` and `-restore` run only by hand.
The job's last step deletes what a killed harness left: servers, networks
and keys of the run, and with the bucket configured the prefix
(`sweep -run <id> -s3`).

### Tests

All against fakes (`go test ./hack/e2e`, also with `-race`): the fake
console (`console_fake_test.go`) now plays upgrades through their phases
(one step per event; the stream breaks once when "the console restarts";
`RollingBack → RolledBack` when the fault annotation reached it, or on
`failUpgrade`), a blocked preflight, nodes, join tokens, per-node k3s
progress (the control plane's restart makes `hello` fail twice), backup
settings with a recovery key, a backup that writes Velero-like objects into
the fake bucket, volumes written by Tasks, secret sets and Apps that serve
a file or a hash. The fake servers (`run_test.go`) run the installer's
modes (`--join` only after the staging roots were trusted and with a token
the console issued; `--restore` only with the uploaded config, keys and
recovery key matching the backup and objects under the prefix, never with
`--domain`), the helm values script (or refuse it like an old chart), the
fault script. The fake cloud has networks (deletion refused while a server
is attached), one address per server and an `onDelete` hook (the console
goes down with its server). The fake bucket (`s3_fake_test.go`) checks
every request's SigV4 signature, pages lists and can fail deletions.
Covered: each run passing; the fault without the chart value; a rollback
that touched the App; an unexpected rollback (log in the summary); a
blocked preflight; a gap over `-max-gap`; a failed join (everything still
deleted, network included); a failed restore (prefix still emptied); a
prefix that was not empty (left alone); event-stream parsing; the prober's
gaps; `previousPatch`; the scripts; host pinning; the bucket's paging,
retries and refusal to delete everything; the sweeper's networks and
bucket; config validation and every mode's `-dry-run` plan;
`TestProductNames` keeps the harness's copies of `kwerft.dev/e2e-fault`,
`RunnerJobName` and the default install base equal to the product's. The
workflow's `resolve` step was run with stubbed `gh`, `go` and `curl` for
nightly (with and without U2 in N-1), the by-hand runs, a missing bucket
and an empty selection, and its scripts pass shellcheck. Nothing ran
against Hetzner, a real console or a real bucket.

### Open

- **Never run for real.** First real runs, in this order: `-via-console`
  and `-fault` once a release after the Phase 6 release exists (or by hand
  between two Phase 6 release candidates), then `-k3s`, then `-restore`.
  Things only a real run shows: whether a runner Job deleted mid-`Backup`
  really resumes cleanly; Hetzner's answer when a network is deleted right
  after its servers (handled: 409/423 retried for 2 min); whether the
  workers' trust of the staging roots is enough for every curl the join
  makes; whether Velero restores the App-rendered workloads without the
  garbage collector removing them (B2's open question).
- **Agent cluster in the 3-node run.** Exit criterion 1 also wants an agent
  cluster upgraded after the console on the 3-node cluster; E1 does not
  create one (a fourth server with `install.sh --agent` and the console's
  adopt flow would be the next step).
- **k3s minor upgrade** works with `-k3s-from`/`-k3s-to` but is not a
  default job: the console only offers a minor from a Kwerft release that
  pins the next minor, so it needs a matching pair of versions.
- **AutoPatch** (exit criterion 4) is not covered: it needs a window that
  opens during the run and a newer patch release on the channel.
- **Stale bucket prefixes.** The scheduled sweeper does not look at the
  bucket; a prefix survives only if both the harness and the job's last
  step failed, and the summary names it.
- **Probing** measures from the runner, through Traefik on the server's
  public address; a gap there may also be the runner's network.
