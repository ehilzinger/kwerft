import { useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { stripAnsi } from "../builds";
import { shortPod, streamLogs, type LogLine, type LogQuery, type Replica } from "../pods";
import { Icon } from "./Icon";
import "../styles/pods.css";

// LogViewer streams the logs of an App or a Task from a log endpoint
// (appLogsPath / taskLogsPath in pods.ts) and shows them the way the
// blueprint's Logs tab does: replica selector, search with highlighting, time
// range, follow, timestamps, download of what is loaded.
//
// Reuse for a Task:
//   <LogViewer path={taskLogsPath(project, task)} replicas={pods} follow={running} downloadName={task} />
// and for a build, which has one pod and is read from its start:
//   <LogViewer path={buildLogsPath(project, build)} single follow={running} downloadName={...} />

type Props = {
  /** Log endpoint below /api/v1, e.g. appLogsPath(project, app). */
  path: string;
  /** Replicas for the selector (from the pods endpoint); without them, the pods the stream reports. */
  replicas?: Pick<Replica, "name" | "restarts">[];
  /** Replica to show first; "" or undefined for all of them. */
  pod?: string;
  /** Follow new lines (default true). Pass false for something that has finished. */
  follow?: boolean;
  /** File name stem for downloads. */
  downloadName: string;
  /** Height of the log area (CSS). */
  height?: string;
  /** One pod read from its start (a build): no replica picker, no time range. */
  single?: boolean;
  /** What an empty log says while following, e.g. "Waiting for the build to start…". */
  waiting?: string;
};

const ROW = 20; // px; matches .logv .row
const MAX_LINES = 20_000; // kept in the browser; older lines leave the view
const TAIL = 5000; // per replica, the server's maximum
const RANGES = [
  { id: "15m", label: "15 m", since: 15 * 60 },
  { id: "1h", label: "1 h", since: 60 * 60 },
  { id: "24h", label: "24 h", since: 24 * 60 * 60 },
] as const;
type RangeId = (typeof RANGES)[number]["id"];

type Phase =
  | { phase: "connecting" }
  | { phase: "live" }
  | { phase: "ended"; reason: string; message?: string }
  | { phase: "error"; message: string };

type PodState = { state: "streaming" | "waiting" | "ended" | "error"; message?: string };

export function LogViewer({ path, replicas, pod: initialPod, follow: initialFollow = true, downloadName, height = "min(62vh, 640px)", single = false, waiting }: Props) {
  const [pod, setPod] = useState(initialPod ?? "");
  useEffect(() => setPod(initialPod ?? ""), [initialPod]);
  const [range, setRange] = useState<RangeId>("1h");
  const [follow, setFollow] = useState(initialFollow);
  useEffect(() => setFollow(initialFollow), [initialFollow]);
  const [timestamps, setTimestamps] = useState(true);
  const [previous, setPrevious] = useState(false);
  const [search, setSearch] = useState("");
  const [attempt, setAttempt] = useState(0);

  const [phase, setPhase] = useState<Phase>({ phase: "connecting" });
  const [streamPods, setStreamPods] = useState<string[]>([]);
  const [pods, setPods] = useState<Record<string, PodState>>({});
  const [dropped, setDropped] = useState(0);
  // Set when the server answered from log history (the pods are gone).
  const [history, setHistory] = useState<string>();
  const lines = useRef<LogLine[]>([]);
  const trimmed = useRef(0);
  const [version, setVersion] = useState(0);

  const selected = replicas?.find((r) => r.name === pod);
  const canPrevious = pod !== "" && (selected?.restarts ?? 0) > 0;
  const usePrevious = previous && canPrevious;
  const live = follow && !usePrevious;

  // One stream per choice of replica, range, follow and previous.
  useEffect(() => {
    const ctrl = new AbortController();
    lines.current = [];
    trimmed.current = 0;
    setVersion((v) => v + 1);
    setPods({});
    setDropped(0);
    setHistory(undefined);
    setPhase({ phase: "connecting" });

    let pending: LogLine[] = [];
    let timer = 0;
    const flush = () => {
      timer = 0;
      if (pending.length === 0) return;
      const all = lines.current;
      all.push(...pending);
      pending = [];
      if (all.length > MAX_LINES) {
        const n = all.length - MAX_LINES;
        all.splice(0, n);
        trimmed.current += n;
      }
      setVersion((v) => v + 1);
    };
    const query: LogQuery = { pod: pod || undefined, follow: live, tail: TAIL, previous: usePrevious };
    if (!usePrevious && !single) query.since = RANGES.find((r) => r.id === range)!.since;
    streamLogs(path, query, (e) => {
      switch (e.type) {
        case "start":
          setStreamPods(e.pods);
          setHistory(e.source === "history" ? e.message ?? "From log history: the pod has been removed." : undefined);
          if (e.follow) setPhase({ phase: "live" });
          break;
        case "line":
          e.line.text = stripAnsi(e.line.text);
          pending.push(e.line);
          // Batch: at most ten renders a second, however fast lines come.
          if (!timer) timer = window.setTimeout(flush, 100);
          break;
        case "status":
          setPods((s) => ({ ...s, [e.pod]: { state: e.state, message: e.message } }));
          break;
        case "dropped":
          setDropped((d) => d + e.lines);
          break;
        case "end":
          flush();
          setPhase({ phase: "ended", reason: e.reason, message: e.message });
          break;
      }
    }, ctrl.signal)
      .then(() => {
        flush();
        setPhase((p) => (p.phase === "ended" ? p : { phase: "ended", reason: "closed", message: "The connection closed." }));
      })
      .catch((err: unknown) => {
        if (ctrl.signal.aborted) return;
        flush();
        setPhase({ phase: "error", message: err instanceof Error ? err.message : String(err) });
      });
    return () => {
      ctrl.abort();
      window.clearTimeout(timer);
    };
  }, [path, pod, range, live, usePrevious, attempt, single]);

  // ---- scrolling: a fixed row height lets us render only what is visible.
  const box = useRef<HTMLDivElement>(null);
  const [view, setView] = useState({ top: 0, height: 480 });
  const stick = useRef(true); // keep the newest line in view while following
  const [atBottom, setAtBottom] = useState(true);
  const onScroll = () => {
    const el = box.current;
    if (!el) return;
    const bottom = el.scrollTop + el.clientHeight >= el.scrollHeight - ROW * 1.5;
    stick.current = bottom;
    setAtBottom(bottom);
    setView({ top: el.scrollTop, height: el.clientHeight });
  };
  useEffect(() => {
    const el = box.current;
    if (!el) return;
    const ro = new ResizeObserver(() => setView({ top: el.scrollTop, height: el.clientHeight }));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);
  // The newest lines are what people look for, followed or not.
  useLayoutEffect(() => {
    const el = box.current;
    if (el && stick.current) el.scrollTop = el.scrollHeight;
  }, [version]);
  const toBottom = () => {
    const el = box.current;
    if (!el) return;
    stick.current = true;
    el.scrollTop = el.scrollHeight;
  };

  // ---- search: highlight every match, Enter jumps to the next line with one.
  const needle = search.trim().toLowerCase();
  const matches = useMemo(() => {
    if (!needle) return [];
    const out: number[] = [];
    lines.current.forEach((l, i) => {
      if (l.text.toLowerCase().includes(needle)) out.push(i);
    });
    return out;
  }, [needle, version]); // lines.current changes with version
  const [current, setCurrent] = useState(-1);
  useEffect(() => setCurrent(-1), [needle]);
  const jump = (dir: 1 | -1) => {
    if (matches.length === 0 || !box.current) return;
    const next = current < 0 ? (dir === 1 ? 0 : matches.length - 1) : (current + dir + matches.length) % matches.length;
    setCurrent(next);
    stick.current = false;
    box.current.scrollTop = Math.max(0, matches[next]! * ROW - box.current.clientHeight / 2);
  };
  const currentRow = current >= 0 ? matches[current] : -1;

  // ---- what to render
  const all = lines.current;
  const first = Math.max(0, Math.floor(view.top / ROW) - 30);
  const last = Math.min(all.length, Math.ceil((view.top + view.height) / ROW) + 30);
  const known = replicas?.map((r) => r.name) ?? streamPods;
  const colors = useMemo(() => {
    const m = new Map<string, number>();
    [...known].sort().forEach((n, i) => m.set(n, (i % 4) + 1));
    return m;
  }, [known.join(" ")]); // by content, not identity
  const tagged = pod === "" && !single;

  const download = () => {
    const text = all.map((l) => [l.ts, tagged ? l.pod : undefined, l.text].filter((x) => x !== undefined && x !== "").join(" ")).join("\n");
    const url = URL.createObjectURL(new Blob([text + "\n"], { type: "text/plain;charset=utf-8" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = `${downloadName}${pod ? "-" + shortPod(pod) : ""}-${new Date().toISOString().slice(0, 19).replace(/[:T]/g, "")}.log`;
    document.body.append(a);
    a.click();
    a.remove();
    window.setTimeout(() => URL.revokeObjectURL(url), 1000);
  };

  return (
    <div className="logviewer">
      <div className="toolbar">
        {!single && (
          <select className="input" aria-label="Replica" value={pod} onChange={(e) => { setPod(e.target.value); setPrevious(false); }}>
            <option value="">All replicas ({known.length})</option>
            {pod !== "" && !known.includes(pod) && <option value={pod}>{pod}</option>}
            {known.map((n) => <option key={n} value={n}>{n}</option>)}
          </select>
        )}
        <span className="logsearch">
          <input className="input mono" type="search" placeholder="Search" aria-label="Search logs" value={search}
            onChange={(e) => setSearch(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); jump(e.shiftKey ? -1 : 1); } }} />
          {needle && <span className="count" aria-live="polite">{matches.length === 0 ? "no match" : `${current >= 0 ? current + 1 + "/" : ""}${matches.length} line${matches.length === 1 ? "" : "s"}`}</span>}
        </span>
        {!single && (
          <div className="seg" role="group" aria-label="Time range">
            {RANGES.map((r) => (
              <button key={r.id} type="button" aria-pressed={range === r.id} disabled={usePrevious} onClick={() => setRange(r.id)}>{r.label}</button>
            ))}
          </div>
        )}
        {canPrevious && <Toggle on={previous} onChange={setPrevious} title="Logs of the container instance before the last restart">Previous container</Toggle>}
        <Toggle on={live} onChange={setFollow} disabled={usePrevious} className="push">Follow</Toggle>
        <Toggle on={timestamps} onChange={setTimestamps}>Timestamps</Toggle>
        <button type="button" className="btn sm" onClick={download} disabled={all.length === 0}>Download</button>
      </div>

      {history && <div className="banner info" role="note"><Icon name="clock" /><span>{history}</span></div>}
      <div className="logpanel">
        <div className="logv" ref={box} onScroll={onScroll} style={{ height }} tabIndex={0} role="log" aria-label="Log lines">
          {all.length === 0 ? (
            <div className="logempty">{emptyText(phase, usePrevious, single ? "" : RANGES.find((r) => r.id === range)!.label, waiting)}</div>
          ) : (
            <div className="logv-in" style={{ height: all.length * ROW }}>
              <div style={{ transform: `translateY(${first * ROW}px)` }}>
                {all.slice(first, last).map((l, i) => (
                  <div key={first + i + trimmed.current} className={first + i === currentRow ? "row cur" : "row"}>
                    {tagged && <span className={`p${colors.get(l.pod) ?? 1}`} title={l.pod}>{shortPod(l.pod)} </span>}
                    {timestamps && l.ts && <span className="ts" title={l.ts}>{clock(l.ts)} </span>}
                    {highlight(l.text, needle)}
                    {l.truncated && <span className="cut"> … (line cut)</span>}
                  </div>
                ))}
              </div>
            </div>
          )}
        </div>
        {live && !atBottom && all.length > 0 && (
          <button type="button" className="btn sm jump" onClick={toBottom}>Jump to latest ↓</button>
        )}
        <div className="logfoot" role="status">
          <Footer phase={phase} live={live} count={all.length} trimmed={trimmed.current} dropped={dropped} pods={pods}
            streaming={Object.values(pods).filter((p) => p.state === "streaming").length} onResume={() => setAttempt((a) => a + 1)} />
        </div>
      </div>
    </div>
  );
}

function emptyText(p: Phase, previous: boolean, range: string, waiting?: string) {
  switch (p.phase) {
    case "connecting":
      return "Loading logs…";
    case "error":
      return "No logs.";
    case "live":
      return waiting ?? (range ? `No lines in the last ${range} yet. New lines appear here.` : "No lines yet. New lines appear here.");
    default:
      if (p.reason === "gone") return p.message ?? "These logs are no longer available.";
      return previous ? "The previous container left no logs." : range ? `No log lines in the last ${range}.` : "No log lines.";
  }
}

function Footer({ phase, live, count, trimmed, dropped, pods, streaming, onResume }: {
  phase: Phase; live: boolean; count: number; trimmed: number; dropped: number; pods: Record<string, PodState>; streaming: number; onResume: () => void;
}) {
  const notes: ReactNode[] = [];
  for (const [name, s] of Object.entries(pods)) {
    if (s.state !== "streaming" && s.message) notes.push(<span key={name} className="podnote" title={name}>{shortPod(name)}: {s.message}</span>);
  }
  const lines = `${count.toLocaleString()} line${count === 1 ? "" : "s"}${trimmed ? ` (${trimmed.toLocaleString()} older ones left the view)` : ""}`;
  return (
    <>
      {phase.phase === "connecting" && <span>— connecting… —</span>}
      {phase.phase === "live" && live && <span>— following · {streaming} {streaming === 1 ? "stream" : "streams"} · {lines} —</span>}
      {phase.phase === "ended" && phase.reason === "complete" && <span>— end of logs{history ? " (from log history)" : ""} · {lines} —</span>}
      {phase.phase === "ended" && phase.reason === "gone" && <span>{phase.message}</span>}
      {phase.phase === "ended" && phase.reason !== "complete" && phase.reason !== "gone" && (
        <><span>{phase.message ?? "The stream stopped."}</span><button type="button" className="btn sm" onClick={onResume}>Resume</button></>
      )}
      {phase.phase === "error" && <><span className="err">{phase.message}</span><button type="button" className="btn sm" onClick={onResume}>Retry</button></>}
      {dropped > 0 && <span className="wn">{dropped.toLocaleString()} lines skipped: more than the stream's rate limit. Pick one replica, or download from kubectl.</span>}
      {notes}
    </>
  );
}

export function Toggle({ on, onChange, disabled, children, title, className }: {
  on: boolean; onChange: (v: boolean) => void; disabled?: boolean; children: ReactNode; title?: string; className?: string;
}) {
  return (
    <button type="button" role="switch" aria-checked={on} className={`toggle${className ? " " + className : ""}`} disabled={disabled} title={title} onClick={() => onChange(!on)}>
      <i aria-hidden="true" />{children}
    </button>
  );
}

/** "2026-10-04T09:14:02.381234567Z" → "09:14:02.381" in local time. */
function clock(ts: string) {
  const d = new Date(ts);
  if (Number.isNaN(d.getTime())) return ts;
  const p = (n: number, w = 2) => String(n).padStart(w, "0");
  return `${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}`;
}

const LEVEL = /\b(ERROR|ERR|FATAL|PANIC|CRITICAL|WARN|WARNING)\b/;

/** Marks search matches and colours the first log level word. */
export function highlight(text: string, needle: string): ReactNode {
  type Range = { from: number; to: number; cls: string };
  const ranges: Range[] = [];
  if (needle) {
    const lower = text.toLowerCase();
    for (let i = lower.indexOf(needle); i >= 0; i = lower.indexOf(needle, i + needle.length)) {
      ranges.push({ from: i, to: i + needle.length, cls: "mark" });
    }
  }
  const lv = LEVEL.exec(text);
  if (lv) {
    const from = lv.index;
    const to = from + lv[0].length;
    if (!ranges.some((r) => r.from < to && from < r.to)) ranges.push({ from, to, cls: lv[0].startsWith("W") ? "wn" : "er" });
  }
  if (ranges.length === 0) return text;
  ranges.sort((a, b) => a.from - b.from);
  const out: ReactNode[] = [];
  let at = 0;
  ranges.forEach((r, i) => {
    if (r.from > at) out.push(text.slice(at, r.from));
    const part = text.slice(r.from, r.to);
    out.push(r.cls === "mark" ? <mark key={i}>{part}</mark> : <span key={i} className={r.cls}>{part}</span>);
    at = r.to;
  });
  if (at < text.length) out.push(text.slice(at));
  return out;
}
