import { useState, type FormEvent } from "react";
import { Link } from "@tanstack/react-router";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { ApiError, api } from "../api";
import { LOCAL } from "../clusters";
import { ClusterPicker } from "../components/ClusterUI";
import { Chart, Legend, type ChartSeries } from "../components/Chart";
import { Icon } from "../components/Icon";
import {
  RANGE_LABEL, formatBytes, formatCores, formatNumber, formatPercent, metricsApi, seriesName, summarize,
  type AppUsage, type MetricsOverview, type NodeUsage, type RangeId,
} from "../metrics";
import { errorText } from "./Apps";
import { RangePicker, stepText } from "./AppMetrics";
import { MonitoringLayout } from "./Monitoring";
import "../styles/metrics.css";

// Monitoring › Metrics (W1, Phase 3; see docs/phase3.md): nodes, the
// platform's own usage, the heaviest apps, and for owners and admins an
// explorer that takes PromQL. Developers and viewers see their projects'
// apps only; the server confines every query (internal/server/api_metrics.go).

export function MonitoringMetrics() {
  const [range, setRange] = useState<RangeId>("1h");
  const [cluster, setCluster] = useState(LOCAL);
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const q = useQuery({
    queryKey: ["metrics-overview", range, cluster],
    queryFn: () => metricsApi.overview(range, cluster),
    refetchInterval: 30_000,
    placeholderData: keepPreviousData,
  });
  const role = session.data?.role;
  const explore = role === "owner" || role === "admin";

  return (
    <MonitoringLayout current="metrics">
      <div className="mx-toolbar">
        <ClusterPicker value={cluster} onChange={setCluster} />
        <RangePicker value={range} onChange={setRange} />
        <span className="dim">{q.isFetching ? "Updating…" : `Last ${RANGE_LABEL[range]}${q.data ? ` · every ${stepText(q.data.step)}` : ""}`}</span>
      </div>
      {q.isPending && <p className="loading">Loading metrics…</p>}
      {q.isError && (
        <div className="empty">
          <h2>{q.error instanceof ApiError && q.error.status === 503 ? "Metrics are unavailable" : "Could not load metrics"}</h2>
          <p>{errorText(q.error)}</p>
        </div>
      )}
      {q.data && <Overview data={q.data} stale={q.isPlaceholderData} />}
      {explore && <Explorer range={range} cluster={cluster} />}
    </MonitoringLayout>
  );
}

