// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { useState, type ReactNode } from "react";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { ApiError } from "../api";
import { Chart, Legend, type ChartSeries } from "../components/Chart";
import {
  RANGES, RANGE_LABEL, formatBytes, formatCores, formatCount, formatRate, formatSeconds, metricsApi, summarize, total,
  type MaybeSeries, type RangeId,
} from "../metrics";
import type { App } from "../workloads";
import { errorText } from "./Apps";
import "../styles/metrics.css";

// App detail › Metrics: CPU, memory against the limit, restarts, requests
// and errors through Traefik, p95 latency. Charts come from the catalog in
// internal/metrics; the console never sends PromQL for them.

export function RangePicker({ value, onChange }: { value: RangeId; onChange: (r: RangeId) => void }) {
  return (
    <div className="seg" role="group" aria-label="Time range">
      {RANGES.map((r) => (
        <button key={r} type="button" aria-pressed={value === r} title={`Last ${RANGE_LABEL[r]}`} onClick={() => onChange(r)}>{r}</button>
      ))}
    </div>
  );
}

export function AppMetrics({ app }: { app: App }) {
  const project = app.metadata.namespace;
  const name = app.metadata.name;
  const [range, setRange] = useState<RangeId>("1h");
  const q = useQuery({
    queryKey: ["app-metrics", project, name, range],
    queryFn: () => metricsApi.app(project, name, range),
    refetchInterval: 30_000,
    placeholderData: keepPreviousData,
  });
  const publicPorts = (app.spec.ports ?? []).some((p) => p.public);

  const toolbar = (
    <div className="mx-toolbar">
      <RangePicker value={range} onChange={setRange} />
      <span className="dim">{q.isFetching ? "Updating…" : `Last ${RANGE_LABEL[range]} · every ${q.data ? stepText(q.data.step) : "—"}`}</span>
    </div>
  );

  if (q.isPending) return <>{toolbar}<p className="loading">Loading metrics…</p></>;
  if (q.isError) {
    const down = q.error instanceof ApiError && q.error.status === 503;
    return (
      <>
        {toolbar}
        <div className="empty">
          <h2>{down ? "Metrics are unavailable" : "Could not load metrics"}</h2>
          <p>{errorText(q.error)}</p>
        </div>
      </>
    );
  }

  const d = q.data;
  const nothing = [d.cpu, d.memory, d.restarts, d.requests].every((s) => !s);
  if (nothing) {
    return (
      <>
        {toolbar}
        <div className="empty">
          <h2>No metrics yet</h2>
          <p>Nothing was recorded for {name} in the last {RANGE_LABEL[range]}. Usage shows up a minute or two after a replica starts; a stopped app has none.</p>
        </div>
      </>
    );
  }

  const win = { start: d.start, end: d.end, step: d.step };
  const mem = summarize(d.memory);
  const limit = summarize(d.memoryLimit);
  const restarts = total(d.restarts);
  const errors = summarize(d.errors);
  const requests = summarize(d.requests);
  const series = (label: string, s: MaybeSeries, extra: Partial<ChartSeries> = {}): ChartSeries[] => (s ? [{ label, points: s, ...extra }] : []);

  return (
    <div className={q.isPlaceholderData ? "mx-stale" : undefined}>
      {toolbar}
      <div className="mx-grid">
        <Card title="CPU" unit="cores" now={d.cpu && formatCores(summarize(d.cpu)!.latest)}
          empty={!d.cpu && "No CPU usage recorded in this range."}>
          <Chart title="CPU in cores, all replicas" {...win} format={formatCores} series={series("CPU", d.cpu, { area: true })} />
        </Card>

        <Card title="Memory" unit={limit ? "against the limit" : undefined}
          now={mem && `${formatBytes(mem.latest)}${limit ? ` of ${formatBytes(limit.latest)}` : ""}`}
          legend={d.memoryLimit && <Legend items={[{ label: "working set", tone: "accent" }, { label: "limit", tone: "warn", dashed: true }]} />}
          empty={!d.memory && "No memory usage recorded in this range."}>
          <Chart title="Memory, all replicas, against their limit" {...win} format={formatBytes} bytes
            series={[...series("working set", d.memory, { area: true }), ...series("limit", d.memoryLimit, { tone: "warn", dashed: true })]} />
        </Card>

        <Card title="Restarts" now={d.restarts ? `${restarts} in ${range}` : undefined}
          empty={!d.restarts && "No replica reported in this range."}>
          <Chart title={`Container restarts per ${stepText(d.step)}`} {...win} height={140} minMax={2} format={formatCount}
            series={series("restarts", d.restarts, { tone: restarts > 0 ? "bad" : "accent", area: true })} />
        </Card>

        <Card title="Response time" unit="95th percentile" now={summarize(d.latencyP95) ? formatSeconds(summarize(d.latencyP95)!.latest) : undefined}
          empty={!d.latencyP95 && noTraffic(publicPorts)}>
          <Chart title="Response time, 95th percentile" {...win} height={140} format={formatSeconds}
            series={series("p95", d.latencyP95)} />
        </Card>

        <Card wide title="Requests" unit="per second, through the public URL"
          now={requests && `${formatRate(requests.latest)}${errors && errors.max > 0 ? ` · ${formatRate(errors.latest)} errors` : ""}`}
          legend={d.requests && <Legend items={[{ label: "all requests", tone: "accent" }, { label: "5xx errors", tone: "bad" }]} />}
          empty={!d.requests && noTraffic(publicPorts)}>
          <Chart title="Requests per second and 5xx errors" {...win} format={formatRate}
            series={[...series("all requests", d.requests, { area: true }), ...series("5xx errors", d.errors, { tone: "bad" })]} />
        </Card>
      </div>
    </div>
  );
}

function noTraffic(publicPorts: boolean) {
  return publicPorts
    ? "No requests reached this app through its public URL in this range."
    : "This app has no public port. Requests are counted where Traefik routes them, so traffic inside the cluster does not show here.";
}

export function stepText(s: number) {
  if (s % 3600 === 0) return `${s / 3600} h`;
  if (s % 60 === 0) return `${s / 60} min`;
  return `${s} s`;
}

function Card({ title, unit, now, legend, empty, wide, children }: {
  title: string; unit?: string; now?: ReactNode; legend?: ReactNode; empty?: string | false | null; wide?: boolean; children: ReactNode;
}) {
  return (
    <div className={`card mx-card${wide ? " wide" : ""}`}>
      <div className="ch-h">
        <h3>{title}{unit && <span className="dim" style={{ fontWeight: 400 }}> · {unit}</span>}</h3>
        {legend ?? (now && <span className="now">{now}</span>)}
      </div>
      {legend && now && <div className="bd" style={{ paddingBottom: 0 }}><span className="now mono">{now}</span></div>}
      {empty ? <div className="mx-none">{empty}</div> : <div className="bd">{children}</div>}
    </div>
  );
}
