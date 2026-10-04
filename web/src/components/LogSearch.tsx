import { useEffect, useLayoutEffect, useMemo, useRef, useState, type FormEvent } from "react";
import { useQuery } from "@tanstack/react-query";
import { ApiError } from "../api";
import {
  LEVELS, RANGES, logSearch, mergeEntries, sourceOf, tailLogs,
  type Level, type LogEntry, type LogFilter, type RangeId, type SearchResult,
} from "../logsearch";
import { shortPod } from "../pods";
import { workloads } from "../workloads";
import { highlight, Toggle } from "./LogViewer";
import { Icon } from "./Icon";
import "../styles/pods.css";
import "../styles/logsearch.css";

// LogSearch searches the log history in VictoriaLogs: every container's
// lines, also of pods that are gone. Monitoring › Logs uses it with project
// and app pickers; the App detail Logs tab with its app fixed.

export type SearchState = { query: string; project: string; app: string; level: Level; range: RangeId; platform: boolean };

export const defaultSearch: SearchState = { query: "", project: "", app: "", level: "", range: "1h", platform: false };

type Props = {
  /** Fixed project and app (App detail): no pickers. */
  fixed?: { project: string; app: string };
  /** Starting filter; Monitoring keeps it in the URL. */
  initial?: Partial<SearchState>;
  onChange?: (s: SearchState) => void;
  /** Owners and admins may include the platform's namespaces. */
  canPlatform?: boolean;
  height?: string;
};

const PAGE = 200;
const MAX = 5000; // lines kept in the browser while tailing

type TailPhase = { phase: "off" } | { phase: "connecting" } | { phase: "live" } | { phase: "ended"; message: string } | { phase: "error"; message: string };

