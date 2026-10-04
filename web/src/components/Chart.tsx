import { useId, useLayoutEffect, useRef, useState, type KeyboardEvent, type PointerEvent } from "react";
import { formatDateTime, formatTime, nearest, niceTicks, segments, summarize, type Point } from "../metrics";

// A small dependency-free SVG time-series chart (CSP is 'self', and the
// blueprint's charts are this simple): one y-axis from 0, area or line
// marks, a crosshair with a readout of every series on hover or with the
// arrow keys, a text summary for screen readers and a data table.

export type Tone = "accent" | "c2" | "bad" | "warn";

export type ChartSeries = {
  label: string;
  points: Point[];
  tone?: Tone;
  /** Fill under the line (the main series); otherwise a line only. */
  area?: boolean;
  /** Dashed, for limits and thresholds that change over time. */
  dashed?: boolean;
};

type Props = {
  /** Names the chart for screen readers, e.g. "CPU, cores". */
  title: string;
  series: ChartSeries[];
  start: number;
  end: number;
  step: number;
  format: (v: number) => string;
  /** Byte axes tick in powers of 1024. */
  bytes?: boolean;
  height?: number;
  /** The y-axis reaches at least this high (e.g. 1 for a 0–100% chart). */
  minMax?: number;
};

const PAD = { l: 52, r: 12, t: 10, b: 24 };

