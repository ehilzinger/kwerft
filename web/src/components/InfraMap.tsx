// The Overview's infrastructure map (docs/plan.md, "Overview map"): draws
// the layout from topology.ts as SVG, with hover and click focus, pan and
// zoom, the Traffic ↔ Placement change and an inspector beside the map.
import { useEffect, useMemo, useRef, useState, type KeyboardEvent, type PointerEvent, type ReactNode, type WheelEvent } from "react";
import { appLogsHref } from "../alerts";
import { describeSources } from "../firewall";
import { runEnd, when } from "../jobs";
import { RouterLink } from "../pages/MonitoringAlerts";
import {
  appId, between, buildModel, COLLAPSE_ABOVE, fit, focusOf, fwLabel, geom, gib, labelAt, layout, openToAll, problemsOf, searchOf, volumeFill,
  type Edge, type Entity, type Focus, type Item, type Layers, type Lens, type Model, type Tone, type Topology, type TopoNode, type ViewBox,
} from "../topology";
import { count, portsLabel, sideLabel } from "../traffic";
import { ago } from "../workloads";
import { Icon } from "./Icon";
import "../styles/map.css";

const reducedMotion = () => typeof window !== "undefined" && window.matchMedia?.("(prefers-reduced-motion: reduce)").matches;
const TWEEN_MS = 560;
const ease = (t: number) => (t < 0.5 ? 2 * t * t : 1 - Math.pow(-2 * t + 2, 2) / 2);
const layerNames: { key: keyof Layers; label: string; swatch: string }[] = [
  { key: "jobs", label: "Jobs", swatch: "var(--ink-2)" },
  { key: "volumes", label: "Volumes", swatch: "var(--c2)" },
  { key: "domains", label: "Domains", swatch: "var(--accent)" },
  { key: "rules", label: "Traffic rules", swatch: "var(--accent)" },
  { key: "firewall", label: "Firewall", swatch: "var(--faint)" },
];

/** A request from outside (Needs attention) to show something; seq makes repeats count. */
export type MapRequest = { id: string; seq: number };

type Props = {
  topology?: Topology;
  error?: string;
  loading: boolean;
  /** Cluster picker: shown with more than one cluster. */
  clusters: string[];
  cluster: string;
  onCluster: (c: string) => void;
  request?: MapRequest;
};