export function LogSearch({ fixed, initial, onChange, canPlatform = false, height = "min(62vh, 640px)" }: Props) {
  const [s, setS] = useState<SearchState>({ ...defaultSearch, ...initial, ...(fixed ?? {}) });
  const [draft, setDraft] = useState(s.query);
  const update = (patch: Partial<SearchState>) => {
    const next = { ...s, ...patch };
    if (patch.project !== undefined && patch.project !== s.project) next.app = "";
    setS(next);
    onChange?.(next);
  };
  const submit = (e: FormEvent) => {
    e.preventDefault();
    update({ query: draft.trim() });
  };

  const projects = useQuery({ queryKey: ["projects"], queryFn: workloads.projects, enabled: !fixed });
  const apps = useQuery({ queryKey: ["logsearch-apps", s.project], queryFn: () => workloads.apps(s.project), enabled: !fixed && s.project !== "" });

  const filter: LogFilter = useMemo(
    () => ({ query: s.query, project: s.project || undefined, app: s.app || undefined, level: s.level, platform: s.platform && !s.project, since: s.range, limit: PAGE }),
    [s],
  );
  const first = useQuery({
    queryKey: ["logsearch", filter],
    queryFn: () => logSearch.search(filter),
    retry: false,
    refetchOnWindowFocus: false,
  });

  // Older pages, loaded on demand; reset with the filter.
  const [older, setOlder] = useState<{ entries: LogEntry[]; truncated: boolean }>();
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [olderError, setOlderError] = useState<string>();
  useEffect(() => { setOlder(undefined); setOlderError(undefined); }, [filter]);

  // Live tail.
  const [live, setLive] = useState(false);
  const [tail, setTail] = useState<LogEntry[]>([]);
  const [tailPhase, setTailPhase] = useState<TailPhase>({ phase: "off" });
  const [dropped, setDropped] = useState(0);
  const [attempt, setAttempt] = useState(0);
  useEffect(() => {
    setTail([]);
    setDropped(0);
    if (!live) {
      setTailPhase({ phase: "off" });
      return;
    }
    const ctrl = new AbortController();
    let pending: LogEntry[] = [];
    let timer = 0;
    const flush = () => {
      timer = 0;
      if (pending.length === 0) return;
      const add = pending;
      pending = [];
      setTail((t) => mergeEntries(t, add, MAX));
    };
    setTailPhase({ phase: "connecting" });
    tailLogs(filter, (e) => {
      switch (e.type) {
        case "start":
          setTailPhase({ phase: "live" });
          break;
        case "line":
          pending.push(e.entry);
          if (!timer) timer = window.setTimeout(flush, 150);
          break;
        case "dropped":
          setDropped((d) => d + e.lines);
          break;
        case "end":
          flush();
          setTailPhase({ phase: "ended", message: e.message ?? "The live stream stopped." });
          break;
      }
    }, ctrl.signal)
      .then(() => setTailPhase((p) => (p.phase === "ended" ? p : { phase: "ended", message: "The connection closed." })))
      .catch((err: unknown) => {
        if (!ctrl.signal.aborted) setTailPhase({ phase: "error", message: err instanceof Error ? err.message : String(err) });
      });
    return () => {
      ctrl.abort();
      window.clearTimeout(timer);
    };
  }, [live, filter, attempt]);

  const page: SearchResult | undefined = first.data;
  const entries = useMemo(
    () => mergeEntries(mergeEntries(older?.entries ?? [], page?.entries ?? []), tail, MAX),
    [older, page, tail],
  );
  const moreOlder = older ? older.truncated : page?.truncated ?? false;

  const loadOlder = async () => {
    if (!page || entries.length === 0) return;
    setLoadingOlder(true);
    setOlderError(undefined);
    try {
      const res = await logSearch.search({ ...filter, since: page.from, until: entries[0]!.time });
      // Keep the lines in view where they are while older ones go above.
      const el = box.current;
      if (el) anchor.current = el.scrollHeight - el.scrollTop;
      stick.current = false;
      setOlder((o) => ({ entries: mergeEntries(res.entries, o?.entries ?? []), truncated: res.truncated }));
    } catch (e) {
      setOlderError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoadingOlder(false);
    }
  };

  // Newest at the bottom; stay there while tailing unless scrolled up.
  const box = useRef<HTMLDivElement>(null);
  const stick = useRef(true);
  const anchor = useRef<number>(undefined); // distance from the bottom to keep after loading older lines
  const onScroll = () => {
    const el = box.current;
    if (el) stick.current = el.scrollTop + el.clientHeight >= el.scrollHeight - 30;
  };
  useLayoutEffect(() => {
    const el = box.current;
    if (!el) return;
    if (anchor.current !== undefined) {
      el.scrollTop = el.scrollHeight - anchor.current;
      anchor.current = undefined;
    } else if (stick.current) {
      el.scrollTop = el.scrollHeight;
    }
  }, [entries]);
  useEffect(() => { stick.current = true; }, [filter]);

  const needle = s.query && !/[:"'()*]/.test(s.query) ? s.query.toLowerCase() : "";
  const multiDay = s.range === "168h" || s.range === "336h";
  const showSource = !fixed;
  const unavailable = first.error instanceof ApiError && first.error.status === 503;

  return (
    <div className="logsearchv">
      <form className="toolbar" role="search" aria-label="Search logs" onSubmit={submit}>
        <input className="input mono q" type="search" value={draft} onChange={(e) => setDraft(e.target.value)}
          placeholder={'Words or LogsQL: timeout, "connection refused", stream:stderr'} aria-label="Search text or LogsQL filter"
          aria-invalid={first.error instanceof ApiError && first.error.field === "query"} />
        {!fixed && (
          <>
            <select className="input" aria-label="Project" value={s.project} onChange={(e) => update({ project: e.target.value })}>
              <option value="">{s.platform ? "All namespaces" : "All projects"}</option>
              {s.project && !projects.data?.some((p) => p.name === s.project) && <option value={s.project}>{s.project}</option>}
              {projects.data?.map((p) => <option key={p.name} value={p.name}>{p.displayName || p.name}</option>)}
            </select>
            <select className="input" aria-label="App" value={s.app} disabled={!s.project} onChange={(e) => update({ app: e.target.value })}>
              <option value="">All apps</option>
              {s.app && !apps.data?.some((a) => a.name === s.app) && <option value={s.app}>{s.app}</option>}
              {apps.data?.map((a) => <option key={a.name} value={a.name}>{a.name}</option>)}
            </select>
          </>
        )}
        <button type="submit" className="btn">Search</button>
        <div className="seg" role="group" aria-label="Level">
          {LEVELS.map((l) => <button key={l.id} type="button" aria-pressed={s.level === l.id} onClick={() => update({ level: l.id })}>{l.label}</button>)}
        </div>
        <div className="seg" role="group" aria-label="Time range">
          {RANGES.map((r) => <button key={r.id} type="button" aria-pressed={s.range === r.id} onClick={() => update({ range: r.id })}>{r.label}</button>)}
        </div>
        {canPlatform && !fixed && (
          <Toggle on={s.platform} onChange={(v) => update({ platform: v })} disabled={!!s.project} title="Include kube-system, kwerft-system, build pods and the other platform namespaces">Platform</Toggle>
        )}
        <Toggle on={live} onChange={setLive} className="push">Live</Toggle>
      </form>

      {unavailable && (
        <div className="banner warn" role="alert"><Icon name="alert" /><span>{first.error?.message}</span>
          <button type="button" className="btn sm" onClick={() => void first.refetch()}>Retry</button></div>
      )}

      <div className="logpanel">
        <div className="logv logrows" ref={box} onScroll={onScroll} style={{ height }} tabIndex={0} role="log" aria-label="Log lines" aria-busy={first.isFetching}>
          {moreOlder && entries.length > 0 && (
            <div className="older">
              <button type="button" className="btn sm" onClick={() => void loadOlder()} disabled={loadingOlder}>{loadingOlder ? "Loading…" : "Load older lines"}</button>
              {olderError && <span className="err">{olderError}</span>}
            </div>
          )}
          {entries.length === 0 ? (
            <div className="logempty">{emptyText(first.isPending, first.error, live, RANGES.find((r) => r.id === s.range)!.label)}</div>
          ) : (
            <table className="logt">
              <caption className="vh">Log lines, oldest first</caption>
              <thead className="vh">
                <tr><th scope="col">Time</th>{showSource && <th scope="col">Source</th>}<th scope="col">Pod</th><th scope="col">Line</th></tr>
              </thead>
              <tbody>
                {entries.map((e, i) => (
                  <tr key={i + ":" + e.time}>
                    <td className="ts" title={e.time}>{when(e.time, multiDay)}</td>
                    {showSource && <td className="src" title={e.namespace}>{sourceOf(e)}</td>}
                    <td className="pod" title={`${e.pod} · ${e.container}${e.stream ? " · " + e.stream : ""}`}>{shortPod(e.pod)}</td>
                    <td className={e.stream === "stderr" ? "ln err-stream" : "ln"}>
                      {highlight(e.line, needle)}
                      {e.truncated && <span className="cut"> … (line cut)</span>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
        <div className="logfoot" role="status">
          <span>{entries.length.toLocaleString()} line{entries.length === 1 ? "" : "s"}{page ? ` · from log history, kept 14 days` : ""}</span>
          {tailPhase.phase === "connecting" && <span>— connecting live… —</span>}
          {tailPhase.phase === "live" && <span>— live —</span>}
          {(tailPhase.phase === "ended" || tailPhase.phase === "error") && (
            <><span className={tailPhase.phase === "error" ? "err" : undefined}>{tailPhase.message}</span>
              <button type="button" className="btn sm" onClick={() => setAttempt((a) => a + 1)}>Resume</button></>
          )}
          {dropped > 0 && <span className="wn">{dropped.toLocaleString()} live lines skipped: more than the stream's rate limit. Narrow the search.</span>}
          {first.error && !unavailable && <span className="err">{first.error.message}</span>}
        </div>
      </div>
    </div>
  );
}

function emptyText(pending: boolean, error: Error | null, live: boolean, range: string) {
  if (pending) return "Searching…";
  if (error) return "No results.";
  return live ? `No lines in the last ${range}. New lines appear here.` : `No lines match in the last ${range}.`;
}

/** Local time; with the date for multi-day ranges. */
function when(iso: string, withDate: boolean) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const p = (n: number, w = 2) => String(n).padStart(w, "0");
  const t = `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`;
  return withDate ? `${d.toLocaleDateString(undefined, { month: "short", day: "numeric" })} ${t}` : t;
}
