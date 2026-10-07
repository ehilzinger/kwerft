// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Metrics API (internal/server/api_metrics.go) and the pure helpers behind
// the charts: number formats, axis ticks, gaps. Values come from
// VictoriaMetrics; points are [unix seconds, value].

import { request } from "./api";
import { clusterQuery } from "./clusters";

export type Point = { t: number; v: number };
/** null: no data for this chart in the range. */
export type MaybeSeries = Point[] | null;

export const RANGES = ["1h", "6h", "24h", "7d"] as const;
export type RangeId = (typeof RANGES)[number];
export const RANGE_LABEL: Record<RangeId, string> = { "1h": "1 hour", "6h": "6 hours", "24h": "24 hours", "7d": "7 days" };

type Window = { range: string; step: number; start: number; end: number };

export type AppMetrics = Window & {
  cpu: MaybeSeries; // cores
  memory: MaybeSeries; // bytes
  memoryLimit: MaybeSeries; // bytes, all replicas
  restarts: MaybeSeries; // per step
  requests: MaybeSeries; // per second
  errors: MaybeSeries; // 5xx per second
  latencyP95: MaybeSeries; // seconds
};

export type NodeUsage = {
  name: string;
  cpu: number;
  cpuCapacity: number;
  memory: number;
  memoryCapacity: number;
  disk: number;
  diskCapacity: number;
};
export type AppUsage = { project: string; app: string; cpu: number; memory: number };
export type MetricsOverview = Window & {
  /** all: owners and admins (nodes, platform); projects: confined. */
  scope: "all" | "projects";
  nodes: NodeUsage[];
  platform: { cpu: number; memory: number; namespaces: { namespace: string; cpu: number; memory: number }[] } | null;
  topApps: AppUsage[];
  series: { cpu: MaybeSeries; memory: MaybeSeries };
};

export type ExploreSeries = { labels: Record<string, string>; points: Point[] };
export type ExploreResult = Window & { query: string; scope: "all" | "projects"; series: ExploreSeries[]; truncated: boolean };

const enc = encodeURIComponent;

export const metricsApi = {
  app: (project: string, app: string, range: RangeId) =>
    request<AppMetrics>(`/projects/${enc(project)}/apps/${enc(app)}/metrics?range=${range}`),
  // Each cluster has its own VictoriaMetrics (Phase 5): cluster picks one; the default is the console's own.
  overview: (range: RangeId, cluster?: string) => request<MetricsOverview>(`/metrics/overview?range=${range}${clusterQuery(cluster, "&")}`),
  query: (query: string, range: RangeId, cluster?: string) =>
    request<ExploreResult>(`/metrics/query?range=${range}&query=${enc(query)}${clusterQuery(cluster, "&")}`),
};

// ---- formats ---------------------------------------------------------------

function trim(n: number, digits: number) {
  return n.toFixed(digits).replace(/\.0+$|(\.\d*[1-9])0+$/, "$1");
}

/** 1536 → "1.5 KiB", 842e6 → "803 MiB". */
export function formatBytes(v: number): string {
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  let n = Math.abs(v);
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  const s = n >= 100 || i === 0 ? n.toFixed(0) : trim(n, n >= 10 ? 1 : 2);
  return `${v < 0 ? "-" : ""}${s} ${units[i]}`;
}

/** CPU cores: 0.004 → "4m", 0.25 → "0.25", 3 → "3". */
export function formatCores(v: number): string {
  if (v === 0) return "0";
  if (Math.abs(v) < 0.1) return `${trim(v * 1000, Math.abs(v) < 0.01 ? 1 : 0)}m`;
  return trim(v, Math.abs(v) < 10 ? 2 : 1);
}

/** Requests per second: "0.03/s", "12/s", "1.2k/s". */
export function formatRate(v: number): string {
  if (v === 0) return "0/s";
  if (Math.abs(v) >= 1000) return `${trim(v / 1000, 1)}k/s`;
  return `${trim(v, Math.abs(v) < 1 ? 2 : Math.abs(v) < 10 ? 1 : 0)}/s`;
}

/** Seconds as a duration: 0.18 → "180 ms", 2.5 → "2.5 s". */
export function formatSeconds(v: number): string {
  if (v < 1) return `${trim(v * 1000, v < 0.01 ? 1 : 0)} ms`;
  return `${trim(v, 1)} s`;
}

export function formatCount(v: number): string {
  return Math.abs(v) >= 10 || Number.isInteger(v) ? Math.round(v).toString() : trim(v, 1);
}

