// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { LOCAL, useClusters } from "../clusters";
import { Icon } from "./Icon";
import "../styles/workloads.css";

// The cluster bits every list page shares (Phase 5). All of them render
// nothing while the console manages only its own cluster, so single-cluster
// installs look exactly as before.

/** The cluster an item lives in, as a small tag. Only with more than one cluster. */
export function ClusterBadge({ cluster }: { cluster?: string }) {
  const { multi, unreachable } = useClusters();
  if (!multi) return null;
  const name = cluster || LOCAL;
  const away = unreachable.includes(name);
  return (
    <span className={`tag cl-badge${away ? " away" : ""}`} title={away ? `Cluster ${name} cannot be reached right now` : `Cluster ${name}`}>
      <Icon name="server" />{name}
    </span>
  );
}

/** "All clusters / local / edge", for list toolbars. */
export function ClusterFilter({ value, onChange }: { value?: string; onChange: (c?: string) => void }) {
  const { clusters, multi } = useClusters();
  if (!multi) return null;
  return (
    <div className="seg" role="group" aria-label="Cluster">
      <button aria-pressed={!value} onClick={() => onChange(undefined)}>All clusters</button>
      {clusters.map((c) => (
        <button key={c.name} aria-pressed={value === c.name} onClick={() => onChange(c.name)}
          title={c.connected ? undefined : "Cannot be reached right now"}>
          {!c.connected && <Icon name="alert" />}{c.name}
        </button>
      ))}
    </div>
  );
}

/** One cluster of several, for pages that show one cluster's data at a time (metrics). */
export function ClusterPicker({ value, onChange }: { value: string; onChange: (c: string) => void }) {
  const { clusters, multi } = useClusters();
  if (!multi) return null;
  return (
    <div className="seg" role="group" aria-label="Cluster">
      {clusters.map((c) => (
        <button key={c.name} aria-pressed={value === c.name} disabled={!c.connected} onClick={() => onChange(c.name)}
          title={c.connected ? `Cluster ${c.name}` : "Cannot be reached right now"}>
          <Icon name="server" />{c.name}
        </button>
      ))}
    </div>
  );
}

/** A banner naming the clusters that cannot be reached: their projects are left out of lists. */
export function UnreachableBanner() {
  const { unreachable } = useClusters();
  if (unreachable.length === 0) return null;
  const names = unreachable.join(", ");
  return (
    <div className="banner warn cl-banner" role="status">
      <Icon name="alert" />
      <span>
        {unreachable.length === 1 ? <>Cluster <b>{names}</b> cannot</> : <>Clusters <b>{names}</b> cannot</>} be reached right now: their projects are left out of
        lists, and their pages answer once the cluster's agent is connected again.
      </span>
    </div>
  );
}
