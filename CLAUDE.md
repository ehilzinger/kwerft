# Kwerft — notes for Claude

Self-hosted Kubernetes console for Hetzner (Cloud + dedicated), installed by
`install/install.sh`. Plan and decisions: `docs/plan.md`. UI mockups and the
long-form plan: `docs/blueprint.html` (also published as a claude.ai artifact:
https://claude.ai/artifact/EzgtpYa7fVU5rYQymWWdZ2 — update that URL in place
when the blueprint changes).

## Conventions

- Kubernetes is the source of truth: features start as a type in
  `api/v1alpha1`, then a reconciler, then API + UI. Run `make generate` after
  editing types and commit the generated files.
- The API acts on behalf of users via impersonation; never use the controller's
  own permissions for user-initiated writes.
- Installer: bash, must pass shellcheck, stays a single self-contained file
  (it is piped from curl). Every stage is idempotent and returns its one-line
  summary on stdout. Exit codes are a public contract — don't renumber.
- Version pins live at the top of `install/install.sh`; bump them together and
  note the date.
- UI design tokens live in `web/src/styles/tokens.css` and mirror the
  blueprint. Fonts are self-hosted (CSP is `'self'` only).
- Hosted under the personal GitHub account `ehilzinger`: module
  `github.com/ehilzinger/kwerft`, image `ghcr.io/ehilzinger/kwerft`. The
  advertised installer URL is `https://kwerft.dev/install.sh` (pinned:
  `https://kwerft.dev/v<version>/install.sh`), 302 redirects in the
  `../kwerft-homepage` repo's `netlify.toml` to the raw files in the public
  `ehilzinger/kwerft-install` repo.

## Commands

`make test`, `make lint`, `make generate`, `make web`, `make check`.
