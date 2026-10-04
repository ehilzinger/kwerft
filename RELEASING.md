# Releasing Kwerft

A release is a git tag. Pushing `v0.2.0` runs `.github/workflows/release.yml`,
which publishes:

| What | Where |
|---|---|
| Console image, linux/amd64 + linux/arm64 | `ghcr.io/ehilzinger/kwerft:0.2.0`, `:v0.2.0`, `:latest` (stable only) |
| SBOM (SPDX) | next to the image in GHCR (`sha256-<digest>.sbom`) and on the GitHub Release |
| Helm chart, version = appVersion = 0.2.0 | `oci://ghcr.io/ehilzinger/charts/kwerft` |
| `install.sh` and `join.sh` stamped with 0.2.0, plus `SHA256SUMS` | the GitHub Release here, and the public install repository |
| GitHub Release with generated notes | this (private) repository |
| Signatures (opt-in) | cosign keyless, stored next to the image and chart |

The code stays private. The image, the chart and the install script are
public, so `curl … | sudo bash` works on any server without credentials.

## Cut a release

1. Make sure `main` is green in CI and you are on the commit to release.
2. Tag and push:

   ```bash
   git tag -a v0.2.0 -m "Kwerft v0.2.0"
   git push origin v0.2.0
   ```

3. Watch the `release` workflow in the Actions tab. It runs the CI checks
   first, then builds, then publishes. The run summary lists the image and
   chart digests.
4. Try it on a fresh server:

   ```bash
   curl -fsSL https://raw.githubusercontent.com/ehilzinger/kwerft-install/main/v0.2.0/install.sh \
     | sudo bash -s -- --domain ops.example.com --yes
   ```

   Re-running the installer of a newer release on an existing server upgrades it.

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

### Rehearse

Nothing is published by either of these:

- Locally, without Docker (needs Go, Node and Helm):

  ```bash
  make release-dry-run RELEASE_VERSION=0.2.0
  ls dist/release   # install.sh join.sh SHA256SUMS kwerft-0.2.0.tgz
                    # kwerft-image-0.2.0-linux-{amd64,arm64}.tar sbom/ NOTES.md
  ```

  The image tarballs work with `install.sh --image ghcr.io/ehilzinger/kwerft:0.2.0
  --image-archive kwerft-image-0.2.0-linux-amd64.tar`.

- On GitHub: Actions → release → Run workflow, with a version such as
  `0.2.0-rc.1`. The artifacts are attached to the run.

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
`v<version>/{install.sh,join.sh,SHA256SUMS}` for every release and, for stable
releases, the same files plus `LATEST` at the top level. A README there could
say:

> `curl -fsSL https://raw.githubusercontent.com/ehilzinger/kwerft-install/main/install.sh | sudo bash -s -- --domain ops.example.com --yes`
> installs the latest stable Kwerft; `v<version>/install.sh` installs a specific
> one. Files here are written by the release pipeline; don't edit them by hand.

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
re-run the `install-repo` job of that run (within 14 days, while its artifacts
exist), or by hand:

```bash
gh release download v0.2.0 -R ehilzinger/kwerft -p install.sh -p join.sh -p SHA256SUMS -D /tmp/kwerft-0.2.0
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