function Overview({ data: d, stale }: { data: MetricsOverview; stale: boolean }) {
  const all = d.scope === "all";
  const win = { start: d.start, end: d.end, step: d.step };
  const cpuCap = d.nodes.reduce((s, n) => s + n.cpuCapacity, 0);
  const memCap = d.nodes.reduce((s, n) => s + n.memoryCapacity, 0);
  const flat = (v: number) => (d.series.cpu ?? d.series.memory ?? []).map((p) => ({ t: p.t, v }));
  const usage = (label: string, s: typeof d.series.cpu, cap: number): ChartSeries[] => [
    ...(s ? [{ label, points: s, area: true }] : []),
    ...(all && cap > 0 && s ? [{ label: "capacity", points: flat(cap), tone: "warn" as const, dashed: true }] : []),
  ];
  const what = all ? "Cluster" : "Your projects' apps";

  return (
    <div className={stale ? "mx-stale" : undefined} style={{ display: "grid", gap: 16, marginBottom: 16 }}>
      {all && d.nodes.length > 0 && (
        <div className="mx-tiles" style={{ marginBottom: 0 }}>
          {d.nodes.map((n) => <NodeTile key={n.name} node={n} />)}
        </div>
      )}
      {all && d.nodes.length === 0 && (
        <div className="banner warn" role="status"><Icon name="alert" /><span>No node metrics yet. node-exporter in kwerft-observability reports every 20 seconds once it runs.</span></div>
      )}

      <div className="mx-grid">
        <div className="card mx-card">
          <div className="ch-h">
            <h3>{what} CPU <span className="dim" style={{ fontWeight: 400 }}>· cores</span></h3>
            {all ? <Legend items={[{ label: "in use", tone: "accent" }, { label: "capacity", tone: "warn", dashed: true }]} />
              : d.series.cpu && <span className="now">{formatCores(summarize(d.series.cpu)!.latest)}</span>}
          </div>
          {d.series.cpu ? <div className="bd"><Chart title={`${what} CPU in cores`} {...win} format={formatCores} series={usage("in use", d.series.cpu, cpuCap)} /></div>
            : <div className="mx-none">No CPU usage recorded in this range.</div>}
        </div>
        <div className="card mx-card">
          <div className="ch-h">
            <h3>{what} memory</h3>
            {all ? <Legend items={[{ label: "in use", tone: "accent" }, { label: "capacity", tone: "warn", dashed: true }]} />
              : d.series.memory && <span className="now">{formatBytes(summarize(d.series.memory)!.latest)}</span>}
          </div>
          {d.series.memory ? <div className="bd"><Chart title={`${what} memory`} {...win} format={formatBytes} bytes series={usage("in use", d.series.memory, memCap)} /></div>
            : <div className="mx-none">No memory usage recorded in this range.</div>}
        </div>
      </div>

      <div className="mx-grid">
        <TopApps title="Top memory consumers" apps={d.topApps} by="memory" platform={d.platform?.memory} />
        <TopApps title="Top CPU consumers · cores" apps={d.topApps} by="cpu" platform={d.platform?.cpu} />
      </div>

      {d.platform && d.platform.namespaces.length > 0 && (
        <div className="card" style={{ overflow: "hidden" }}>
          <div className="ch-h"><h3>Platform</h3><span className="dim" style={{ fontWeight: 400, fontSize: 12 }}>Kwerft, Kubernetes, ingress, monitoring and builds</span></div>
          <div className="scroll-x">
            <table className="t">
              <thead><tr><th>Namespace</th><th className="num">CPU, cores</th><th className="num">Memory</th></tr></thead>
              <tbody>
                {d.platform.namespaces.map((n) => (
                  <tr key={n.namespace}><td className="mono">{n.namespace}</td><td className="num">{formatCores(n.cpu)}</td><td className="num">{formatBytes(n.memory)}</td></tr>
                ))}
                <tr><td><b>Total</b></td><td className="num"><b>{formatCores(d.platform.cpu)}</b></td><td className="num"><b>{formatBytes(d.platform.memory)}</b></td></tr>
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  );
}

function NodeTile({ node: n }: { node: NodeUsage }) {
  return (
    <div className="mx-node">
      <b>{n.name}</b>
      <Meter label="CPU" used={n.cpu} cap={n.cpuCapacity} text={`${formatCores(n.cpu)} of ${formatCores(n.cpuCapacity)}`} />
      <Meter label="Memory" used={n.memory} cap={n.memoryCapacity} text={`${formatBytes(n.memory)} of ${formatBytes(n.memoryCapacity)}`} />
      <Meter label="Disk" used={n.disk} cap={n.diskCapacity} text={`${formatBytes(n.disk)} of ${formatBytes(n.diskCapacity)}`} />
    </div>
  );
}

function Meter({ label, used, cap, text }: { label: string; used: number; cap: number; text: string }) {
  const f = cap > 0 ? Math.min(1, used / cap) : 0;
  const level = f >= 0.9 ? "bad" : f >= 0.75 ? "warn" : "";
  return (
    <div className={`mx-meter ${level}`}>
      <span>{label}</span>
      <span className="tr" role="meter" aria-label={`${label} ${formatPercent(used, cap)} used`} aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.round(f * 100)}>
        <i style={{ width: `${f * 100}%` }} />
      </span>
      <span className="num" title={text}>{formatPercent(used, cap)}{level && <span className="mx-sr"> ({level === "bad" ? "critical" : "high"})</span>}</span>
    </div>
  );
}

function TopApps({ title, apps, by, platform }: { title: string; apps: AppUsage[]; by: "cpu" | "memory"; platform?: number }) {
  const fmt = by === "cpu" ? formatCores : formatBytes;
  const top = [...apps].sort((a, b) => b[by] - a[by]).filter((a) => a[by] > 0).slice(0, 8);
  const max = Math.max(platform ?? 0, ...top.map((a) => a[by]), Number.MIN_VALUE);
  return (
    <div className="card">
      <h3>{title}</h3>
      {top.length === 0 && platform === undefined ? <div className="mx-none">No app usage recorded yet.</div> : (
        <div className="mx-bars">
          {top.map((a) => (
            <div className="mx-row" key={`${a.project}/${a.app}`}>
              <span><Link to="/apps/$project/$name" params={{ project: a.project, name: a.app }} title={`${a.project}/${a.app}`}>{a.app}</Link> <span className="dim">{a.project}</span></span>
              <span className="tr" aria-hidden="true"><i style={{ width: `${(a[by] / max) * 100}%` }} /></span>
              <span className="num">{fmt(a[by])}</span>
            </div>
          ))}
          {platform !== undefined && (
            <div className="mx-row platform">
              <span className="dim">Kwerft platform</span>
              <span className="tr" aria-hidden="true"><i style={{ width: `${(platform / max) * 100}%` }} /></span>
              <span className="num">{fmt(platform)}</span>
            </div>
          )}
        </div>
      )}
    </div>
  );
}

const EXAMPLE = "sum by (namespace) (kwerft:container_memory_working_set_bytes)";

function Explorer({ range, cluster }: { range: RangeId; cluster: string }) {
  const [draft, setDraft] = useState("");
  const [query, setQuery] = useState("");
  const q = useQuery({
    queryKey: ["metrics-query", query, range, cluster],
    queryFn: () => metricsApi.query(query, range, cluster),
    enabled: query !== "",
    retry: false,
  });
  const run = (e: FormEvent) => {
    e.preventDefault();
    setQuery(draft.trim());
  };
  const shown = q.data?.series.slice(0, 20) ?? [];

  return (
    <div className="card mx-explore">
      <div className="ch-h"><h3>Explore</h3><span className="dim" style={{ fontWeight: 400, fontSize: 12 }}>PromQL / MetricsQL over every namespace and node</span></div>
      <form onSubmit={run}>
        <label htmlFor="mx-query" className="mx-sr">Query</label>
        <textarea id="mx-query" className="input" rows={1} spellCheck={false} placeholder={EXAMPLE} value={draft}
          onChange={(e) => setDraft(e.target.value)}
          onKeyDown={(e) => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); setQuery(draft.trim()); } }} />
        <button className="btn pri" disabled={!draft.trim() || q.isFetching}>{q.isFetching ? "Running…" : "Run"}</button>
      </form>
      <p className="hint">Recording rules: <code>kwerft:container_cpu_usage_cores:rate5m</code>, <code>kwerft:container_memory_working_set_bytes</code>, <code>kwerft:http_requests:rate5m</code>, <code>kwerft:http_latency_p95_seconds:5m</code>. Enter runs the query.</p>
      {q.isError && <div className="mx-none" role="alert">{errorText(q.error)}</div>}
      {q.data && q.data.series.length === 0 && <div className="mx-none">No series match this query in the last {RANGE_LABEL[range]}.</div>}
      {q.data && q.data.series.length > 0 && (
        <>
          <div className="bd" style={{ padding: "10px 14px 8px" }}>
            <Chart title={`Result of ${q.data.query}`} start={q.data.start} end={q.data.end} step={q.data.step} format={formatNumber}
              series={shown.map((s) => ({ label: seriesName(s.labels), points: s.points, tone: "accent" as const }))} />
          </div>
          <div className="series-list sep">
            <table className="t">
              <thead><tr><th>Series{q.data.series.length > shown.length ? ` (first ${shown.length} of ${q.data.series.length}${q.data.truncated ? "+" : ""})` : ""}</th><th className="num">Latest</th></tr></thead>
              <tbody>
                {q.data.series.map((s, i) => (
                  <tr key={i}><td>{seriesName(s.labels)}</td><td className="num">{s.points.length ? formatNumber(s.points[s.points.length - 1]!.v) : "—"}</td></tr>
                ))}
              </tbody>
            </table>
          </div>
        </>
      )}
    </div>
  );
}
