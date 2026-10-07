# Contributing to Kwerft

Thank you for your interest. Kwerft is licensed under the
[GNU Affero General Public License v3.0 only](LICENSE), and is also offered
under a commercial license. That dual licensing needs every contributor to
sign a contributor license agreement (CLA) first.

**The CLA is still being prepared, so pull requests cannot be merged yet.**
Issues are welcome: bug reports, questions and ideas. Once the CLA is in
place, a check on every pull request will ask you to sign it.

Security problems: please do not open a public issue; write to the address
on the author's GitHub profile instead.

## License headers

Every source file (`.go`, `.ts`, `.tsx`, `.js`, `.mjs`, `.css`, `.sh`,
`.bats`) starts with the lines `SPDX-FileCopyrightText: 2026 Enzo Hilzinger`
and `SPDX-License-Identifier: AGPL-3.0-only` in its comment syntax (after a
shebang, if any); `make lint` and CI run `hack/check-license-headers.sh` to
enforce it, and `make generate` adds them to generated Go files from
`hack/boilerplate.go.txt`.
