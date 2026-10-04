import { type ReactNode } from "react";
import { Link } from "@tanstack/react-router";
import "../styles/workloads.css";
import "../styles/jobs.css";

// Clusters & nodes (Phase 5). /clusters lists the clusters (W3); a
// cluster's page has tabs Overview (W3) and Nodes (W2), each its own file
// (see docs/phase5.md).

export type ClusterTab = "overview" | "nodes";

export function ClusterTabs({ cluster, current }: { cluster: string; current: ClusterTab }) {
  const tabs: { id: ClusterTab; label: string; to: string }[] = [
    { id: "overview", label: "Overview", to: "/clusters/$name" },
    { id: "nodes", label: "Nodes", to: "/clusters/$name/nodes" },
  ];
  return (
    <nav className="tabs" aria-label="Cluster">
      {tabs.map((t) => (
        <Link key={t.id} to={t.to} params={{ name: cluster }} className={current === t.id ? "on" : undefined} aria-current={current === t.id ? "page" : undefined}>
          {t.label}
        </Link>
      ))}
    </nav>
  );
}

/** Header and tab strip shared by a cluster's tabs. */
export function ClusterLayout({ cluster, current, actions, children }: { cluster: string; current: ClusterTab; actions?: ReactNode; children: ReactNode }) {
  return (
    <section className="view">
      <div className="ph">
        <div>
          <p className="crumb"><Link to="/clusters">Clusters</Link></p>
          <h1>{cluster}</h1>
        </div>
        {actions && <div className="acts">{actions}</div>}
      </div>
      <ClusterTabs cluster={cluster} current={current} />
      <div className="tabpanel">{children}</div>
    </section>
  );
}