export function formatPercent(part: number, whole: number): string {
  if (!whole) return "—";
  const p = (part / whole) * 100;
  return `${p < 10 ? trim(p, 1) : Math.round(p)}%`;
}

/** A plain number for the explorer: compact, no unit. */
export function formatNumber(v: number): string {
  const a = Math.abs(v);
  if (a === 0) return "0";
  if (a >= 1e9) return `${trim(v / 1e9, 2)}G`;
  if (a >= 1e6) return `${trim(v / 1e6, 2)}M`;
  if (a >= 1e4) return `${trim(v / 1e3, 1)}k`;
  if (a >= 1) return trim(v, 2);
  return v.toPrecision(2).replace(/\.?0+(e|$)/, "$1");
}

/** Clock time for an axis: HH:MM within a day, "Mon 14" beyond. */
export function formatTime(t: number, spanSeconds: number): string {
  const d = new Date(t * 1000);
  if (spanSeconds <= 26 * 3600) return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit" });
  return d.toLocaleDateString(undefined, { weekday: "short", day: "numeric" });
}

export function formatDateTime(t: number): string {
  return new Date(t * 1000).toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

// ---- chart geometry ------------------------------------------------------------

/**
 * Axis ticks from 0 to a "nice" maximum at or above max: steps of 1, 2, 2.5
 * or 5 × 10^n. Byte axes step in powers of 1024 so labels read 256 MiB, 512 MiB.
 */
export function niceTicks(max: number, count = 4, bytes = false): number[] {
  if (!(max > 0)) return [0, 1];
  const raw = max / count;
  let step: number;
  if (bytes && raw >= 1024) {
    const unit = 1024 ** Math.floor(Math.log(raw) / Math.log(1024));
    step = niceStep(raw / unit, [1, 2, 4, 8, 16, 32, 64, 128, 256, 512]) * unit;
  } else {
    const mag = 10 ** Math.floor(Math.log10(raw));
    step = niceStep(raw / mag, [1, 2, 2.5, 5, 10]) * mag;
  }
  const ticks: number[] = [];
  for (let v = 0; v < max + step * 1e-9; v += step) ticks.push(Number(v.toPrecision(12)));
  if (ticks[ticks.length - 1]! < max) ticks.push(Number((ticks.length * step).toPrecision(12)));
  return ticks;
}

function niceStep(x: number, steps: number[]) {
  return steps.find((s) => s >= x - 1e-9) ?? steps[steps.length - 1]!;
}

/** Splits a series where samples are missing (a gap longer than 1.5 steps). */
export function segments(points: Point[], step: number): Point[][] {
  const out: Point[][] = [];
  let cur: Point[] = [];
  for (const p of points) {
    const prev = cur[cur.length - 1];
    if (prev && p.t - prev.t > step * 1.5) {
      out.push(cur);
      cur = [];
    }
    cur.push(p);
  }
  if (cur.length) out.push(cur);
  return out;
}

export type Summary = { latest: number; min: number; max: number; mean: number };

export function summarize(points: Point[] | null | undefined): Summary | null {
  if (!points?.length) return null;
  let min = Infinity;
  let max = -Infinity;
  let sum = 0;
  for (const p of points) {
    min = Math.min(min, p.v);
    max = Math.max(max, p.v);
    sum += p.v;
  }
  return { latest: points[points.length - 1]!.v, min, max, mean: sum / points.length };
}

/** The sample nearest to t (points sorted by time). */
export function nearest(points: Point[], t: number): Point | undefined {
  let lo = 0;
  let hi = points.length - 1;
  if (hi < 0) return undefined;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    if (points[mid]!.t < t) lo = mid + 1;
    else hi = mid;
  }
  const a = points[lo]!;
  const b = points[lo - 1];
  return b && Math.abs(b.t - t) <= Math.abs(a.t - t) ? b : a;
}

/** {__name__="up", job="x"} for the explorer's legend; the metric name leads. */
export function seriesName(labels: Record<string, string>): string {
  const { __name__: name = "", ...rest } = labels;
  const inner = Object.keys(rest).sort().map((k) => `${k}="${rest[k]}"`).join(", ");
  return inner ? `${name}{${inner}}` : name || "{}";
}

/** Total restarts in a restarts series (one value per step). */
export function total(points: MaybeSeries): number {
  return Math.round((points ?? []).reduce((s, p) => s + p.v, 0));
}
