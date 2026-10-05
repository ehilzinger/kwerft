# Releasing Kwerft

A release is a git tag. Pushing `v0.2.0` runs `.github/workflows/release.yml`,
which publishes:

| What | Where |
|---|---|
| Console image, linux/amd64 + linux/arm64 | `ghcr.io/ehilzinger/kwerft:0.2.0`, `:v0.2.0`, `:latest` (stable only) |
| SBOM (SPDX) | next to the image in GHCR (`sha256-<digest>.sbom`) and on the GitHub Release |
| Helm chart, version = appVersion = 0.2.0 | `oci://ghcr.io/ehilzinger/charts/kwerft` |
| `install.sh` and `join.sh` stamped with 0.2.0, plus `SHA256SUMS` | the GitHub Release here, and the public install repository |
| `manifest.json`: what the release pins and how to upgrade to it ([below](#the-release-manifest)) | the GitHub Release, and `v0.2.0/` in the install repository |
| `NOTES.md` (the GitHub Release's notes) and `releases.json` (every release) | the install repository, where consoles discover updates |
| GitHub Release with generated notes | this repository |
| Signatures (opt-in) | cosign keyless, stored next to the image and chart |

The code is public under AGPL-3.0-only (since 2026-10-05; private before).
The image, the chart and the install script are public too, so
`curl … | sudo bash` works on any server without credentials.

## Cut a release

1. Make sure `main` is green in CI and you are on the commit to release.
2. Tag and push. Always an annotated tag (`-a`): its message can carry
   hand-written notes and the [trailers](#upgrade-path-and-rollback-safety)
   `Upgrade-From` and `Rollback-Safe`.

   ```bash
   git tag -a v0.2.0 -m "Kwerft v0.2.0"
   git push origin v0.2.0
   ```

   The workflow first checks that the CRDs are still compatible with the
   previous releases ([crdcompat](#crd-compatibility)) and stops before
   building anything if they are not.

3. Watch the `release` workflow in the Actions tab. It runs the CI checks
   first, then builds, then publishes. The run summary lists the image and
   chart digests.
4. Try it on a fresh server:

   ```bash
   curl -fsSL https://kwerft.dev/v0.2.0/install.sh \
     | sudo bash -s -- --domain ops.example.com --yes
   ```

   Re-running the installer of a newer release on an existing server upgrades it.

   The workflow does this itself once everything is published: its `e2e`
   job installs the release on a fresh Hetzner Cloud server, and upgrades a
   second one from the stable release before it (see [e2e install
   runs](#e2e-install-runs); skipped without the `HCLOUD_TOKEN` secret).

### Prereleases

Tags with a suffix, such as `v0.2.0-rc.1`, are prereleases: the GitHub
Release is marked as one, the image gets no `latest` tag, and the install
repository gets only the versioned copy (`v0.2.0-rc.1/install.sh`); its
top-level `install.sh` keeps pointing at the latest stable release.

### Versions

- Semantic versions without build metadata (`+…` is not allowed in OCI tags).
- Git tag `v0.2.0`; chart version, appVersion and image tag `0.2.0`; the image
  is also tagged `v0.2.0`. `install.sh --version` takes `0.2.0` or `v0.2.0`.
- Nothing is bumped in the source tree. `charts/kwerft/Chart.yaml` and
  `KWERFT_VERSION_DEFAULT` in `install/install.sh` stay at `0.1.0-dev`; the
  release stamps the packaged chart and the published scripts.
- The image and chart locations are read from the pinned block of
  `install/install.sh` (`KWERFT_IMAGE_REPO`, `KWERFT_CHART_REPO`).

### The release manifest

Consoles learn about releases from the install repository
(`docs/phase6-upgrades.md`, Releases): `releases.json` lists every release,
`v<version>/manifest.json` describes one, `v<version>/NOTES.md` has its notes.
`hack/release.sh manifest` writes the manifest after the image and chart are
pushed; it is attached to the GitHub Release, and the `install-repo` job
copies it from there.

```json
{
  "version": "0.6.0",
  "channel": "stable",
  "published": "2026-11-02T10:00:00Z",
  "upgradeFrom": "0.4.0",
  "rollbackSafe": true,
  "kubernetes": { "pinned": "v1.37.1+k3s1", "supported": ["1.36", "1.37"] },
  "components": { "helm": "v4.3.0", "cilium": "1.20.2", "certManager": "v1.21.2", "traefikChart": "41.6.1", "…": "…" },
  "image": "ghcr.io/ehilzinger/kwerft@sha256:…",
  "chart": { "ref": "oci://ghcr.io/ehilzinger/charts/kwerft", "version": "0.6.0", "digest": "sha256:…" }
}
```

| Field | Source |
|---|---|
| `channel` | `edge` for prereleases (`0.6.0-rc.1`), else `stable` |
| `published` | the tag's date, in UTC |
| `upgradeFrom`, `rollbackSafe` | the tag's trailers, or their defaults (next section) |
| `kubernetes.pinned` | `K3S_VERSION` of the stamped `install.sh`; `supported` is its minor and the one before |
| `components` | every other `*_VERSION` of the pinned block, in camelCase without the suffix: `CERT_MANAGER_VERSION` → `certManager`, `TRAEFIK_CHART_VERSION` → `traefikChart`. A new pin shows up by itself. |
| `image`, `chart.digest` | the digests `push-image` and `push-chart` reported |

`releases.json` is a JSON array of `{version, channel, published}`, newest
version first (semver order: `0.6.0` above `0.6.0-rc.1` above `0.5.0`).
`install-repo` adds or replaces the release's entry, so re-running the job is
harmless. Releases before the manifest existed (up to 0.5.x) are not in it.
`LATEST` and the top-level scripts work as before.

### Upgrade path and rollback safety

Two trailers in the tag message override the defaults; nothing is bumped in
the source tree:

| Trailer | Default | Meaning |
|---|---|---|
| `Upgrade-From: 0.5.0` | two minor lines back: `0.4.0` for 0.6.x | the oldest version a console may upgrade from directly. Older ones are offered the newest release that allows them first. |
| `Rollback-Safe: no` | `yes` | a failed upgrade to this release also restores the console's database copy from before it (losing what changed in between), so the owner must accept that. Also lets incompatible CRD changes through. |

```bash
git tag -a v0.6.0 -F - <<'EOF'
Kwerft v0.6.0

Upgrades from the console. The store's audit table moves to a new schema.

Upgrade-From: 0.5.0
Rollback-Safe: no
EOF
```

The paragraphs between the first line and the trailers go on top of the
release notes. The default counts minor lines that have a release tag,
prereleases included (a major release counts the lines before it), so the
workflow checks out the full history. `hack/release.sh trailers 0.6.0` prints
what a tag resolves to; the run's summary shows it too. A lightweight tag
has no message: the defaults apply, with a warning in the log.

### CRD compatibility

An upgrade's rollback restores the Helm releases but never the CRDs, so the
previous console must keep working with the new ones. Within a served
version (`v1alpha1`), CRDs may only gain optional fields. `hack/crdcompat`
compares `charts/kwerft/crds` with those of the newest stable release before
this one and, if newer, the newest prerelease, and fails on:

- a removed CRD, version, short name or status subresource; a changed scope or kind
- a removed field (a rename is a removal; it suggests the new name)
- a new required field, or an existing field becoming required (required
  fields inside a *new* optional field are fine)
- a narrowed enum (or an enum on a field that had none), a changed type,
  lower maxima or higher minima, a new `pattern` or `format`, a map that no
  longer takes arbitrary keys, `x-kubernetes-preserve-unknown-fields` or
  int-or-string turned off

New or changed CEL rules (`x-kubernetes-validations`) and changed patterns
are printed as warnings: the tool cannot tell whether they reject old objects,
so check by hand. The release workflow runs it before building; a tag with
`Rollback-Safe: no` passes despite findings (they are still printed).

```bash
hack/release.sh crdcompat 0.6.0                 # as the workflow does
go run ./hack/crdcompat OLD_DIR charts/kwerft/crds
```

### Rehearse

Nothing is published by either of these:

- Locally, without Docker (needs Go, Node, Helm and jq):

  ```bash
  make release-dry-run RELEASE_VERSION=0.2.0
  ls dist/release   # install.sh join.sh SHA256SUMS kwerft-0.2.0.tgz
                    # kwerft-image-0.2.0-linux-{amd64,arm64}.tar sbom/ NOTES.md
                    # manifest.json install-repo/
  ```

  It also runs crdcompat, and writes `manifest.json` as a draft: the image
  by tag and no chart digest, since nothing is pushed. `install-repo/` is a
  scratch git repository showing what the install repository would get
  (`v0.2.0/`, `releases.json`, `LATEST`), starting from an empty one.

  The image tarballs work with `install.sh --image ghcr.io/ehilzinger/kwerft:0.2.0
  --image-archive kwerft-image-0.2.0-linux-amd64.tar`.

- On GitHub: Actions → release → Run workflow, with a version such as
  `0.2.0-rc.1`. It builds everything and writes the notes to the run's
  summary; nothing is published and no workflow artifacts are stored (they
  count against the account's storage quota for private repositories).

## One-time GitHub setup

Do these once, in this order. Until step 4 is done, releases still publish the
image, chart and GitHub Release; only the install repository is skipped, with
a notice in the run.

### 1. Cut the first release

GHCR creates the two packages on the first push, private like the
repository. Tag a release candidate (`v0.1.0-rc.1`) or the first release as
described above.

### 2. Make the two packages public

There is no API for this. For each of **`kwerft`** and **`charts/kwerft`**:

1. Open https://github.com/ehilzinger?tab=packages and click the package.
2. **Package settings** (right-hand side).
3. Under **Manage Actions access**, check that `ehilzinger/kwerft` is listed
   (the release workflow adds it; it needs the Write role or higher).
4. **Danger Zone → Change visibility → Public**, type the package name, confirm.

GitHub does not let a public package go back to private.

Check it worked, logged out of ghcr.io:

```bash
helm registry logout ghcr.io 2>/dev/null
helm show chart oci://ghcr.io/ehilzinger/charts/kwerft --version 0.1.0-rc.1
curl -s -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer $(curl -s 'https://ghcr.io/token?scope=repository:ehilzinger/kwerft:pull' | jq -r .token)" \
  -H 'Accept: application/vnd.oci.image.index.v1+json' \
  https://ghcr.io/v2/ehilzinger/kwerft/manifests/0.1.0-rc.1      # 200
```

`install.sh` runs the same check before it changes anything and stops with
exit code 50 while a package is still private.

### 3. Create the public install repository

```bash
gh repo create ehilzinger/kwerft-install --public --add-readme \
  --description "Install script for Kwerft (published by the release workflow)"
```

It must have a `main` branch (`--add-readme` creates it). The workflow writes
`v<version>/{install.sh,join.sh,SHA256SUMS,manifest.json,NOTES.md}` and
updates `releases.json` for every release and, for stable releases, copies
the three scripts plus `LATEST` to the top level. A README there could say:

> `curl -fsSL https://kwerft.dev/install.sh | sudo bash -s -- --domain ops.example.com --yes`
> installs the latest stable Kwerft; `v<version>/install.sh` installs a specific
> one. Files here are written by the release pipeline; don't edit them by hand.

Users and the console never see these raw URLs: `https://kwerft.dev/install.sh`
and `https://kwerft.dev/v<version>/install.sh` redirect to them (302, in the
`kwerft-homepage` repo's `netlify.toml`), and `hack/release.sh` stamps the
kwerft.dev URL into `join.sh` and the release notes. A fork with its own
`INSTALL_REPO` sets `INSTALLER_BASE` as well.

To use another name, set the repository variable `INSTALL_REPO` (step 5).
The stamped `join.sh` contains the install repository URL, so pick the name
before the first stable release.

### 4. Create the token and the secret

1. https://github.com/settings/personal-access-tokens/new (Settings → Developer
   settings → Personal access tokens → **Fine-grained tokens** → Generate new token).
2. **Token name**: `kwerft release → kwerft-install`. **Resource owner**:
   `ehilzinger`. **Expiration**: e.g. one year, and put the renewal in your
   calendar (an expired token fails the `install-repo` job, not the release).
3. **Repository access → Only select repositories →** `ehilzinger/kwerft-install`.
4. **Permissions → Repository permissions → Contents: Read and write.**
   (Metadata: Read-only is added automatically.) Nothing else.
5. Generate and copy the token, then store it in **this** repository:

   ```bash
   gh secret set INSTALL_REPO_TOKEN -R ehilzinger/kwerft   # paste the token
   ```

   or Settings → Secrets and variables → Actions → New repository secret.

If a release ran before the secret existed, publish its scripts afterwards:
re-run the `install-repo` job of that run (it fetches the stamped scripts,
`manifest.json` and the notes from the GitHub Release), or by hand:

```bash
gh release download v0.2.0 -R ehilzinger/kwerft -p install.sh -p join.sh -p SHA256SUMS -p manifest.json -D /tmp/kwerft-0.2.0
gh release view v0.2.0 -R ehilzinger/kwerft --json body --jq .body >/tmp/kwerft-0.2.0/NOTES.md
git clone https://github.com/ehilzinger/kwerft-install /tmp/kwerft-install
hack/release.sh install-repo /tmp/kwerft-install 0.2.0 /tmp/kwerft-0.2.0
git -C /tmp/kwerft-install push
```

### 5. Optional repository variables

```bash
gh variable set SIGN_RELEASES --body true -R ehilzinger/kwerft    # see "Signing"
gh variable set INSTALL_REPO --body owner/name -R ehilzinger/kwerft
```

## Signing

Off by default; `SIGN_RELEASES=true` turns on the `sign` job, which signs the
image and the chart by digest with cosign keyless (GitHub's OIDC token →
Fulcio certificate → Rekor transparency log).

The trade-off: every signature adds a **public, permanent** entry to Rekor.
Its certificate names the repository (`ehilzinger/kwerft`), the workflow
path and ref (`.github/workflows/release.yml@refs/tags/v0.2.0`), the commit
SHA and the trigger. It does not reveal any code. The repository name is
already public through the packages (their `org.opencontainers.image.source`
link), so what signing adds is the workflow layout, commit SHAs and release
times. Turn it on when that is acceptable, at the latest when the code goes
public. A key pair kept in secrets with `--tlog-upload=false` would avoid
Rekor, but users would then need the public key and could not rely on the
transparency log; it is not set up.

Verify a signed release:

```bash
cosign verify ghcr.io/ehilzinger/kwerft:0.2.0 \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github.com/ehilzinger/kwerft/\.github/workflows/release\.yml@refs/tags/v'
```

## e2e install runs

`.github/workflows/e2e.yml` runs the **published** installer on fresh
Hetzner Cloud servers and checks Kwerft the way a user would, through the
console's public HTTPS API. The harness is `hack/e2e` (Go, tested against a
fake Hetzner API, a fake server and a fake console).

| When | What |
|---|---|
| A release tag, after publishing (`release.yml` › `e2e`) | that version: a fresh install, and an upgrade from the newest stable release before it |
| Nightly, 02:17 UTC | the latest stable release, fresh and as an upgrade |
| By hand: Actions → e2e → Run workflow | any published version (empty: the latest), upgrade optional |
| Every 3 hours (and nightly) | the sweeper |

Each scenario gets its own server, in parallel. A run:

1. Uploads a per-run ed25519 SSH key and creates a server labelled
   `kwerft-e2e=true`, `run=<run id>`, `created=<unix time>`: the first
   available of `cx33`, `cx43` in `nbg1`, `fsn1`, `hel1`, Ubuntu 26.04 (24.04
   if 26.04 is missing). cloud-init only installs a host key generated for the
   run, which the harness pins.
2. Downloads `https://raw.githubusercontent.com/ehilzinger/kwerft-install/main/v<version>/install.sh`
   on the server and runs it with `--domain <ip>.sslip.io --acme-server staging --yes`;
   it must exit 0. (Releases up to v0.4.0 have no `--acme-server`; their
   ClusterIssuer is switched to staging right after the installer.)
3. Checks: the console on HTTPS with a Let's Encrypt (staging) certificate —
   Traefik's self-signed fallback fails the check — `/healthz`, the version;
   the owner account from the installer's setup token, sign-in; a project
   with an image App on `web.<ip>.sslip.io` answering over HTTPS (the time
   from SSH to here is the Phase 1 "fresh server → app on HTTPS < 10 min");
   a Task that succeeds and restarts that App; a Git build of
   `github.com/traefik/whoami` (Dockerfile) that deploys and answers on its
   own hostname (budget 3 min); log search finds the Task's output; the
   metrics explorer returns the project's series; a crash-looping App raises
   its alert in the console's alerts API (budget 2 min). Over a budget the
   check is marked *slow*, not failed.
4. For the upgrade: before upgrading, the owner and an App on
   `hello.<ip>.sslip.io`; after it, the console reports the new version, the
   owner still signs in and the App still answers; then the checks of step 3.
5. **Always deletes the server and the SSH key** — after a failure, a
   timeout (80 min) or a cancel too — and waits until Hetzner confirms. A
   final workflow step deletes anything left with the run's label, and the
   sweeper deletes anything labelled `kwerft-e2e=true` older than 3 hours.

Results are in the run's summary (a table per scenario, the server type,
the cost, the cleanup, the installer's last lines on failure); there are no
workflow artifacts. A failed run after a release does **not** unpublish
anything; the summary says so. Fix forward with a new tag.

Let's Encrypt staging is used because `sslip.io` is shared by everyone (it
is not on the Public Suffix List), so production certificates for it run into
Let's Encrypt's per-domain rate limit; staging's limits are far higher. Its
certificates are untrusted; the harness accepts them only if the issuer says
`(STAGING)` and the name matches.

### One-time setup

1. In the [Hetzner Console](https://console.hetzner.com), create a **new
   project** just for this, e.g. `kwerft-e2e`, with nothing else in it. The
   sweeper only deletes resources labelled `kwerft-e2e=true`, but a separate
   project keeps the token's reach small and the bill readable.
2. In that project: **Security → API tokens → Generate API token**, name
   `github e2e`, permission **Read & Write**.
3. Store it in this repository (paste when asked):

   ```bash
   gh secret set HCLOUD_TOKEN -R ehilzinger/kwerft
   ```

4. Optional repository variables, if `cx33` is often unavailable or you want
   other locations (comma-separated, tried in order):

   ```bash
   gh variable set E2E_SERVER_TYPES --body cx33,cx43 -R ehilzinger/kwerft
   gh variable set E2E_LOCATIONS --body nbg1,fsn1,hel1 -R ehilzinger/kwerft
   ```

The project's server limit must allow two servers at a time (fresh and
upgrade run in parallel); new Hetzner accounts start with a small limit.

### Costs

`cx33` (4 vCPU, 8 GB, 80 GB) costs €0.0136 per hour plus about €0.001 for
the IPv4 address (prices from 15 June 2026, excl. VAT); Hetzner bills each
started hour. A run takes about 25–40 minutes, so each scenario costs one
server-hour: about €0.03 per release tag (two servers) and the same per
night, roughly €1 a month. The run summary shows the price the API reported.

`cx23` (the successor of `cx22`: 2 vCPU, 4 GB, €0.0088/h) is cheaper but
below the recommended 8 GB: the platform takes about 2.5 GB and a Git build
1–2 GB more, so runs on it would test memory pressure rather than Kwerft.
`E2E_SERVER_TYPES=cx23` tries it.

### Run it

- In GitHub: Actions → **e2e** → Run workflow, or

  ```bash
  gh workflow run e2e.yml -R ehilzinger/kwerft -f version=0.4.0
  gh run watch -R ehilzinger/kwerft
  ```

- Locally (creates a real server, deleted at the end; Ctrl-C deletes it too):

  ```bash
  go run ./hack/e2e run -version 0.4.0 -dry-run           # the plan, nothing created
  HCLOUD_TOKEN=… go run ./hack/e2e run -version 0.4.0 -from 0.3.0
  HCLOUD_TOKEN=… go run ./hack/e2e sweep -dry-run          # what the sweeper would delete
  ```

### When it fails

- **"Installer not published"** — the `install-repo` job of that release did
  not run (no `INSTALL_REPO_TOKEN`); publish the scripts as in step 4 of the
  one-time GitHub setup, then re-run.
- **No server could be created** — every type/location was unavailable or the
  project is at its server limit; the summary lists what was tried.
- **Cleanup incomplete** — the summary says which server or key is left. The
  sweeper deletes it within 3 hours, or delete it in the Hetzner Console.
- The job log has the installer's full output and, after a failed install,
  the tail of `/var/log/kwerft/install.log` and the pods that are not running.

## How it is built

- **ko, not `docker buildx`.** Kwerft is a single static Go binary with the UI
  embedded, so ko cross-compiles both architectures natively (no QEMU, no
  Docker daemon), produces the same distroless/nonroot image as the
  `Dockerfile`, generates the SBOM, and is the tool `make dev-server` already
  uses. That is also why the release can be rehearsed on a Mac. The
  `Dockerfile` remains for `make image`; keep the two in step.
- The steps live in `hack/release.sh`; the workflow and `make release-dry-run`
  call the same commands. ko's SBOM covers the Go modules in the binary, not
  the files of the distroless base image.
- The manifest, `releases.json` and the version arithmetic (semver order,
  minor lines) use jq, which the runners have. `install/test/release.bats`
  covers trailers, manifest, notes, install repository and the crdcompat
  wrapper in scratch repositories; `hack/crdcompat` has Go tests with fixture
  CRDs.
- The `build` job has no write permissions; only `publish` (packages, contents),
  `sign` (OIDC) and `install-repo` (its own token) can write.

## Troubleshooting

- **`denied: permission_denied: write_package`** — the package exists but this
  repository has no write access to it (e.g. it was first pushed by hand).
  Package settings → Manage Actions access → Add repository → `ehilzinger/kwerft`, role Write.
- **Installer exits 50, "still private"** — step 2.
- **Installer exits 50, "not published"** — the tag never finished the
  `publish` job, or `--version` has a typo.
- **raw.githubusercontent.com serves the old `install.sh`** — its CDN caches
  for a few minutes; the versioned path is never stale.
- **"the CRDs changed incompatibly"** — the release stopped before building
  anything. Make the change additive (keep the old field, add the new one,
  migrate in a later release), or, if a rollback across it is acceptable,
  delete the tag and tag again with `Rollback-Safe: no`
  (`git tag -d v0.6.0 && git push origin :refs/tags/v0.6.0`).
- **"lightweight tag" in the log** — the tag was made without `-a`, so its
  trailers were not read and the defaults apply. Re-tag with `git tag -a` if
  it should carry trailers.
