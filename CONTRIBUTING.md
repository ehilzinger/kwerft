# Contributing to Kwerft

Thank you for your interest. Kwerft is licensed under the
[GNU Affero General Public License v3.0 only](LICENSE), and is also offered
under a commercial license. That dual licensing needs every contributor to
sign a contributor license agreement (CLA) first.

The agreement is [CLA.md](CLA.md). You keep your copyright; you give the
maintainer a license to use your contributions under the AGPL and the
commercial license. When you open your first pull request, a check named
**cla** asks you to sign by commenting one sentence on it; that covers all
your later contributions. Contributing for an employer? See section 5 of the
agreement. (The agreement is still a draft pending legal review.)

Issues are welcome too: bug reports, questions and ideas.

The name "Kwerft" and its logo are covered by the
[trademark policy](TRADEMARKS.md), not by the code's license.

Security problems: please do not open a public issue; write to the address
on the author's GitHub profile instead.

## License headers

Every source file (`.go`, `.ts`, `.tsx`, `.js`, `.mjs`, `.css`, `.sh`,
`.bats`) starts with the lines `SPDX-FileCopyrightText: 2026 Enzo Hilzinger`
and `SPDX-License-Identifier: AGPL-3.0-only` in its comment syntax (after a
shebang, if any); `make lint` and CI run `hack/check-license-headers.sh` to
enforce it, and `make generate` adds them to generated Go files from
`hack/boilerplate.go.txt`.
