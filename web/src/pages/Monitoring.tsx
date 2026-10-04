import { type ReactNode } from "react";
import { Link } from "@tanstack/react-router";
import "../styles/workloads.css";
import "../styles/jobs.css";

// The Monitoring page: Alerts | Metrics | Logs | Rules | Channels. Each tab
// is its own route and renders inside MonitoringLayout, so the header and
// tab strip stay the same. Phase 3 workers own one tab file each (see
// docs/phase3.md).

export type MonitoringTab = "alerts" | "metrics" | "logs" | "rules" | "channels";

const tabs: { id: MonitoringTab; to: string; label: string }[] = [
  { id: "alerts", to: "/monitoring", label: "Alerts" },
  { id: "metrics", to: "/monitoring/metrics", label: "Metrics" },
  { id: "logs", to: "/monitoring/logs", label: "Logs" },
  { id: "rules", to: "/monitoring/rules", label: "Alert rules" },
  { id: "channels", to: "/monitoring/channels", label: "Channels" },
];

export function MonitoringTabs({ current }: { current: MonitoringTab }) {
  return (
    <nav className="tabs" aria-label="Monitoring">
      {tabs.map((t) => (
        <Link key={t.id} to={t.to} className={current === t.id ? "on" : undefined} aria-current={current === t.id ? "page" : undefined}>
          {t.label}
        </Link>
      ))}
    </nav>
  );
}

/** Header and tab strip shared by every Monitoring tab. */
export function MonitoringLayout({ current, actions, children }: { current: MonitoringTab; actions?: ReactNode; children: ReactNode }) {
  return (
    <section className="view">
      <div className="ph">
        <div>
          <h1>Monitoring</h1>
          <p>Alerts, resource usage, logs and where notifications go · metrics kept 30 days, logs 14 days</p>
        </div>
        {actions && <div className="acts">{actions}</div>}
      </div>
      <MonitoringTabs current={current} />
      <div className="tabpanel">{children}</div>
    </section>
  );
}
