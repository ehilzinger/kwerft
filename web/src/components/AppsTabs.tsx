// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { Link } from "@tanstack/react-router";
import "../styles/jobs.css";

// Apps | Volumes, shown above both lists. Volumes live under Apps: they are
// the project's disks that apps (and the jobs started from them) mount, and
// the blueprint has no storage section of its own.
export function AppsTabs({ current }: { current: "apps" | "volumes" }) {
  return (
    <nav className="tabs" aria-label="Apps and volumes">
      <Link to="/apps" className={current === "apps" ? "on" : undefined} aria-current={current === "apps" ? "page" : undefined}>Apps</Link>
      <Link to="/apps/volumes" search={{}} className={current === "volumes" ? "on" : undefined} aria-current={current === "volumes" ? "page" : undefined}>Volumes</Link>
    </nav>
  );
}
