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
