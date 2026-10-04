import { useState } from "react";
import { Link } from "@tanstack/react-router";
import { useQueries, useQuery } from "@tanstack/react-query";
import { api } from "../api";
import { alertingUnavailable, attentionHeadline, attentionItems, type AttentionItem } from "../alerts";
import type { Build } from "../builds";
import { Icon } from "../components/Icon";
import { jobs } from "../jobs";
import { ago, workloads } from "../workloads";
import { RouterLink, useFiringAlerts } from "./MonitoringAlerts";
import "../styles/workloads.css";
import "../styles/monitoring.css";

const POLL = 15000;
/** Latest builds are fetched per Git app; past this many apps the feed relies on the BuildFailing alert. */
const MAX_BUILD_LOOKUPS = 40;
const SHOWN = 6;

export function Overview() {
  const version = useQuery({ queryKey: ["version"], queryFn: api.version, staleTime: Infinity });
  const attention = useAttention();
  const { title, detail } = attentionHeadline(attention.items);
  const worst = attention.items.some((i) => i.tone === "bad") ? "bad" : "warn";
  const alerts = attention.items.filter((i) => i.alert).length;

  return (
    <section className="view">
      <div className="ph">
        <div>
          <h1>Overview</h1>
          <p>Cluster health, capacity and what needs attention.</p>
        </div>
      </div>
      {attention.items.length > 0 && (
        <div className={`banner ${worst}`} role="status">
          <Icon name="alert" />
          <span><b>{title}</b> {detail}</span>
          {alerts > 0 ? <Link to="/monitoring" className="btn sm">Review alerts</Link> : <a href="#attention" className="btn sm">Review</a>}
        </div>
      )}
      <div className="tiles">
        <div className="tile">
          <span className="k">Console</span>
          <span className="v">{version.data?.version ?? "—"}</span>
          <span className="s">
            {version.isError ? <span className="pill bad">API unreachable</span> : <span className="pill ok">Running</span>}
          </span>
        </div>
        <div className="tile">
          <span className="k">Platform</span>
          <span className="v">{version.data ? (version.data.platform === "cloud" ? "Hetzner Cloud" : "Dedicated") : "—"}</span>
          <span className="s">Detected by the installer</span>
        </div>
      </div>
      <NeedsAttention {...attention} />
    </section>
  );
}

/** Firing alerts plus what the console knows on its own; each source may fail without hiding the others. */
function useAttention() {
  const firing = useFiringAlerts();
  const apps = useQuery({ queryKey: ["apps"], queryFn: () => workloads.apps(), refetchInterval: POLL });
  const schedules = useQuery({ queryKey: ["schedules"], queryFn: () => jobs.schedules(), refetchInterval: 30000 });
  const domains = useQuery({ queryKey: ["domains"], queryFn: () => jobs.domains(), refetchInterval: 60000 });
  const gitApps = (apps.data ?? []).filter((a) => a.source.type === "git").slice(0, MAX_BUILD_LOOKUPS);
  const details = useQueries({
    queries: gitApps.map((a) => ({
      queryKey: ["app", a.project, a.name],
      queryFn: () => workloads.app(a.project, a.name),
      staleTime: 30000,
      refetchInterval: 60000,
    })),
  });
  const builds: Record<string, Build | undefined> = {};
  gitApps.forEach((a, i) => {
    builds[`${a.project}/${a.name}`] = details[i]?.data?.latestBuild;
  });
  const items = attentionItems({
    alerts: firing.data, apps: apps.data, builds, schedules: schedules.data, domains: domains.data, origin: window.location.origin,
  });
  return {
    items,
    loading: apps.isPending || (firing.isPending && !firing.isError),
    alertingOff: firing.isError && alertingUnavailable(firing.error),
    failed: [apps.isError && "apps", schedules.isError && "schedules", domains.isError && "domains", firing.isError && !alertingUnavailable(firing.error) && "alerts"]
      .filter((x): x is string => !!x),
  };
}

function NeedsAttention({ items, loading, alertingOff, failed }: { items: AttentionItem[]; loading: boolean; alertingOff: boolean; failed: string[] }) {
  const [all, setAll] = useState(false);
  const shown = all ? items : items.slice(0, SHOWN);
  return (
    <div className="card attention" id="attention">
      <div className="ch-h">
        <h3>Needs attention</h3>
        <Link to="/monitoring" className="btn ghost sm">Monitoring</Link>
      </div>
      {loading && items.length === 0 && <p className="loading pad">Checking…</p>}
      {!loading && items.length === 0 && (
        <div className="all-clear"><Icon name="shield" /><span>Nothing needs attention: apps run, builds and scheduled jobs pass, certificates are valid{alertingOff ? "" : " and no alert fires"}.</span></div>
      )}
      {items.length > 0 && (
        <div className="list">
          {shown.map((i) => (
            <div className="li" key={i.key}>
              <span className={`ico ${i.tone}`}><Icon name={i.icon} /></span>
              <div className="grow">
                <b>{i.title}</b> · {i.what}
                {i.detail && <p>{i.detail}</p>}
                <div className="acts">
                  <RouterLink href={i.action.href} className="btn ghost sm">{i.action.label} →</RouterLink>
                </div>
              </div>
              {i.since && <span className="when" title={new Date(i.since).toLocaleString()}>{ago(i.since)}</span>}
            </div>
          ))}
          {items.length > SHOWN && (
            <div className="li">
              <button type="button" className="linkbtn" onClick={() => setAll(!all)} aria-expanded={all}>
                {all ? "Show fewer" : `Show all ${items.length}`}
              </button>
            </div>
          )}
        </div>
      )}
      {(alertingOff || failed.length > 0) && (
        <div className="bd sep">
          {alertingOff && (
            <p className="dim note">Alerts from monitoring are not available on this console yet; this list shows what Kwerft knows without them.</p>
          )}
          {failed.length > 0 && <p className="warn-text note">Could not check {failed.join(", ")}; the list may be incomplete.</p>}
        </div>
      )}
    </div>
  );
}
