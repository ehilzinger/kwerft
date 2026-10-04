// Log search over VictoriaLogs (GET /api/v1/logs, /api/v1/logs/tail; see
// internal/server/api_logsearch.go). The server confines every query to the
// projects the user may read; owners and admins may add the platform's
// namespaces. The query is LogsQL filters only (no pipes): words, phrases
// in quotes, field:value (pod:web-1, stream:stderr, log.status:500).
import { ApiError, request } from "./api";
import { stripAnsi } from "./builds";

export type LogEntry = {
  time: string;
  namespace: string;
  project?: string;
  app?: string;
  build?: string;
  task?: string;
  pod: string;
  container: string;
  stream?: string;
  level?: string;
  line: string;
  truncated?: boolean;
};

export type SearchResult = { entries: LogEntry[]; truncated: boolean; from: string; to: string };

export type Level = "" | "warn" | "error";

export type LogFilter = {
  query?: string;
  project?: string;
  app?: string;
  level?: Level;
  /** Seconds or a Go duration ago ("15m", "24h"), or an RFC 3339 time. */
  since?: string;
  until?: string;
  limit?: number;
  /** Owners and admins: every namespace, not just projects. */
  platform?: boolean;
  /** Whose VictoriaLogs to search without a project (a project names its own); default the console's cluster. */
  cluster?: string;
};

export const RANGES = [
  { id: "15m", label: "15 m" },
  { id: "1h", label: "1 h" },
  { id: "6h", label: "6 h" },
  { id: "24h", label: "24 h" },
  { id: "168h", label: "7 d" },
  { id: "336h", label: "14 d" },
] as const;
export type RangeId = (typeof RANGES)[number]["id"];
export const isRange = (v: unknown): v is RangeId => RANGES.some((r) => r.id === v);

export const LEVELS: { id: Level; label: string }[] = [
  { id: "", label: "All" },
  { id: "warn", label: "Warnings" },
  { id: "error", label: "Errors" },
];
export const isLevel = (v: unknown): v is Level => LEVELS.some((l) => l.id === v);

/** The query string for a filter; empty values are left out. */
export function logParams(f: LogFilter): string {
  const q = new URLSearchParams();
  const set = (k: string, v: string | number | boolean | undefined) => {
    if (v === undefined || v === "" || v === false) return;
    q.set(k, v === true ? "1" : String(v));
  };
  set("query", f.query?.trim());
  set("project", f.project);
  set("app", f.project ? f.app : undefined); // an app needs its project
  set("level", f.level);
  set("since", f.since);
  set("until", f.until);
  set("limit", f.limit);
  set("platform", f.project ? undefined : f.platform);
  set("cluster", f.project || f.cluster === "local" ? undefined : f.cluster);
  return q.toString();
}

export const logSearch = {
  search: (f: LogFilter) => {
    const q = logParams(f);
    return request<SearchResult>(`/logs${q ? `?${q}` : ""}`).then((r) => ({ ...r, entries: r.entries.map(clean) }));
  },
};

function clean(e: LogEntry): LogEntry {
  return { ...e, line: stripAnsi(e.line) };
}

/** A line's identity, to drop the lines a page boundary or the tail repeats. */
export const entryKey = (e: LogEntry) => `${e.time}\u0000${e.namespace}\u0000${e.pod}\u0000${e.container}\u0000${e.line}`;

/** Merges lines into one list, oldest first, without duplicates; keeps the newest max. */
export function mergeEntries(a: LogEntry[], b: LogEntry[], max = Infinity): LogEntry[] {
  const seen = new Set<string>();
  const out: LogEntry[] = [];
  for (const e of [...a, ...b]) {
    const k = entryKey(e);
    if (seen.has(k)) continue;
    seen.add(k);
    out.push(e);
  }
  // Stable: lines with the same time keep their order.
  out.sort((x, y) => (x.time < y.time ? -1 : x.time > y.time ? 1 : 0));
  return out.length > max ? out.slice(out.length - max) : out;
}

/** Where the source of a line is shown: project/app, the build or task, or the namespace. */
export function sourceOf(e: LogEntry): string {
  const p = e.project ?? e.namespace;
  if (e.app) return `${p}/${e.app}`;
  if (e.build) return `${p} · build ${e.build}`;
  if (e.task) return `${p} · task ${e.task}`;
  return e.project ? p : e.namespace;
}

// ---- live tail ----------------------------------------------------------------

export type TailEvent =
  | { type: "start" }
  | { type: "line"; entry: LogEntry }
  | { type: "dropped"; lines: number }
  | { type: "end"; reason: "idle" | "maxDuration" | "signedOut" | "error"; message?: string };

/** Splits a Server-Sent Events buffer into complete events and what is left. */
export function parseSSE(buf: string): { events: { name: string; data: string }[]; rest: string } {
  const events: { name: string; data: string }[] = [];
  let cut: number;
  while ((cut = buf.indexOf("\n\n")) >= 0) {
    const block = buf.slice(0, cut);
    buf = buf.slice(cut + 2);
    let name = "";
    let data = "";
    for (const line of block.split("\n")) {
      if (line.startsWith("event: ")) name = line.slice(7);
      else if (line.startsWith("data: ")) data += line.slice(6);
    }
    if (name && data) events.push({ name, data }); // comments (": ping") have neither
  }
  return { events, rest: buf };
}

/**
 * tailLogs follows new lines matching the filter (its time range is
 * ignored). An error before the stream is an ApiError; it resolves when the
 * stream ends.
 */
export async function tailLogs(f: LogFilter, onEvent: (e: TailEvent) => void, signal: AbortSignal): Promise<void> {
  const q = logParams({ ...f, since: undefined, until: undefined, limit: undefined });
  const res = await fetch(`/api/v1/logs/tail${q ? `?${q}` : ""}`, { headers: { Accept: "text/event-stream" }, credentials: "same-origin", signal });
  if (!res.ok || !res.body) {
    let message = `${res.status} ${res.statusText}`;
    let field: string | undefined;
    try {
      const body = (await res.json()) as { error?: string; field?: string };
      message = body.error ?? message;
      field = body.field;
    } catch {
      /* not JSON */
    }
    throw new ApiError(res.status, message, field);
  }
  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
  let buf = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) return;
    const parsed = parseSSE(buf + value);
    buf = parsed.rest;
    for (const ev of parsed.events) {
      const payload = JSON.parse(ev.data);
      if (ev.name === "line") onEvent({ type: "line", entry: clean(payload as LogEntry) });
      else onEvent({ type: ev.name, ...payload } as TailEvent);
    }
  }
}
