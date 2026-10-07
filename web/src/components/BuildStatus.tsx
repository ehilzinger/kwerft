// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { buildLook, type Build } from "../builds";

// The status pill of a build: Queued, Building, Deployed, Superseded, Failed, …
export function BuildStatus({ build }: { build: Build }) {
  const { cls, label } = buildLook(build);
  return <span className={`pill ${cls}`} title={build.statusMessage}>{label}</span>;
}