export function InfraMap({ topology, error, loading, clusters, cluster, onCluster, request }: Props) {
  const model = useMemo(() => (topology ? buildModel(topology) : undefined), [topology]);
  const [lens, setLens] = useState<Lens>("traffic");
  const [layers, setLayers] = useState<Layers>({ jobs: true, volumes: true, domains: true, rules: true, firewall: true });
  const [project, setProject] = useState("");
  const [collapsed, setCollapsed] = useState<Set<string>>(new Set());
  const [problems, setProblems] = useState(false);
  const [query, setQuery] = useState("");
  const [selected, setSelected] = useState<string | null>(null);
  const [hover, setHover] = useState<string | null>(null);
  const [full, setFull] = useState(false);
  const [vb, setVB] = useState<ViewBox | null>(null);
  const [userZoom, setUserZoom] = useState(false);
  const [anim, setAnim] = useState<{ from: Layout; vb: ViewBox } | null>(null);
  const [frame, setFrame] = useState<Layout | null>(null);
  const [tip, setTip] = useState<{ x: number; y: number; text: string } | null>(null);
  const [hint, setHint] = useState(false);
  const svgRef = useRef<SVGSVGElement>(null);
  const canvasRef = useRef<HTMLDivElement>(null);
  const cardRef = useRef<HTMLDivElement>(null);

  // Big clusters open with every project collapsed, once per cluster.
  const seenCluster = useRef<string | null>(null);
  useEffect(() => {
    if (!topology || seenCluster.current === topology.cluster) return;
    seenCluster.current = topology.cluster;
    setCollapsed(new Set(topology.apps.length > COLLAPSE_ABOVE ? topology.projects.map((p) => p.name) : []));
    setProject("");
    setSelected(null);
    setUserZoom(false);
  }, [topology]);

  const view = useMemo(() => ({ lens, layers, project: project || undefined, collapsed }), [lens, layers, project, collapsed]);
  const target = useMemo(() => (model ? layout(model, view) : undefined), [model, view]);
  const shown = frame ?? target;

  // Fit the view to the map unless the user has panned or zoomed.
  useEffect(() => {
    if (target && !anim && (!userZoom || !vb)) setVB(fit(target));
  }, [target]);

  // The lens change: items move from the old layout to the new one.
  useEffect(() => {
    if (!anim || !target) return;
    const to = fit(target);
    let raf = 0;
    const t0 = performance.now();
    const step = (now: number) => {
      const raw = Math.min(1, (now - t0) / TWEEN_MS), t = ease(raw);
      setFrame(between(anim.from, target, t));
      setVB({ x: anim.vb.x + (to.x - anim.vb.x) * t, y: anim.vb.y + (to.y - anim.vb.y) * t, w: anim.vb.w + (to.w - anim.vb.w) * t, h: anim.vb.h + (to.h - anim.vb.h) * t });
      if (raw < 1) raf = requestAnimationFrame(step);
      else { setFrame(null); setAnim(null); setVB(to); }
    };
    raf = requestAnimationFrame(step);
    return () => cancelAnimationFrame(raf);
  }, [anim]);

  function switchLens(l: Lens) {
    if (l === lens) return;
    if (target && vb && !reducedMotion()) setAnim({ from: target, vb });
    setUserZoom(false);
    setLens(l);
  }

  const focus: Focus | null = useMemo(() => {
    if (!shown || frame) return null;
    if (selected) return focusOf(shown, selected);
    if (hover) return focusOf(shown, hover);
    if (query.trim()) return searchOf(shown, query);
    if (problems) return problemsOf(shown);
    return null;
  }, [shown, frame, selected, hover, query, problems]);

  // Show what Needs attention asked for: its project open, its layers on.
  useEffect(() => {
    if (!request || !model) return;
    const e = model.byId.get(request.id);
    if (!e) return;
    setProject("");
    setCollapsed((c) => { const n = new Set(c); n.delete(e.project); return n; });
    if (e.kind === "job" || e.kind === "task" || e.kind === "volume" || e.kind === "domain") {
      const key = e.kind === "task" ? "jobs" : e.kind === "job" ? "jobs" : e.kind === "volume" ? "volumes" : "domains";
      setLayers((l) => ({ ...l, [key]: true }));
    }
    setSelected(request.id);
    cardRef.current?.scrollIntoView({ behavior: reducedMotion() ? "auto" : "smooth", block: "center" });
  }, [request?.seq]);

  // Keep the selection out from under the inspector: an item, or both ends of a
  // selected connection (a rule, a drop), with its whole width left of the panel.
  useEffect(() => {
    if (!selected || !shown || !vb || !svgRef.current || !canvasRef.current) return;
    let its = Object.values(shown.items).filter((i) => i.sel === selected);
    if (!its.length) {
      const e = shown.edges.find((x) => x.sel === selected);
      if (e) its = [shown.items[e.a], shown.items[e.b]].filter((i): i is Item => !!i);
    }
    const m = svgRef.current.getScreenCTM();
    if (!its.length || !m) return;
    const box = canvasRef.current.getBoundingClientRect();
    const panel = canvasRef.current.querySelector(".imap-insp")?.getBoundingClientRect();
    const right = (panel ? panel.left - box.left : box.width - 340) - 16;
    const sx = (x: number) => x * m.a + m.e - box.left, sy = (y: number) => y * m.d + m.f - box.top;
    const x0 = Math.min(...its.map((i) => sx(i.x - i.w / 2))), x1 = Math.max(...its.map((i) => sx(i.x + i.w / 2)));
    const y0 = Math.min(...its.map((i) => sy(i.y - i.h / 2))), y1 = Math.max(...its.map((i) => sy(i.y + i.h / 2)));
    // Shift by what sticks out; when it is too big to fit, its left or top edge wins.
    let dx = Math.max(0, x1 - right), dy = Math.max(0, y1 - (box.height - 20));
    if (x0 - dx < 20) dx = x0 - 20;
    if (y0 - dy < 20) dy = y0 - 20;
    if (dx || dy) {
      const u = 1 / m.a;
      setVB({ ...vb, x: vb.x + dx * u, y: vb.y + dy * u });
      setUserZoom(true);
    }
  }, [selected]);

  useEffect(() => {
    const onKey = (e: globalThis.KeyboardEvent) => {
      if (e.key !== "Escape") return;
      if (selected) setSelected(null);
      else if (full) setFull(false);
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [selected, full]);

  // Pan by dragging the background; ⌘/Ctrl + wheel zooms (plain wheel scrolls the page).
  const drag = useRef<{ x: number; y: number; vb: ViewBox; u: number; moved: boolean } | null>(null);
  const suppressClick = useRef(false);
  function onPointerDown(e: PointerEvent<SVGSVGElement>) {
    if (e.button !== 0 || !vb || (e.target as Element).closest("[data-sel],[data-act]")) return;
    const m = svgRef.current?.getScreenCTM();
    drag.current = { x: e.clientX, y: e.clientY, vb, u: m ? 1 / m.a : 1, moved: false };
    svgRef.current?.setPointerCapture(e.pointerId);
  }
  function onPointerMove(e: PointerEvent<SVGSVGElement>) {
    const d = drag.current;
    if (d) {
      const dx = e.clientX - d.x, dy = e.clientY - d.y;
      if (!d.moved && Math.hypot(dx, dy) > 3) d.moved = true;
      if (d.moved) { setVB({ ...d.vb, x: d.vb.x - dx * d.u, y: d.vb.y - dy * d.u }); setUserZoom(true); }
      return;
    }
    const g = (e.target as Element).closest<SVGGElement>(".imap-e");
    const box = canvasRef.current?.getBoundingClientRect();
    const edge = g && shown?.edges.find((x) => x.id === g.dataset.eid);
    if (edge && box) setTip({ x: Math.min(e.clientX - box.left + 14, box.width - 280), y: e.clientY - box.top + 14, text: edge.tip });
    else if (tip) setTip(null);
  }
  function onPointerUp() {
    if (drag.current) { suppressClick.current = drag.current.moved; drag.current = null; }
  }
  function zoomAt(f: number, cx?: number, cy?: number) {
    if (!vb) return;
    const x = cx ?? vb.x + vb.w / 2, y = cy ?? vb.y + vb.h / 2;
    const w = Math.min(Math.max(vb.w * f, 300), 6000), k = w / vb.w;
    setVB({ x: x - (x - vb.x) * k, y: y - (y - vb.y) * k, w, h: vb.h * k });
    setUserZoom(true);
  }
  // React's wheel listener is passive; zoom needs preventDefault.
  useEffect(() => {
    const svg = svgRef.current;
    if (!svg) return;
    const onWheel = (e: globalThis.WheelEvent) => { if (e.ctrlKey || e.metaKey) e.preventDefault(); };
    svg.addEventListener("wheel", onWheel, { passive: false });
    return () => svg.removeEventListener("wheel", onWheel);
  }, []);
  const hintTimer = useRef<number>(0);
  function onWheel(e: WheelEvent<SVGSVGElement>) {
    if (!(e.ctrlKey || e.metaKey)) {
      setHint(true);
      window.clearTimeout(hintTimer.current);
      hintTimer.current = window.setTimeout(() => setHint(false), 1200);
      return;
    }
    const m = svgRef.current?.getScreenCTM();
    if (!m) return;
    const p = new DOMPoint(e.clientX, e.clientY).matrixTransform(m.inverse());
    zoomAt(Math.exp(e.deltaY * 0.0025), p.x, p.y);
  }

  function toggleCollapse(p: string) {
    setCollapsed((c) => { const n = new Set(c); if (n.has(p)) n.delete(p); else n.add(p); return n; });
    if (lens !== "traffic") switchLens("traffic");
  }
  function onClick(e: React.MouseEvent<SVGSVGElement>) {
    if (suppressClick.current) { suppressClick.current = false; return; }
    const act = (e.target as Element).closest<SVGGElement>("[data-act]");
    if (act?.dataset.p) return toggleCollapse(act.dataset.p);
    const t = (e.target as Element).closest<SVGGElement>("[data-sel]");
    setSelected(t?.dataset.sel && t.dataset.sel !== selected ? t.dataset.sel : null);
  }
  function onKeyDown(e: KeyboardEvent<SVGSVGElement>) {
    if (e.key !== "Enter" && e.key !== " ") return;
    const act = (e.target as Element).closest<SVGGElement>("[data-act]");
    const t = (e.target as Element).closest<SVGGElement>("[data-sel]");
    if (!act && !t) return;
    e.preventDefault();
    if (act?.dataset.p) toggleCollapse(act.dataset.p);
    else if (t?.dataset.sel) setSelected(t.dataset.sel === selected ? null : t.dataset.sel);
  }
  function onMouseOver(e: React.MouseEvent<SVGSVGElement>) {
    const s = (e.target as Element).closest<SVGGElement>("[data-sel]")?.dataset.sel ?? null;
    if (s !== hover) setHover(s);
  }

  const t = topology;
  const apps = t?.apps.length ?? 0;
  const jobsN = (t?.schedules.length ?? 0) + (t?.tasks.length ?? 0);
  const summary = t ? [`${t.nodes.length} ${t.nodes.length === 1 ? "server" : "servers"}`, `${apps} ${apps === 1 ? "app" : "apps"}`,
    `${jobsN} ${jobsN === 1 ? "job" : "jobs"}`, `${t.volumes.length} ${t.volumes.length === 1 ? "volume" : "volumes"}`, `${t.rules.length} traffic ${t.rules.length === 1 ? "rule" : "rules"}`].join(" · ") : "";

  return (
    <div className={`card imap${full ? " full" : ""}`} ref={cardRef} id="map">
      <div className="ch-h">
        <div className="imap-title"><h3>Infrastructure map</h3><span className="dim">{summary}</span></div>
        <div className="imap-acts">
          {clusters.length > 1 && (
            <select className="input imap-select" aria-label="Cluster" value={cluster} onChange={(e) => onCluster(e.target.value)}>
              {clusters.map((c) => <option key={c} value={c}>{c}</option>)}
            </select>
          )}
          <button type="button" className="btn sm" aria-pressed={full} onClick={() => { setFull(!full); setUserZoom(false); if (target) setVB(fit(target)); }}>
            <Icon name="expand" />{full ? "Exit full screen" : "Full screen"}
          </button>
        </div>
      </div>
      <div className="imap-tool">
        <div className="seg" role="group" aria-label="Lens">
          <button type="button" aria-pressed={lens === "traffic"} onClick={() => switchLens("traffic")}><Icon name="net" />Traffic</button>
          <button type="button" aria-pressed={lens === "placement"} onClick={() => switchLens("placement")}><Icon name="server" />Placement</button>
        </div>
        <div className="imap-layers" role="group" aria-label="Show">
          <span className="imap-lbl">Show</span>
          {layerNames.filter((l) => l.key !== "firewall" || t?.firewall).map((l) => (
            <button type="button" key={l.key} className="imap-chip" aria-pressed={layers[l.key]} style={{ ["--sw" as string]: l.swatch }}
              onClick={() => setLayers({ ...layers, [l.key]: !layers[l.key] })}><i />{l.label}</button>
          ))}
        </div>
        {t && t.projects.length > 1 && (
          <select className="input imap-select" aria-label="Project" value={project} onChange={(e) => { setProject(e.target.value); setSelected(null); setUserZoom(false); }}>
            <option value="">All projects</option>
            {t.projects.map((p) => <option key={p.name} value={p.name}>{p.name}</option>)}
          </select>
        )}
        <button type="button" className="imap-chip prob" aria-pressed={problems} style={{ ["--sw" as string]: "var(--bad)" }} onClick={() => setProblems(!problems)}><i />Only problems</button>
        <label className="imap-find">
          <Icon name="search" />
          <input value={query} onChange={(e) => setQuery(e.target.value)} placeholder="Find on map" aria-label="Find on map" autoComplete="off" />
        </label>
      </div>
      <div className="imap-canvas" ref={canvasRef}>
        {shown && vb && (
          <svg ref={svgRef} className={`imap-svg${focus ? " focus" : ""}`} viewBox={`${vb.x} ${vb.y} ${vb.w} ${vb.h}`} preserveAspectRatio="xMidYMid meet"
            role="img" aria-label={`Infrastructure map of ${t?.cluster}`}
            onPointerDown={onPointerDown} onPointerMove={onPointerMove} onPointerUp={onPointerUp}
            onPointerLeave={() => { setTip(null); setHover(null); }} onMouseOver={onMouseOver} onClick={onClick} onKeyDown={onKeyDown} onWheel={onWheel}>
            <Markers />
            <Drawing layout={shown} focus={focus} selected={selected} animating={!!frame} />
          </svg>
        )}
        {loading && !t && <p className="imap-state">Loading the map…</p>}
        {error && <p className="imap-state warn-text">{error}</p>}
        {t && t.apps.length === 0 && t.volumes.length === 0 && (
          <p className="imap-state">Nothing runs on {t.cluster} yet. <RouterLink href="/apps/new" className="btn ghost sm">Deploy an app →</RouterLink></p>
        )}
        <Legend topology={t} />
        <div className="imap-zoom">
          <button type="button" aria-label="Zoom in" onClick={() => zoomAt(0.8)}><Icon name="plus" /></button>
          <button type="button" aria-label="Zoom out" onClick={() => zoomAt(1.25)}><Icon name="minus" /></button>
          <button type="button" aria-label="Fit to view" onClick={() => { if (target) setVB(fit(target)); setUserZoom(false); }}><Icon name="fit" /></button>
        </div>
        <div className={`imap-hint${hint ? " on" : ""}`} aria-hidden="true">Hold ⌘ or Ctrl and scroll to zoom</div>
        {tip && <div className="imap-tip" style={{ left: tip.x, top: tip.y }}>{tip.text}</div>}
        {selected && model && (
          <Inspector model={model} sel={selected} onSelect={setSelected} onExpand={(p) => toggleCollapse(p)} />
        )}
      </div>
    </div>
  );
}

type Layout = ReturnType<typeof layout>;

// ---- drawing ---------------------------------------------------------------------

function Markers() {
  const arrow = (id: string, color: string) => (
    <marker id={id} viewBox="0 0 10 10" refX="8.5" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse" markerUnits="userSpaceOnUse">
      <path d="M1.5 1.5L8.5 5L1.5 8.5" fill="none" style={{ stroke: color }} strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round" />
    </marker>
  );
  return (
    <defs>
      {arrow("imap-ar-acc", "color-mix(in srgb, var(--accent) 70%, var(--line))")}
      {arrow("imap-ar-c2", "var(--c2)")}
      {arrow("imap-ar-faint", "var(--faint)")}
      <marker id="imap-x-bad" viewBox="0 0 12 12" refX="10" refY="6" markerWidth="11" markerHeight="11" orient="auto" markerUnits="userSpaceOnUse">
        <path d="M6 2l6 8M12 2L6 10" fill="none" style={{ stroke: "var(--bad)" }} strokeWidth="2" strokeLinecap="round" />
      </marker>
    </defs>
  );
}

const MARK: Record<Edge["kind"], string | undefined> = { rule: "imap-ar-acc", route: "imap-ar-acc", pub: "imap-ar-acc", mount: "imap-ar-c2", ssh: "imap-ar-faint", drop: "imap-x-bad", runs: undefined };
const flowDur = (n: number) => (n > 50000 ? 0.9 : n > 10000 ? 1.4 : n > 1000 ? 2.2 : 3.4);
const BACK = new Set<Item["kind"]>(["pbox", "server", "fwband"]);

function Drawing({ layout: l, focus, selected, animating }: { layout: Layout; focus: Focus | null; selected: string | null; animating: boolean }) {
  const items = Object.values(l.items).filter((i) => i.op > 0.01);
  const edges = l.edges
    .map((e) => ({ e, a: l.items[e.a], b: l.items[e.b] }))
    .filter((o) => o.a && o.b && o.a.op > 0.01 && o.b.op > 0.01 && (o.e.op ?? 1) > 0.01)
    .map((o) => ({ ...o, g: geom(o.a, o.b, o.e.kind), op: Math.min(o.a.op, o.b.op, o.e.op ?? 1) }));
  // Labels keep off the chips (not off the boxes and server cards they sit in).
  const chips = items.filter((i) => !BACK.has(i.kind));
  const motion = !animating && !reducedMotion();
  const hl = (on: boolean) => (focus && on ? " hl" : "");
  return (
    <>
      <g>{items.filter((i) => BACK.has(i.kind)).map((i) => <Shape key={i.key} it={i} lit={!!focus?.keys.has(i.key)} selected={selected} />)}</g>
      <g>
        {edges.map(({ e, g, op }) => (
          <g key={e.id} className={`imap-e k-${e.kind}${hl(!!focus?.edges.has(e.id))}${e.sel === selected ? " sel" : ""}`} data-eid={e.id} data-sel={e.sel} opacity={op < 1 ? op : undefined}>
            <path className="v" d={g.d} markerEnd={MARK[e.kind] ? `url(#${MARK[e.kind]})` : undefined} />
            {motion && !!e.traffic && (e.kind === "rule" || e.kind === "route" || e.kind === "pub" || e.kind === "drop") && (
              <path className="flow" d={g.d} style={{ ["--d" as string]: `${flowDur(e.traffic)}s` }} />
            )}
            <path className="hit" d={g.d} />
          </g>
        ))}
      </g>
      <g>{items.filter((i) => !BACK.has(i.kind)).map((i) => <Shape key={i.key} it={i} lit={!!focus?.keys.has(i.key)} selected={selected} />)}</g>
      <g>
        {edges.filter(({ e }) => e.label).map(({ e, g, op }) => {
          // Dropped counts prefer a spot near the source; every label moves along its edge, or between rows, off the chips.
          const w = e.label!.length * 6.1 + 12;
          const [x, y] = labelAt(g, w, e.kind === "drop" ? 0.28 : 0.5, chips);
          return (
            <g key={e.id} className={`imap-lbl k-${e.kind}${hl(!!focus?.edges.has(e.id))}`} transform={`translate(${x.toFixed(1)},${y.toFixed(1)})`} opacity={op < 1 ? op : undefined}>
              <rect x={-w / 2} y={-9} width={w} height={18} rx={4} />
              <text textAnchor="middle" y={3.5}>{e.label}</text>
            </g>
          );
        })}
      </g>
    </>
  );
}

const trunc = (s: string, n: number) => (s.length > n ? `${s.slice(0, n - 1)}…` : s);
const ICON: Partial<Record<Item["kind"], string>> = { app: "box", job: "clock", task: "play", volume: "disk", domain: "lock", gateway: "gate", pchip: "layers" };
const KIND: Partial<Record<Item["kind"], string>> = { app: "App", job: "Scheduled job", task: "Task", volume: "Volume", domain: "Domain", gateway: "Gateway", pchip: "Project" };
const toneWord = (s?: Tone) => (s === "bad" ? ", failing" : s === "warn" ? ", needs attention" : "");

/** A 16×16 icon inside the map's SVG. */
function Glyph({ name, x, y, size = 16, className = "icg" }: { name: string; x: number; y: number; size?: number; className?: string }) {
  return (
    <svg x={x} y={y} width={size} height={size} viewBox="0 0 16 16" className={className} fill="none" stroke="currentColor" strokeWidth={1.5} strokeLinecap="round" strokeLinejoin="round">
      {GLYPHS[name]}
    </svg>
  );
}
const GLYPHS: Record<string, ReactNode> = {
  box: <><path d="M8 1.8l5.6 3.1v6.2L8 14.2 2.4 11.1V4.9z" /><path d="M2.6 5L8 8l5.4-3M8 8v6" /></>,
  clock: <><circle cx="8" cy="8" r="6" /><path d="M8 4.5V8l2.5 1.5" /></>,
  play: <path d="M5 3.2v9.6L12.6 8z" />,
  disk: <><ellipse cx="8" cy="4" rx="5.5" ry="2" /><path d="M2.5 4v8c0 1.1 2.5 2 5.5 2s5.5-.9 5.5-2V4" /></>,
  lock: <><rect x="3" y="7" width="10" height="7" rx="1.5" /><path d="M5.5 7V5a2.5 2.5 0 015 0v2" /></>,
  gate: <path d="M2 3v10M2 8h11M9.5 4.5L13 8l-3.5 3.5" />,
  layers: <><path d="M8 2l6 3.2-6 3.2-6-3.2z" /><path d="M2 8.6l6 3.2 6-3.2" /></>,
  globe: <><circle cx="8" cy="8" r="6.2" /><path d="M1.8 8h12.4M8 1.8c1.8 1.8 2.6 3.9 2.6 6.2S9.8 12.4 8 14.2C6.2 12.4 5.4 10.3 5.4 8S6.2 3.6 8 1.8z" /></>,
  server: <><rect x="2" y="2.5" width="12" height="4.5" rx="1" /><rect x="2" y="9" width="12" height="4.5" rx="1" /></>,
  shield: <path d="M8 1.5l5.5 2v4.2c0 3.3-2.3 5.6-5.5 6.8-3.2-1.2-5.5-3.5-5.5-6.8V3.5z" />,
};

function Shape({ it: i, lit, selected }: { it: Item; lit: boolean; selected: string | null }) {
  const x = -i.w / 2, y = -i.h / 2;
  const op = i.op < 1 ? i.op : undefined;
  const transform = `translate(${i.x.toFixed(1)},${i.y.toFixed(1)})`;
  const sel = i.sel !== null && i.sel === selected ? " sel" : "";
  const a11y = (label: string) => ({ tabIndex: 0, role: "button", "aria-label": label });

  if (i.kind === "pbox") {
    return (
      <g className="imap-box pbox" data-key={i.key} transform={transform} opacity={op}>
        <rect className="bx" x={x} y={y} width={i.w} height={i.h} rx={12} />
        <Glyph name="layers" x={x + 14} y={y + 11} size={14} className="icf" />
        <text x={x + 34} y={y + 22}><tspan className="bl">{i.label}</tspan><tspan className="bc" dx={8}>{i.counts}</tspan></text>
        <g className="tg" data-act="collapse" data-p={i.project} {...a11y(`${i.collapsed ? "Expand" : "Collapse"} ${i.label}`)} transform={`translate(${-x - 14},${y + 22})`}>
          <rect x={-66} y={-13} width={70} height={19} rx={4} fill="transparent" />
          <text textAnchor="end">{i.collapsed ? "Expand ▸" : "Collapse ▾"}</text>
        </g>
      </g>
    );
  }
  if (i.kind === "server") {
    const s = i.server;
    const u = s?.usage;
    const meter = (k: string, v: number | undefined, mx: number) => v === undefined ? null : (
      <g>
        <text className="mk" x={mx} y={y + 58}>{k}</text>
        <rect className="mb" x={mx + 26} y={y + 52} width={44} height={5} rx={2.5} />
        <rect className="mf" x={mx + 26} y={y + 52} width={(44 * Math.min(100, v)) / 100} height={5} rx={2.5} />
        <text className="mv" x={mx + 76} y={y + 58}>{Math.round(v)}%</text>
      </g>
    );
    const kind = s ? [s.serverType?.toUpperCase() ?? (s.platform === "dedicated" ? "Dedicated" : s.platform === "cloud" ? "Cloud" : "Server"), s.roles.join(", ")].filter(Boolean).join(" · ") : "Pending pods, unattached volumes";
    return (
      <g className={`imap-box k-server${sel}${s && !s.ready ? " s-bad" : ""}`} data-key={i.key} data-sel={i.sel ?? undefined} {...(i.sel ? a11y(`Server ${i.label}`) : {})} transform={transform} opacity={op}>
        <rect className="sx" x={x} y={y} width={i.w} height={i.h} rx={12} />
        <path className="sh" d={`M${x + 1} ${y + 72}H${-x - 1}`} />
        <Glyph name="server" x={x + 14} y={y + 14} />
        <text className="snm" x={x + 38} y={y + 26}>{trunc(i.label, 22)}</text>
        <text className="sty" x={x + 38} y={y + 41}>{trunc(kind, 32)}</text>
        {u ? <>{meter("CPU", u.cpuCapacity ? (100 * u.cpu) / u.cpuCapacity : undefined, x + 14)}{meter("MEM", u.memoryCapacity ? (100 * u.memory) / u.memoryCapacity : undefined, x + 120)}</>
          : <text className="mk" x={x + 14} y={y + 58}>{s ? (s.ready ? s.status : "NOT READY") : ""}</text>}
      </g>
    );
  }
  if (i.kind === "fwband") {
    return (
      <g className={`imap-box k-fwband${sel}`} data-key={i.key} data-sel="fwband" {...a11y("Server firewall")} transform={transform} opacity={op}>
        <rect className="fb" x={x} y={y} width={i.w} height={i.h} rx={10} />
        <Glyph name="shield" x={-6} y={y + 10} size={12} />
        <text textAnchor="middle" y={y + 36}>FIREWALL</text>
      </g>
    );
  }
  const cls = `imap-it k-${i.kind}${i.status ? ` s-${i.status}` : ""}${i.dashed ? " dashed" : ""}${i.kind === "fw" && i.fw && !openToAll(i.fw) ? " closed" : ""}${lit ? " hl" : ""}${sel}`;
  const common = { className: cls, "data-key": i.key, "data-sel": i.sel ?? undefined, transform, opacity: op };
  if (i.kind === "net") {
    const r = i.w / 2;
    return (
      <g {...common} {...a11y(i.out ? "Internet, outbound" : "Internet")}>
        <circle className="bg" r={r} />
        <Glyph name="globe" x={-10} y={-10} size={20} />
        <text className="nm" textAnchor="middle" y={r + 15}>Internet</text>
        {i.out && <text className="sb" textAnchor="middle" y={r + 28}>outbound</text>}
      </g>
    );
  }
  if (i.kind === "fw") {
    const r = i.fw!;
    return (
      <g {...common} {...a11y(`Firewall ${i.label}`)}>
        <rect className="bg" x={x} y={y} width={i.w} height={i.h} rx={7} />
        <text className="nm" textAnchor="middle" y={-1}>{trunc(i.label, 11)}</text>
        <text className="fs" textAnchor="middle" y={10}>{openToAll(r) ? "any" : r.sources.length ? "limited" : "any"}</text>
      </g>
    );
  }
  if (i.kind === "domain") {
    return (
      <g {...common} {...a11y(`Domain ${i.label}${toneWord(i.status)}`)}>
        <rect className="bg" x={x} y={y} width={i.w} height={i.h} rx={15} />
        <Glyph name="lock" x={x + 10} y={-6} size={12} />
        <text className="nm" x={x + 28} y={4}>{trunc(i.label, 16)}</text>
        <circle className="dot" cx={-x - 12} cy={0} r={3.5} />
      </g>
    );
  }
  const e = i.entity;
  const name = i.label;
  let sub = e?.sub ?? "";
  if (i.kind === "gateway") sub = "TLS · HTTP routes";
  if (i.kind === "pchip") sub = i.counts ?? "";
  if (i.pod) sub = `pod ${i.pod.i + 1}/${i.pod.n} · ${i.pod.pod?.status ?? "Running"}`;
  if (i.dashed) sub = e?.kind === "task" ? `ran here · ${e.task?.phase}` : "last ran here";
  const icon = ICON[i.kind] ?? "box";
  const f = e?.volume ? volumeFill(e.volume) : undefined;
  return (
    <g {...common} {...a11y(`${KIND[i.kind] ?? ""} ${name}${toneWord(i.status)}`)}>
      <rect className="bg" x={x} y={y} width={i.w} height={i.h} rx={8} />
      <rect className="ic" x={x + 7} y={-12} width={24} height={24} rx={6} />
      <Glyph name={icon} x={x + 11} y={-8} />
      {i.kind === "volume" ? (
        <>
          <text className="nm" x={x + 38} y={-7}>{trunc(name, 21)}</text>
          <text className="sb" x={x + 38} y={7}>{trunc(sub, 28)}</text>
          {f !== undefined && <><rect className="mb" x={x + 38} y={13} width={i.w - 50} height={3.5} rx={1.75} /><rect className="mf" x={x + 38} y={13} width={(i.w - 50) * f} height={3.5} rx={1.75} /></>}
        </>
      ) : (
        <>
          <text className="nm" x={x + 38} y={-3}>{trunc(name, i.kind === "pchip" ? 24 : 16)}</text>
          <text className="sb" x={x + 38} y={11}>{trunc(sub, i.kind === "pchip" ? 30 : 23)}</text>
          {i.status && i.kind !== "gateway" && <circle className="dot" cx={-x - 11} cy={-8} r={3.5} />}
        </>
      )}
    </g>
  );
}

function Legend({ topology: t }: { topology?: Topology }) {
  const line = (stroke: string, dash?: string) => (
    <svg viewBox="0 0 26 8" aria-hidden="true"><path d="M1 4h24" stroke={stroke} strokeWidth={1.6} strokeDasharray={dash} strokeLinecap="round" /></svg>
  );
  const counts = !t ? "" : t.hubble.state === "ok" ? `Counts: last ${t.hubble.window}` : t.hubble.state === "off" ? "No live counts" : "Counts: waiting for Hubble";
  return (
    <div className="imap-legend" title={t?.hubble.message}>
      <span>{line("color-mix(in srgb, var(--accent) 55%, var(--line))")}Allowed traffic</span>
      <span>{line("var(--bad)", "6 5")}Dropped, no rule</span>
      <span>{line("var(--c2)", "2 4")}Volume mount</span>
      <span>{line("var(--faint)", "3 4")}Runs as / SSH</span>
      {counts && <span>{counts}</span>}
    </div>
  );
}

// ---- inspector ---------------------------------------------------------------------

type Row = { sel: string; kind?: "drop" | "mount" | "route"; label: string; sub?: string; count?: string; tone?: "warn" | "bad" };

function Inspector({ model, sel, onSelect, onExpand }: { model: Model; sel: string; onSelect: (s: string | null) => void; onExpand: (p: string) => void }) {
  const d = detail(model, sel);
  if (!d) return null;
  return (
    <aside className="imap-insp" aria-live="polite" aria-label="Details" onClick={(ev) => {
      // Links inside the details (data-goto) select what they name.
      const b = (ev.target as Element).closest<HTMLElement>("[data-goto]");
      if (b?.dataset.goto) onSelect(b.dataset.goto);
    }}>
      <div className="ihd">
        <span className="eb">{d.eyebrow}</span>
        <h4>{d.title}</h4>
        {d.pills && <div className="pills">{d.pills}</div>}
        <button type="button" className="x" aria-label="Close" onClick={() => onSelect(null)}><Icon name="x" /></button>
      </div>
      <div className="ibd">
        {d.body.map((b, i) => <Section key={i} block={b} onSelect={onSelect} />)}
      </div>
      {(d.links.length > 0 || d.expand) && (
        <div className="iacts">
          {d.expand && <button type="button" className="btn sm pri" onClick={() => onExpand(d.expand!)}>Expand project</button>}
          {d.links.map((l, i) => <RouterLink key={l.href} href={l.href} className={`btn sm${i === 0 && !d.expand ? " pri" : ""}`}>{l.label}</RouterLink>)}
        </div>
      )}
    </aside>
  );
}

type Block =
  | { note: string; tone: "bad" | "warn" | "info" }
  | { text: string }
  | { kv: [string, ReactNode][] }
  | { title: string; rows: Row[]; empty?: string }
  | { title: string; chips: { sel: string; label: string }[] }
  | { title: string; node: ReactNode };

type Detail = { eyebrow: string; title: string; pills?: ReactNode; body: Block[]; links: { label: string; href: string }[]; expand?: string };

function Section({ block: b, onSelect }: { block: Block; onSelect: (s: string) => void }) {
  if ("note" in b) return <div className={`imap-note ${b.tone}`}>{b.note}</div>;
  if ("text" in b) return <p>{b.text}</p>;
  if ("kv" in b) return <div className="kvs">{b.kv.map(([k, v]) => <div className="kv" key={k}><span>{k}</span><div>{v}</div></div>)}</div>;
  if ("rows" in b) {
    return (
      <div>
        <h5>{b.title}</h5>
        {b.rows.length === 0 ? <p className="empty">{b.empty ?? "None"}</p> : (
          <div className="flows">
            {b.rows.map((r, i) => (
              <button type="button" className="fl" key={`${r.sel}-${i}`} onClick={() => onSelect(r.sel)}>
                <i className={`sw ${r.kind ?? ""}`} /><span className="fn">{r.label}</span><span className={`fc ${r.tone ?? ""}`}>{r.count ?? ""}</span>
                {r.sub && <span className="fs">{r.sub}</span>}
              </button>
            ))}
          </div>
        )}
      </div>
    );
  }
  if ("chips" in b) {
    return (
      <div>
        <h5>{b.title}</h5>
        {b.chips.length === 0 ? <p className="empty">None</p> : (
          <div className="nodes">{b.chips.map((c) => <button type="button" className="lk" key={c.sel + c.label} onClick={() => onSelect(c.sel)}>{c.label}</button>)}</div>
        )}
      </div>
    );
  }
  return <div><h5>{b.title}</h5>{b.node}</div>;
}

const pill = (tone: string, text: string) => <span className={`pill ${tone}`}>{text}</span>;
const enc = encodeURIComponent;
const PHASE_TONE: Record<string, string> = { running: "ok", deploying: "warn", stopped: "mute", pending: "info", failed: "bad" };

/** An endpoint's name as the inspector lists it. */
function nameOf(m: Model, id: string, from?: string): string {
  if (id === "internet") return "Internet";
  if (id === "out") return "Internet (outbound)";
  if (id === "gateway") return "Gateway";
  if (id.startsWith("prj:")) return `every app of ${id.slice(4)}`;
  const e = m.byId.get(id);
  if (!e) return id;
  const near = from ? m.byId.get(from)?.project === e.project : true;
  return near ? e.name : `${e.project}/${e.name}`;
}

function linksOf(m: Model, id: string, dir: "in" | "out"): Row[] {
  const rows: Row[] = [];
  for (const l of m.links) {
    if ((dir === "in" ? l.to : l.from) !== id || l.kind === "mount" || l.kind === "runs") continue;
    const other = dir === "in" ? l.from : l.to;
    if (l.kind === "route") {
      if (dir === "in") rows.push({ sel: l.sel, kind: "route", label: nameOf(m, l.from), sub: "domain" });
      continue;
    }
    rows.push({
      sel: l.sel, kind: l.kind === "drop" ? "drop" : undefined, label: nameOf(m, other, id),
      sub: l.kind === "drop" ? "no rule allows this" : `${l.sel.startsWith("allow:") ? "allow list" : l.sel.split("/").pop()} · ${l.label ?? ""}`,
      count: l.kind === "drop" ? l.label : l.traffic !== undefined ? count(l.traffic) : "", tone: l.tone,
    });
  }
  // One row per rule and other end.
  return rows.filter((r, i) => rows.findIndex((x) => x.sel === r.sel && x.label === r.label) === i);
}

function detail(m: Model, sel: string): Detail | null {
  const t = m.topology;
  if (sel.startsWith("rule:")) {
    const r = t.rules.find((x) => `rule:${x.project}/${x.name}` === sel);
    if (!r) return null;
    const ends = (peers: typeof r.from, from: boolean) => peers.map((p, i) => {
      const id = p.app ? appId(p.app.includes("/") ? p.app.split("/")[0] : r.project, p.app.split("/").pop()!) : undefined;
      const label = p.app ? (p.app.includes("/") ? p.app : p.app) : p.project ? `${p.project}/*` : p.internet ? (from ? "internet (through the gateway)" : "internet") : p.cidr ?? "?";
      return id && m.byId.has(id) ? <button type="button" key={i} className="lk" data-goto={id}>{label}</button> : <span key={i} className="mono">{label}</span>;
    });
    return {
      eyebrow: `Traffic rule · ${r.project}`, title: r.name,
      pills: <>{r.phase === "ready" ? pill("ok", "Active") : pill(r.phase === "failed" ? "bad" : "info", r.phase)}{!!r.counts?.dropped && pill("warn", `${count(r.counts.dropped)} dropped`)}<span className="tag">{portsLabel(r.ports)}</span></>,
      body: [
        ...(r.description ? [{ text: r.description }] : []),
        ...(r.message && r.phase !== "ready" ? [{ note: r.message, tone: "warn" as const }] : []),
        { kv: [["From", <Ends key="f">{ends(r.from, true)}</Ends>], ["To", <Ends key="t">{ends(r.to, false)}</Ends>], ["Ports", portsLabel(r.ports)],
          ["Allowed", r.counts ? `${count(r.counts.allowed)} in the last ${t.hubble.window}` : "no counts yet"]] },
      ],
      links: [{ label: "Open traffic rules", href: "/network?tab=rules" }],
    };
  }
  if (sel.startsWith("allow:")) {
    const id = sel.slice(6), e = m.byId.get(id);
    if (!e?.app) return null;
    return {
      eyebrow: `Allow list · ${e.project}`, title: e.name, body: [
        { text: `Only these apps may connect to ${e.name}; the gateway always reaches its public ports.` },
        { title: "Allowed", chips: e.app.allowFrom.map((a) => ({ sel: a.includes("/") ? `app:${a}` : appId(e.project, a), label: a })) },
      ],
      links: [{ label: "Open app settings", href: `/apps/${enc(e.project)}/${enc(e.name)}?tab=settings` }],
    };
  }
  if (sel.startsWith("drop:")) {
    const d = t.drops[Number(sel.slice(5))];
    if (!d) return null;
    const port = d.port ? `${d.protocol ?? "TCP"} ${d.port}` : "any port";
    return {
      eyebrow: "Dropped traffic", title: `${sideLabel(d.from)} → ${sideLabel(d.to)}`, pills: pill("bad", `${count(d.count)} dropped`),
      body: [
        { note: `No traffic rule allows this: ${sideLabel(d.from)} tries to reach ${sideLabel(d.to)} on ${port}.`, tone: "bad" },
        { kv: [["Port", port], ["First seen", ago(d.first)], ["Last seen", ago(d.last)]] },
        ...(d.suggestion ? [{ title: "Suggested rule", node: <p className="mono imap-small">{d.suggestion.name} · {portsLabel(d.suggestion.spec.ports)}</p> }] : []),
      ],
      links: [{ label: d.suggestion ? "Create allow rule" : "Open traffic rules", href: "/network?tab=rules" }],
    };
  }
  if (sel.startsWith("srv:")) {
    const s = t.nodes.find((n) => `srv:${n.name}` === sel);
    if (!s) return null;
    return serverDetail(m, s);
  }
  if (sel === "fwband" || sel.startsWith("fw:")) {
    const rules = t.firewall ?? [];
    if (sel === "fwband") {
      return {
        eyebrow: "Server firewall", title: "Firewall", body: [
          { text: "Applies to every server; mirrored to the Hetzner Cloud Firewall for cloud servers when a Cloud token is set." },
          { title: "Rules", rows: rules.map((r) => ({ sel: `fw:${r.name}`, label: fwLabel(r), sub: r.description, count: openToAll(r) ? "any" : "limited" })) },
        ],
        links: [{ label: "Open firewall", href: "/network?tab=firewall" }],
      };
    }
    const r = rules.find((x) => `fw:${x.name}` === sel);
    if (!r) return null;
    return {
      eyebrow: "Server firewall", title: fwLabel(r), pills: <>{r.disabled ? pill("mute", "Disabled") : openToAll(r) ? pill("ok", "Open to anyone") : pill("info", "Limited")}{r.required && <span className="tag">required</span>}</>,
      body: [{ text: r.description }, { kv: [["Sources", <span key="s" className="mono">{describeSources(r.sources)}</span>], ["Servers", r.nodes === "all" ? "All" : "Control plane"]] }],
      links: [{ label: "Open firewall", href: "/network?tab=firewall" }],
    };
  }
  if (sel === "gateway" || sel === "internet") {
    const doms = m.entities.filter((e) => e.kind === "domain");
    return {
      eyebrow: sel === "gateway" ? "Gateway" : "Outside", title: sel === "gateway" ? "Gateway" : "Internet",
      body: [
        { text: sel === "gateway" ? `Terminates TLS for ${doms.length} ${doms.length === 1 ? "domain" : "domains"} and routes each hostname to its app. It runs on every server.`
          : "Reaches the cluster only through firewall ports open to anyone: web traffic on 80 and 443 to the gateway." },
        { title: "Domains", rows: doms.map((e) => ({ sel: e.id, kind: "route" as const, label: e.name, sub: e.domain?.app ? `→ ${e.project}/${e.domain.app}` : "no app yet", count: e.domain?.certificate })) },
      ],
      links: [{ label: "Domains & TLS", href: "/network?tab=domains" }],
    };
  }
  if (sel === "out") {
    return {
      eyebrow: "Outbound", title: "Internet",
      body: [
        { text: "Apps reach the internet as their outbound setting (none, HTTPS, all) and traffic rules allow; everything else is dropped." },
        { title: "Allowed by rules", rows: linksOf(m, "out", "in") },
      ],
      links: [{ label: "Open traffic rules", href: "/network?tab=rules" }],
    };
  }
  if (sel.startsWith("prj:")) {
    const p = sel.slice(4), members = m.entities.filter((e) => e.project === p && e.kind !== "domain");
    const proj = t.projects.find((x) => x.name === p);
    return {
      eyebrow: "Project", title: p, pills: proj?.isolated ? pill("info", "Isolated") : undefined,
      body: [
        { text: "Collapsed: connections to and from its apps are drawn to this box." },
        { title: "Contains", chips: members.map((e) => ({ sel: e.id, label: e.name })) },
      ],
      links: [], expand: p,
    };
  }
  const e = m.byId.get(sel);
  if (!e) return null;
  return entityDetail(m, e);
}

function Ends({ children }: { children: ReactNode[] }) {
  return <span className="imap-ends">{children.map((c, i) => <span key={i}>{i > 0 && ", "}{c}</span>)}</span>;
}

function serverDetail(m: Model, s: TopoNode): Detail {
  const pods = m.topology.apps.flatMap((a) => a.pods.filter((p) => p.node === s.name).map(() => a));
  const vols = m.topology.volumes.filter((v) => v.node === s.name);
  const counted = new Map<string, number>();
  for (const a of pods) counted.set(appId(a.project, a.name), (counted.get(appId(a.project, a.name)) ?? 0) + 1);
  const pct = (a: number, b: number) => (b ? Math.round((100 * a) / b) : 0);
  const kv: [string, ReactNode][] = [];
  if (!m.topology.nodesPartial) {
    kv.push(["Type", [s.serverType?.toUpperCase(), s.platform === "dedicated" ? "Dedicated" : s.platform === "cloud" ? "Cloud" : undefined].filter(Boolean).join(" · ") || "—"]);
    kv.push(["Role", s.roles.join(", ") || "worker"]);
    if (s.externalIp) kv.push(["Public IP", <span key="ip" className="mono">{s.externalIp}</span>]);
    if (s.internalIp) kv.push(["Private IP", <span key="pip" className="mono">{s.internalIp}</span>]);
    if (s.usage) {
      kv.push(["CPU", <Meter key="c" pct={pct(s.usage.cpu, s.usage.cpuCapacity)} text={`${s.usage.cpu.toFixed(1)} of ${s.usage.cpuCapacity} cores`} />]);
      kv.push(["Memory", <Meter key="m" pct={pct(s.usage.memory, s.usage.memoryCapacity)} text={`${gib(s.usage.memory)} of ${gib(s.usage.memoryCapacity)} GiB`} />]);
    }
  }
  return {
    eyebrow: "Server", title: s.name,
    pills: <>{s.ready ? pill("ok", s.status) : pill("bad", s.status)}{s.unschedulable && pill("warn", "Cordoned")}</>,
    body: [
      ...(m.topology.nodesPartial ? [{ text: "Only owners and admins see servers' details; this one runs pods of your projects." }] : []),
      ...(kv.length ? [{ kv }] : []),
      { title: `Runs ${pods.length} ${pods.length === 1 ? "pod" : "pods"}`, chips: [...counted].map(([id, n]) => ({ sel: id, label: `${m.byId.get(id)?.name ?? id}${n > 1 ? ` ×${n}` : ""}` })) },
      { title: "Volumes on this server", chips: vols.map((v) => ({ sel: `vol:${v.project}/${v.name}`, label: m.byId.get(`vol:${v.project}/${v.name}`)?.name ?? v.name })) },
    ],
    links: m.topology.nodesPartial ? [] : [{ label: "Open nodes", href: `/clusters/${enc(m.topology.cluster)}/nodes` }],
  };
}

function Meter({ pct, text }: { pct: number; text: string }) {
  return <>{text}<div className="imap-meter"><i className={pct >= 90 ? "bad" : pct >= 80 ? "warn" : ""} style={{ width: `${Math.min(100, pct)}%` }} /></div></>;
}

function entityDetail(m: Model, e: Entity): Detail {
  const t = m.topology;
  const proj = t.projects.find((p) => p.name === e.project);
  if (e.kind === "app" && e.app) {
    const a = e.app;
    const vols = m.links.filter((l) => l.kind === "mount" && l.from === e.id).map((l) => m.byId.get(l.to)).filter((v): v is Entity => !!v);
    const bad = a.pods.find((p) => p.tone === "bad");
    const nodes = new Map<string, number>();
    for (const p of a.pods) if (p.node) nodes.set(p.node, (nodes.get(p.node) ?? 0) + 1);
    const jobs = m.links.filter((l) => l.kind === "runs" && l.to === e.id).map((l) => m.byId.get(l.from)).filter((j): j is Entity => !!j);
    const href = `/apps/${enc(a.project)}/${enc(a.name)}`;
    return {
      eyebrow: `App · ${a.project}`, title: a.name,
      pills: <>{bad ? pill("bad", bad.status) : pill(PHASE_TONE[a.phase] ?? "mute", a.phase)}<span className="tag">{a.readyReplicas}/{a.replicas} ready</span></>,
      body: [
        ...(bad ? [{ note: `${bad.restarts} ${bad.restarts === 1 ? "restart" : "restarts"} of ${bad.name}. ${a.message ?? ""}`.trim(), tone: "bad" as const }] : a.phase === "failed" && a.message ? [{ note: a.message, tone: "bad" as const }] : []),
        { kv: [
          ["Source", a.source.type === "git" ? `Git · ${a.source.repository?.replace(/^https?:\/\//, "")} · ${a.source.branch}` : `Image · ${a.image}`],
          ["Revision", a.revision ? `${a.revision} · ${ago(a.updated)}` : "—"],
          ["Ports", a.ports.length ? a.ports.map((p) => `${p.container}${p.public ? ` → ${p.public}` : ""}`).join(", ") : "none"],
          ["Outbound", a.egress === "none" ? "No internet" : a.egress === "all" ? "Any port" : "HTTPS only"],
          ["Replicas", proj?.placed === false ? "Your role does not show pods here" : nodes.size ? <span className="imap-ends">{[...nodes].map(([n, k]) => <button type="button" key={n} className="lk" data-goto={`srv:${n}`}>{n}{k > 1 ? ` ×${k}` : ""}</button>)}</span> : "not running"],
        ] },
        { title: "Reached from", rows: linksOf(m, e.id, "in") },
        { title: "Talks to", rows: linksOf(m, e.id, "out") },
        ...(vols.length ? [{ title: "Volumes", rows: vols.map((v) => ({ sel: v.id, kind: "mount" as const, label: v.name, sub: m.links.find((l) => l.kind === "mount" && l.from === e.id && l.to === v.id)?.label, count: v.volume && volumeFill(v.volume) !== undefined ? `${Math.round(volumeFill(v.volume)! * 100)}%` : "", tone: v.status === "warn" || v.status === "bad" ? v.status as "warn" | "bad" : undefined })) }] : []),
        ...(jobs.length ? [{ title: "Jobs running as this app", chips: jobs.map((j) => ({ sel: j.id, label: j.name })) }] : []),
      ],
      links: [
        ...(bad || a.phase === "failed" ? [{ label: "Open logs", href: appLogsHref(a.project, a.name) }, { label: "Open app", href }] : [{ label: "Open app", href }, { label: "Logs", href: appLogsHref(a.project, a.name) }]),
      ],
    };
  }
  if ((e.kind === "job" && e.schedule) || (e.kind === "task" && e.task)) {
    const s = e.schedule, k = e.task ?? s?.lastRun;
    const node = s?.node ?? e.task?.node;
    return {
      eyebrow: `${s ? "Scheduled job" : "Task"} · ${e.project}`, title: e.name,
      pills: s ? (s.suspend ? pill("mute", "Suspended") : e.status === "bad" ? pill("bad", s.phase) : e.status === "warn" ? pill("warn", "Last run failed") : pill("ok", s.lastRun ? "Last run passed" : "Scheduled"))
        : pill(e.status === "bad" ? "bad" : e.status === "ok" ? "ok" : "info", k?.phase ?? ""),
      body: [
        { kv: [
          ...(s ? [["Schedule", `${s.schedule}${s.timeZone ? ` (${s.timeZone})` : ""}`] as [string, ReactNode]] : []),
          ["Runs as", (s?.fromApp ?? e.task?.fromApp) ? <button type="button" className="lk" data-goto={appId(e.project, (s?.fromApp ?? e.task?.fromApp)!)}>{s?.fromApp ?? e.task?.fromApp}</button> : (s?.image ?? e.task?.image ?? "its own image")],
          ["Last run", k ? `${k.phase}${k.phase === "failed" && runEnd(k, true) ? ` (${runEnd(k, true)})` : ""} · ${ago(k.finished ?? k.created)}` : "not yet"],
          ...(s?.nextRun ? [["Next run", when(s.nextRun)] as [string, ReactNode]] : []),
          ...(node ? [["Ran on", <button type="button" key="n" className="lk" data-goto={`srv:${node}`}>{node}</button>] as [string, ReactNode]] : []),
        ] },
      ],
      links: s ? [{ label: "Open job", href: `/jobs/${enc(e.project)}/schedules/${enc(e.name)}` }] : [{ label: "Open task", href: `/jobs/${enc(e.project)}/tasks/${enc(e.name)}` }],
    };
  }
  if (e.kind === "volume" && e.volume) {
    const v = e.volume, f = volumeFill(v);
    return {
      eyebrow: `${v.own ? "Own disk" : "Volume"} · ${e.project}`, title: e.name,
      pills: <>{e.status === "ok" ? pill("ok", v.phase) : pill(e.status, f !== undefined && f >= 0.8 ? `${Math.round(f * 100)}% full` : v.phase)}<span className="tag">{v.size}</span></>,
      body: [
        { kv: [
          ["Used", f !== undefined && v.usedBytes !== undefined && v.capacityBytes ? <Meter key="u" pct={Math.round(f * 100)} text={`${gib(v.usedBytes)} of ${gib(v.capacityBytes)} GiB`} /> : t.metrics ? "no reading yet" : "monitoring is off"],
          ["Storage", v.class === "hcloud-volume" ? "Hetzner Volume" : "Local NVMe"],
          ["On server", v.node ? <button type="button" key="n" className="lk" data-goto={`srv:${v.node}`}>{v.node}</button> : "not attached"],
        ] },
        { title: "Mounted by", rows: m.links.filter((l) => l.kind === "mount" && l.to === e.id).map((l) => ({ sel: l.from, kind: "mount" as const, label: nameOf(m, l.from), sub: l.label })) },
      ],
      links: v.own && v.app ? [{ label: "Open app", href: `/apps/${enc(e.project)}/${enc(v.app)}` }] : [{ label: "Open volumes", href: `/apps/volumes?project=${enc(e.project)}` }],
    };
  }
  if (e.kind === "domain" && e.domain) {
    const d = e.domain;
    return {
      eyebrow: `Domain · ${e.project}`, title: d.hostname,
      pills: d.certificate === "valid" ? pill("ok", "Certificate valid") : pill(e.status, `Certificate ${d.certificate}`),
      body: [
        { kv: [
          ["Routes to", d.app ? <button type="button" key="a" className="lk" data-goto={appId(d.project, d.app)}>{d.app}</button> : "no app claims it yet"],
          ["Expires", d.notAfter ? when(d.notAfter) : "—"],
        ] },
        ...(d.message && d.certificate !== "valid" ? [{ note: d.message, tone: (d.certificate === "failed" ? "bad" : "warn") as "bad" | "warn" }] : []),
      ],
      links: [{ label: "Domains & TLS", href: "/network?tab=domains" }],
    };
  }
  return { eyebrow: e.kind, title: e.name, body: [], links: [] };
}
