# Kwerft

Rent a Hetzner server, run one script, manage containers from the browser.

Kwerft installs a hardened Kubernetes (k3s + Cilium) on a fresh Ubuntu server —
Hetzner Cloud or dedicated — and puts a console on top: deploy registry images
or Git repositories, watch them, and administer network rules, domains, nodes,
clusters and access.

> **Status: Phase 0 (foundations).** The installer, chart, API types and
> console shell exist; most console features are still to be built. See
> [docs/plan.md](docs/plan.md) for the roadmap and
> [docs/blueprint.html](docs/blueprint.html) for clickable UI mockups.

## Install

```bash
curl -fsSL https://kwerft.dev/install.sh | sudo bash -s -- --domain ops.example.com --email ops@example.com --yes
```

That URL always serves the latest stable release; pin one with
`https://kwerft.dev/v0.4.0/install.sh`. Both redirect (302, set in the
`kwerft-homepage` repo's `netlify.toml`) to the script the release pipeline
publishes to the public
[`ehilzinger/kwerft-install`](https://github.com/ehilzinger/kwerft-install)
repository. The image (`ghcr.io/ehilzinger/kwerft`, amd64 and arm64) and the
Helm chart (`oci://ghcr.io/ehilzinger/charts/kwerft`) are public, so no
registry login is needed. To try unreleased changes, install from a checkout:
`make dev-server HOST=root@<ip>` (see Development).

No domain yet? Leave out `--domain` and the console gets a temporary
`<public-ip>.sslip.io` hostname; once DNS points at the server, change it under
Settings in the console (or re-run with `--domain`). A re-run without
`--domain` keeps whatever Settings chose. `--email` is optional.
`--acme-server staging` takes certificates from Let's Encrypt's staging CA
(not trusted by browsers, generous rate limits — for test servers).

`./install/install.sh --help` lists every flag; `--dry-run` prints the plan
without changing anything. Re-running the script resumes or repairs an
install. For unattended installs (cloud-init), pass `--config kwerft.yaml`:

```yaml
domain: ops.example.com
email: ops@example.com
appsDomain: apps.example.com          # apps get <name>.apps.example.com
dns: { solver: hetzner, tokenFile: /root/dns.token }   # wildcard certificate via Hetzner DNS
owner: { email: you@example.com, passwordFile: /root/owner.pw }
```

## Repository

| Path | What |
|---|---|
| `install/` | `install.sh` (install, join, uninstall, firewall rescue), `join.sh`, bats tests |
| `charts/kwerft/` | Helm chart; `crds/` is generated |
| `api/v1alpha1/` | `Project`, `App`, `Build` custom resource types |
| `cmd/kwerft/` | The console binary: API, reconcilers, embedded UI |
| `internal/` | Server, version and (soon) controllers, auth, Hetzner clients |
| `web/` | React + TypeScript + Vite console |
| `docs/` | Plan and blueprint (mockups) |

## Development

Requirements: Go 1.26+, Node 24+. Optional: shellcheck, bats, helm, docker.

```bash
make web        # build the UI into web/dist
make test       # Go tests, incl. controllers against a real API server (envtest)
make lint       # gofmt, go vet, TypeScript
make generate   # regenerate deepcopy + CRDs after editing api/v1alpha1
make check      # everything CI runs
make release-dry-run RELEASE_VERSION=0.2.0   # build release artifacts, publish nothing
```

Releases are cut by pushing a `v*` tag; see [RELEASING.md](RELEASING.md).

Try a change on a real server (no Docker or registry needed — the image is
built locally with ko and copied over SSH):

```bash
make dev-server HOST=root@203.0.113.24 ARGS="--domain ops.example.com"
```

Run the console locally with live UI reload — `make dev-api` in one terminal,
`make dev-web` in another, then open http://localhost:5173.

## License

Kwerft is free software under the [GNU Affero General Public License v3.0
only](LICENSE) (`AGPL-3.0-only`): anyone may run, modify and share it, and
whoever offers a modified Kwerft to others over a network must publish their
changes. A commercial license is available on request for companies that
cannot use the AGPL. All dependencies are Apache-2.0 or MIT.

Contributions need a contributor license agreement, which is being prepared;
see [CONTRIBUTING.md](CONTRIBUTING.md). "Kwerft" is a trademark of its author.