export function Chart({ title, series, start, end, step, format, bytes, height = 180, minMax = 0 }: Props) {
  const box = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(640);
  const [cursor, setCursor] = useState<number>(); // unix seconds
  const [table, setTable] = useState(false);
  const id = useId();

  useLayoutEffect(() => {
    const el = box.current;
    if (!el) return;
    if (el.clientWidth > 0) setWidth(Math.max(240, el.clientWidth));
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(([e]) => e && setWidth(Math.max(240, Math.round(e.contentRect.width))));
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  let max = minMax;
  for (const s of series) for (const p of s.points) max = Math.max(max, p.v);
  const ticks = niceTicks(max, height < 150 ? 2 : 4, bytes);
  const top = ticks[ticks.length - 1]!;
  const span = Math.max(1, end - start);
  const W = width;
  const H = height;
  const x = (t: number) => PAD.l + ((t - start) / span) * (W - PAD.l - PAD.r);
  const y = (v: number) => PAD.t + (1 - v / top) * (H - PAD.t - PAD.b);
  const nx = W < 420 ? 3 : 5;
  const xTicks = Array.from({ length: nx }, (_, i) => start + (span * i) / (nx - 1));

  // The crosshair snaps to the samples of the longest series.
  const base = series.reduce<Point[]>((a, s) => (s.points.length > a.length ? s.points : a), []);
  const snapped = cursor === undefined ? undefined : nearest(base, cursor);

  const toTime = (clientX: number) => {
    const r = box.current?.querySelector("svg")?.getBoundingClientRect();
    if (!r) return undefined;
    const px = ((clientX - r.left) / r.width) * W;
    return start + ((px - PAD.l) / (W - PAD.l - PAD.r)) * span;
  };
  const onMove = (e: PointerEvent) => setCursor(toTime(e.clientX));
  const onKey = (e: KeyboardEvent) => {
    if (!base.length) return;
    const i = snapped ? base.indexOf(snapped) : base.length - 1;
    const next = e.key === "ArrowLeft" ? i - 1 : e.key === "ArrowRight" ? i + 1 : e.key === "Home" ? 0 : e.key === "End" ? base.length - 1 : undefined;
    if (next === undefined) {
      if (e.key === "Escape") setCursor(undefined);
      return;
    }
    e.preventDefault();
    setCursor(base[Math.max(0, Math.min(base.length - 1, next))]!.t);
  };

  const summary = series.map((s) => {
    const sum = summarize(s.points);
    return sum
      ? `${s.label}: now ${format(sum.latest)}, peak ${format(sum.max)}, low ${format(sum.min)}`
      : `${s.label}: no data`;
  }).join(". ");

  return (
    <figure className="mx-chart" aria-labelledby={`${id}-cap`}>
      <figcaption id={`${id}-cap`} className="mx-sr">{title}. {summary}.</figcaption>
      <div className="mx-plot" ref={box}>
        <svg
          viewBox={`0 0 ${W} ${H}`} width="100%" height={H} role="img" aria-label={`${title} chart; use the arrow keys to read values`}
          tabIndex={0} onPointerMove={onMove} onPointerLeave={() => setCursor(undefined)} onKeyDown={onKey} onBlur={() => setCursor(undefined)}
        >
          {ticks.map((t) => (
            <g key={t}>
              <line className="grid" x1={PAD.l} x2={W - PAD.r} y1={y(t)} y2={y(t)} />
              <text className="ax" x={PAD.l - 8} y={y(t) + 3.5} textAnchor="end">{format(t)}</text>
            </g>
          ))}
          {xTicks.map((t, i) => (
            <text key={i} className="ax" x={x(t)} y={H - 6} textAnchor={i === 0 ? "start" : i === xTicks.length - 1 ? "end" : "middle"}>
              {formatTime(t, span)}
            </text>
          ))}
          {series.map((s, k) => {
            const tone = s.tone ?? (k === 0 ? "accent" : "c2");
            return segments(s.points, step).map((seg, j) => {
              const line = seg.map((p, i) => `${i ? "L" : "M"}${x(p.t).toFixed(1)} ${y(p.v).toFixed(1)}`).join(" ");
              const single = seg.length === 1;
              return (
                <g key={`${k}-${j}`} className={`tone-${tone}`}>
                  {s.area && !single && <path className="area" d={`${line} L${x(seg[seg.length - 1]!.t).toFixed(1)} ${y(0)} L${x(seg[0]!.t).toFixed(1)} ${y(0)} Z`} />}
                  {single ? <circle className="dot" cx={x(seg[0]!.t)} cy={y(seg[0]!.v)} r={2.5} /> : <path className={s.dashed ? "line dashed" : "line"} d={line} />}
                </g>
              );
            });
          })}
          {snapped && (
            <g className="cross">
              <line x1={x(snapped.t)} x2={x(snapped.t)} y1={PAD.t} y2={H - PAD.b} />
              {series.map((s, k) => {
                const p = nearest(s.points, snapped.t);
                return p && Math.abs(p.t - snapped.t) <= step / 2 ? (
                  <circle key={k} className={`tone-${s.tone ?? (k === 0 ? "accent" : "c2")}`} cx={x(p.t)} cy={y(p.v)} r={4} />
                ) : null;
              })}
            </g>
          )}
        </svg>
        {snapped && (
          <div className="mx-tip" role="status" style={x(snapped.t) > W / 2 ? { right: `${((W - x(snapped.t)) / W) * 100 + 1}%` } : { left: `${(x(snapped.t) / W) * 100 + 1}%` }}>
            <div className="mx-tip-t">{formatDateTime(snapped.t)}</div>
            {readout(series, snapped.t, step).map(({ s, k, v }) => (
              <div key={k} className="mx-tip-row">
                <i className={`mx-key tone-${s.tone ?? (k === 0 ? "accent" : "c2")}${s.dashed ? " dashed" : ""}`} aria-hidden="true" />
                <b>{v === undefined ? "—" : format(v)}</b> <span>{s.label}</span>
              </div>
            ))}
            {series.length > TIP_ROWS && <div className="mx-tip-t">{series.length - TIP_ROWS} more series</div>}
          </div>
        )}
      </div>
      <button type="button" className="btn ghost sm mx-table-toggle" aria-expanded={table} onClick={() => setTable(!table)}>
        {table ? "Hide data" : "Show data"}
      </button>
      {table && <DataTable series={series} format={format} />}
    </figure>
  );
}

const TIP_ROWS = 8;

/** Every series' value at t; with many series, the largest TIP_ROWS. */
function readout(series: ChartSeries[], t: number, step: number) {
  const rows = series.map((s, k) => {
    const p = nearest(s.points, t);
    return { s, k, v: p && Math.abs(p.t - t) <= step / 2 ? p.v : undefined };
  });
  if (rows.length <= TIP_ROWS) return rows;
  return rows.sort((a, b) => (b.v ?? -Infinity) - (a.v ?? -Infinity)).slice(0, TIP_ROWS);
}

function DataTable({ series, format }: { series: ChartSeries[]; format: (v: number) => string }) {
  const times = [...new Set(series.flatMap((s) => s.points.map((p) => p.t)))].sort((a, b) => b - a);
  const byT = series.map((s) => new Map(s.points.map((p) => [p.t, p.v])));
  return (
    <div className="mx-table scroll-x">
      <table className="t">
        <thead><tr><th>Time</th>{series.map((s) => <th key={s.label} className="num">{s.label}</th>)}</tr></thead>
        <tbody>
          {times.map((t) => (
            <tr key={t}>
              <td className="mono">{formatDateTime(t)}</td>
              {byT.map((m, i) => <td key={i} className="num">{m.has(t) ? format(m.get(t)!) : "—"}</td>)}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

/** The legend for charts with two or more series: line keys, labels in ink. */
export function Legend({ items }: { items: { label: string; tone: Tone; dashed?: boolean }[] }) {
  return (
    <span className="mx-legend">
      {items.map((i) => (
        <span key={i.label}><i className={`mx-key tone-${i.tone}${i.dashed ? " dashed" : ""}`} aria-hidden="true" />{i.label}</span>
      ))}
    </span>
  );
}
