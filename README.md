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
curl -fsSL https://raw.githubusercontent.com/ehilzinger/kwerft/main/install/install.sh | sudo bash -s -- --domain ops.example.com --email ops@example.com --yes
```

Until the console image is published, the last stage needs a locally built
image (see the roadmap in docs/plan.md). A shorter `get.kwerft.dev`-style URL
comes once Kwerft has its own domain.

No domain yet? Leave out `--domain` and the console gets a temporary
`<public-ip>.sslip.io` hostname; re-run with `--domain` once DNS points at the
server. `--email` is optional.

`./install/install.sh --help` lists every flag; `--dry-run` prints the plan
without changing anything. Re-running the script resumes or repairs an
install. For unattended installs (cloud-init), pass `--config kwerft.yaml`:

```yaml
domain: ops.example.com
email: ops@example.com
dns: { solver: hetzner, tokenFile: /root/dns.token }
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
```

Try a change on a real server (no Docker or registry needed — the image is
built locally with ko and copied over SSH):

```bash
make dev-server HOST=root@203.0.113.24 ARGS="--domain ops.example.com"
```

Run the console locally with live UI reload — `make dev-api` in one terminal,
`make dev-web` in another, then open http://localhost:5173.

## License

To be decided before the public beta. All dependencies are Apache-2.0 or MIT.
