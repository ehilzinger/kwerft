// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// The Overview's infrastructure map (docs/plan.md, "Overview map"): one
// cluster's apps, jobs, volumes, domains, traffic rules and servers from
// GET /topology, turned into a model of entities and links, and laid out
// for two lenses:
//   - traffic: internet → firewall → gateway → domains → apps (grouped by
//     project, in columns by who calls whom) → volumes, outbound internet;
//   - placement: the same things on the servers that run them.
// The layout is deterministic (fixed columns, not force-directed) so the
// picture stays put across polls. Everything here is pure; the component
// (components/InfraMap.tsx) draws it.
import { request } from "./api";
import { clusterQuery } from "./clusters";
import { describePorts, type FirewallRule } from "./firewall";
import { runEnd, type Domain, type ScheduleSummary, type TaskSummary, type Volume } from "./jobs";
import type { ClusterNode } from "./nodes";
import { count, portsLabel, type Drop, type HubbleStatus, type Side, type TrafficPeer, type TrafficRule } from "./traffic";
import type { AppPort, AppSummary } from "./workloads";

// ---- the API ------------------------------------------------------------------

export type TopoPod = { name: string; node?: string; status: string; tone: "ok" | "warn" | "bad" | "mute"; ready: boolean; restarts: number };
export type TopoMount = { volume: string; path: string; readOnly: boolean };
export type TopoApp = AppSummary & { ports: AppPort[]; allowFrom: string[]; egress: "none" | "https" | "all"; mounts: TopoMount[]; pods: TopoPod[] };
export type TopoSchedule = ScheduleSummary & { node?: string };
export type TopoTask = TaskSummary & { node?: string };
/** own: an app's own disk ("<app>/data-<n>"), not a Volume object. */
export type TopoVolume = Volume & { own: boolean; app?: string; node?: string; usedBytes?: number; capacityBytes?: number };
export type TopoRule = TrafficRule & { project: string };
export type NodeUsage = { cpu: number; cpuCapacity: number; memory: number; memoryCapacity: number };
export type TopoNode = ClusterNode & { serverType?: string; usage?: NodeUsage };
export type TopoProject = { name: string; isolated: boolean; placed: boolean };

export type Topology = {
  cluster: string;
  projects: TopoProject[];
  apps: TopoApp[];
  schedules: TopoSchedule[];
  tasks: TopoTask[];
  volumes: TopoVolume[];
  domains: Domain[];
  rules: TopoRule[];
  drops: Drop[];
  hubble: HubbleStatus;
  /** Owners and admins: every server. Others: the servers their pods run on, by name (nodesPartial). */
  nodes: TopoNode[];
  nodesPartial: boolean;
  /** null: the firewall is for owners and admins. */
  firewall: FirewallRule[] | null;
  metrics: boolean;
};

export const topologyKey = (cluster?: string) => ["topology", cluster ?? "local"] as const;
export const topologyApi = {
  get: (cluster?: string) => request<Topology>(`/topology${clusterQuery(cluster)}`),
};

// ---- the model ----------------------------------------------------------------

export type Tone = "ok" | "warn" | "bad" | "info" | "mute";
export type EntityKind = "app" | "job" | "task" | "volume" | "domain";

export type Entity = {
  /** app:<project>/<name>, job:…, task:…, vol:…, dom:<project>/<domain object>. */
  id: string;
  kind: EntityKind;
  name: string;
  project: string;
  status: Tone;
  /** The chip's second line. */
  sub: string;
  /** Traffic lens: column (0 callers … 2 called) and place in it. */
  col: number;
  order: number;
  app?: TopoApp;
  schedule?: TopoSchedule;
  task?: TopoTask;
  volume?: TopoVolume;
  domain?: Domain;
};

export type LinkKind = "route" | "rule" | "drop" | "mount" | "runs";

export type Link = {
  id: string;
  /** Entity ids, or internet (sources), out (outbound internet), gateway, prj:<project>. */
  from: string;
  to: string;
  kind: LinkKind;
  /** What selecting the link selects: a rule, a drop, a domain, a volume, a job. */
  sel: string;
  label?: string;
  /** Allowed (or dropped) connections in the window; drives the flow animation. */
  traffic?: number;
  tone?: "warn" | "bad";
  tip: string;
};

export type Model = {
  topology: Topology;
  entities: Entity[];
  byId: Map<string, Entity>;
  links: Link[];
};

export const appId = (project: string, name: string) => `app:${project}/${name}`;
export const volumeId = (project: string, name: string) => `vol:${project}/${name}`;
export const ruleSel = (r: { project: string; name: string }) => `rule:${r.project}/${r.name}`;

const RANK: Record<Tone, number> = { ok: 0, mute: 0, info: 1, warn: 2, bad: 3 };
export const worst = (tones: Tone[]): Tone => tones.reduce<Tone>((w, t) => (RANK[t] > RANK[w] ? t : w), "ok");

export function appTone(a: TopoApp): Tone {
  if (a.phase === "failed" || a.pods.some((p) => p.tone === "bad")) return "bad";
  if (a.phase === "stopped") return "mute";
  if (a.phase === "pending") return "info";
  if (a.phase === "deploying" || a.readyReplicas < a.replicas) return "warn";
  return "ok";
}

function appSub(a: TopoApp): string {
  const bad = a.pods.find((p) => p.tone === "bad");
  if (bad) return `${bad.status} · ${a.readyReplicas}/${a.replicas}`;
  if (a.phase === "stopped") return "stopped";
  return `${a.readyReplicas}/${a.replicas} ready${a.revision ? ` · rev ${a.revision}` : ""}`;
}

export function scheduleTone(s: TopoSchedule): Tone {
  if (s.suspend) return "mute";
  if (s.phase === "failed") return "bad";
  if (s.lastRun?.phase === "failed" && s.lastRun.reason !== "Cancelled") return "warn";
  return "ok";
}

export function taskTone(t: TaskSummary): Tone {
  return t.phase === "failed" ? "bad" : t.phase === "succeeded" ? "ok" : "info";
}

/** Used share of a volume, 0–1, when monitoring says. */
export function volumeFill(v: TopoVolume): number | undefined {
  if (v.usedBytes === undefined || !v.capacityBytes) return undefined;
  return Math.min(1, v.usedBytes / v.capacityBytes);
}

export function volumeTone(v: TopoVolume): Tone {
  if (v.phase === "failed" || v.phase === "lost") return "bad";
  if (v.phase === "pending" || v.phase === "deleting") return "info";
  const f = volumeFill(v);
  if (f !== undefined && f >= 0.9) return "bad";
  if (f !== undefined && f >= 0.8) return "warn";
  return "ok";
}

export const gib = (bytes: number) => {
  const g = bytes / 2 ** 30;
  return g >= 100 ? g.toFixed(0) : g >= 10 ? g.toFixed(1).replace(/\.0$/, "") : g.toFixed(2).replace(/0$/, "").replace(/\.0$/, "");
};

function volumeSub(v: TopoVolume): string {
  const f = volumeFill(v);
  if (f !== undefined && v.usedBytes !== undefined && v.capacityBytes) return `${gib(v.usedBytes)} of ${gib(v.capacityBytes)} GiB · ${Math.round(f * 100)}%`;
  return `${v.size}${v.own ? " · own disk" : ""}`;
}

export const domainTone = (d: Domain): Tone =>
  d.certificate === "valid" ? "ok" : d.certificate === "failed" ? "bad" : d.certificate === "disabled" ? "mute" : "info";

/** A volume's name on the map: an own disk is "<app> disk". */
export const volumeName = (v: TopoVolume) => (v.own && v.app ? `${v.app} disk${v.name.endsWith("/data-0") ? "" : ` ${v.name.split("-").pop()}`}` : v.name);

/** Where a rule peer is on the map; from: the peer is a source. */
export function peerId(p: TrafficPeer, project: string, from: boolean): string | undefined {
  if (p.app) {
    const [proj, name] = p.app.includes("/") ? p.app.split("/", 2) : [project, p.app];
    return appId(proj, name);
  }
  if (p.project) return `prj:${p.project}`;
  // The internet reaches apps through the gateway; outbound it is its own node.
  if (p.internet) return from ? "gateway" : "out";
  if (p.cidr) return from ? "internet" : "out";
  return undefined;
}

/** A drop's side on the map, when it is something the map draws. */
export function sideId(s: Side, from: boolean, apps: Set<string>): string | undefined {
  if (s.kind === "pod" && s.namespace && s.app) {
    const id = appId(s.namespace, s.app);
    return apps.has(id) ? id : undefined;
  }
  if (s.kind === "world") return from ? "internet" : "out";
  if (s.kind === "host" || s.kind === "remote-node") return "gateway";
  return undefined;
}

const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? "" : "s"}`;

export function buildModel(t: Topology): Model {
  const entities: Entity[] = [];
  const add = (e: Omit<Entity, "col" | "order">) => entities.push({ ...e, col: 0, order: 0 });
  for (const a of t.apps) add({ id: appId(a.project, a.name), kind: "app", name: a.name, project: a.project, status: appTone(a), sub: appSub(a), app: a });
  for (const s of t.schedules) {
    const last = s.lastRun ? `${s.lastRun.phase === "failed" ? "failed" : s.lastRun.phase === "succeeded" ? "ok" : s.lastRun.phase}` : "not run yet";
    add({ id: `job:${s.project}/${s.name}`, kind: "job", name: s.name, project: s.project, status: scheduleTone(s), sub: s.suspend ? "suspended" : `${s.schedule} · ${last}`, schedule: s });
  }
  for (const k of t.tasks) add({ id: `task:${k.project}/${k.name}`, kind: "task", name: k.name, project: k.project, status: taskTone(k), sub: k.phase === "failed" && runEnd(k, true) ? `failed · ${runEnd(k, true)}` : k.phase, task: k });
  for (const v of t.volumes) add({ id: volumeId(v.project, v.name), kind: "volume", name: volumeName(v), project: v.project, status: volumeTone(v), sub: volumeSub(v), volume: v });
  for (const d of t.domains) add({ id: `dom:${d.project}/${d.name}`, kind: "domain", name: d.hostname, project: d.project, status: domainTone(d), sub: d.certificate, domain: d });
  const byId = new Map(entities.map((e) => [e.id, e]));
  const apps = new Set(t.apps.map((a) => appId(a.project, a.name)));
  const projects = new Set(t.projects.map((p) => p.name));

  const links: Link[] = [];
  const seen = new Set<string>();
  const link = (l: Link) => {
    const ok = (id: string) => byId.has(id) || id === "internet" || id === "out" || id === "gateway" || (id.startsWith("prj:") && projects.has(id.slice(4)));
    if (!ok(l.from) || !ok(l.to) || l.from === l.to || seen.has(l.id)) return;
    seen.add(l.id);
    links.push(l);
  };

  for (const d of t.domains) {
    const id = `dom:${d.project}/${d.name}`;
    const tip = `${d.hostname}${d.app ? ` → ${d.project}/${d.app}` : ""} · certificate ${d.certificate}`;
    link({ id: `g:${id}`, from: "gateway", to: id, kind: "route", sel: id, tip });
    if (d.app) link({ id: `r:${id}`, from: id, to: appId(d.project, d.app), kind: "route", sel: id, tip });
  }
  for (const r of t.rules) {
    if (r.disabled) continue;
    const sel = ruleSel(r), label = portsLabel(r.ports);
    const tip = `${r.name} · ${label} · ${r.counts ? `${count(r.counts.allowed)} allowed${r.counts.dropped ? `, ${count(r.counts.dropped)} dropped` : ""} in the last hour` : "no counts"}`;
    r.from.forEach((f, i) => r.to.forEach((to, j) => {
      const a = peerId(f, r.project, true), b = peerId(to, r.project, false);
      if (a && b) link({ id: `${sel}:${i}:${j}`, from: a, to: b, kind: "rule", sel, label, traffic: r.counts?.allowed, tone: r.counts?.dropped ? "warn" : undefined, tip });
    }));
  }
  for (const a of t.apps) {
    const id = appId(a.project, a.name);
    for (const from of a.allowFrom) {
      const [proj, name] = from.includes("/") ? from.split("/", 2) : [a.project, from];
      link({ id: `allow:${id}:${from}`, from: appId(proj, name), to: id, kind: "rule", sel: `allow:${id}`, label: "allow list", tip: `${proj}/${name} may connect to ${a.name} (the app's allow list)` });
    }
    for (const m of a.mounts) {
      const v = volumeId(a.project, m.volume);
      link({ id: `m:${id}:${v}`, from: id, to: v, kind: "mount", sel: v, label: m.path, tip: `${a.name} mounts ${byId.get(v)?.name ?? m.volume} at ${m.path}${m.readOnly ? " (read-only)" : ""}` });
    }
  }
  t.drops.forEach((d, i) => {
    const a = sideId(d.from, true, apps), b = sideId(d.to, false, apps);
    if (!a || !b) return;
    const port = d.port ? `${d.protocol ?? "TCP"} ${d.port}` : "";
    link({ id: `drop:${i}`, from: a, to: b, kind: "drop", sel: `drop:${i}`, label: `${count(d.count)} dropped`, traffic: d.count, tone: "bad",
      tip: `${count(d.count)} connections dropped${port ? ` on ${port}` : ""}: no rule allows this` });
  });
  for (const s of t.schedules) if (s.fromApp) {
    const id = `job:${s.project}/${s.name}`;
    link({ id: `runs:${id}`, from: id, to: appId(s.project, s.fromApp), kind: "runs", sel: id, tip: `${s.name} runs as ${s.fromApp}: its image, settings and traffic rules` });
  }
  for (const k of t.tasks) if (k.fromApp) {
    const id = `task:${k.project}/${k.name}`;
    link({ id: `runs:${id}`, from: id, to: appId(k.project, k.fromApp), kind: "runs", sel: id, tip: `${k.name} runs as ${k.fromApp}` });
  }

  assignColumns(entities, links);
  return { topology: t, entities, byId, links };
}

/**
 * Columns within a project: apps nobody in the project calls on the left
 * (frontends, workers), apps that only get called on the right (databases),
 * the rest in the middle; jobs and tasks with the callers. A project without
 * links among its apps is a plain grid of three columns.
 */
export function assignColumns(entities: Entity[], links: Link[]) {
  const byProject = new Map<string, Entity[]>();
  for (const e of entities) if (e.kind === "app" || e.kind === "job" || e.kind === "task") byProject.set(e.project, [...(byProject.get(e.project) ?? []), e]);
  for (const members of byProject.values()) {
    const ids = new Set(members.map((e) => e.id));
    const inner = links.filter((l) => (l.kind === "rule" || l.kind === "drop") && ids.has(l.from) && ids.has(l.to));
    const order = (a: Entity, b: Entity) => kindOrder(a) - kindOrder(b) || a.name.localeCompare(b.name);
    if (inner.length === 0) {
      [...members].sort(order).forEach((e, i) => { e.col = i % 3; e.order = Math.floor(i / 3); });
      continue;
    }
    const ins = new Map<string, string[]>(), outs = new Map<string, number>();
    for (const l of inner) {
      ins.set(l.to, [...(ins.get(l.to) ?? []), l.from]);
      outs.set(l.from, (outs.get(l.from) ?? 0) + 1);
    }
    for (const e of members) {
      const i = ins.get(e.id)?.length ?? 0, o = outs.get(e.id) ?? 0;
      e.col = e.kind !== "app" || i === 0 ? 0 : o === 0 ? 2 : 1;
    }
    // Left to right, each column ordered by where its callers sit.
    const pos = new Map<string, number>();
    for (const col of [0, 1, 2]) {
      const list = members.filter((e) => e.col === col);
      const bary = (e: Entity) => {
        const from = (ins.get(e.id) ?? []).map((f) => pos.get(f)).filter((p): p is number => p !== undefined);
        return from.length ? from.reduce((s, p) => s + p, 0) / from.length : Infinity;
      };
      list.sort((a, b) => (col === 0 ? order(a, b) : bary(a) - bary(b) || order(a, b)));
      list.forEach((e, i) => { e.order = i; pos.set(e.id, i); });
    }
  }
}
const kindOrder = (e: Entity) => (e.kind === "app" ? 0 : e.kind === "job" ? 1 : 2);

// ---- the layout ---------------------------------------------------------------

export type Lens = "traffic" | "placement";
export type Layers = { jobs: boolean; volumes: boolean; domains: boolean; rules: boolean; firewall: boolean };
export type View = { lens: Lens; layers: Layers; project?: string; collapsed: Set<string> };

export type ItemKind = EntityKind | "gateway" | "net" | "fw" | "fwband" | "pbox" | "pchip" | "server";

export type Item = {
  key: string;
  kind: ItemKind;
  /** Centre and size, in map units. */
  x: number;
  y: number;
  w: number;
  h: number;
  /** Opacity: below 1 only while the lens changes. */
  op: number;
  /** What clicking it selects (null: nothing, e.g. a project box). */
  sel: string | null;
  label: string;
  status?: Tone;
  entity?: Entity;
  /** Placement: which of the app's pods this chip is. */
  pod?: { i: number; n: number; pod?: TopoPod };
  /** Placement: a job or task drawn where it last ran. */
  dashed?: boolean;
  /** Placement: the server it sits on. */
  node?: string;
  server?: TopoNode;
  project?: string;
  counts?: string;
  collapsed?: boolean;
  fw?: FirewallRule;
  out?: boolean;
};

export type Edge = Omit<Link, "from" | "to" | "kind"> & { a: string; b: string; kind: LinkKind | "pub" | "ssh"; op?: number };

export type Layout = { items: Record<string, Item>; edges: Edge[] };

export const CHIP = { w: 164, h: 40 };
const X = { net: 64, fw: 172, gw: 282, dom: 414, cols: [600, 770, 940], box: [505, 1035] as const, vol: 1135, out: 1270 };
const ROW = 60, BOX_HEAD = 64, BOX_GAP = 22, SERVER_W = 230, SERVER_GAP = 16, SLOT = 52;
/** Placement: the gateway's and SSH's lines to the servers run in lanes this far above the server cards. */
export const LANE = { route: 18, ssh: 36 };
/** Where those lines turn up into their lane, left of the first server; SSH's turn lies further right, so the two cross once. */
const TURN = { route: X.box[0] - 120, ssh: X.box[0] - 90 };

/** Above this many apps the map opens with every project collapsed. */
export const COLLAPSE_ABOVE = 40;

/** Open to the whole internet: no sources, or 0.0.0.0/0. */
export const openToAll = (r: FirewallRule) => r.sources.length === 0 || r.sources.some((s) => s === "0.0.0.0/0" || s === "::/0");
const isWeb = (r: FirewallRule) => r.protocol === "TCP" && (r.port === 80 || r.port === 443) && !r.endPort;
export const fwLabel = (r: FirewallRule) => (r.protocol === "ICMP" ? "icmp" : `${describePorts(r)}/${r.protocol.toLowerCase()}`);

function resolve<T extends { y: number }>(list: T[], gap: number): T[] {
  list.sort((a, b) => a.y - b.y);
  for (let i = 1; i < list.length; i++) if (list[i].y < list[i - 1].y + gap) list[i].y = list[i - 1].y + gap;
  return list;
}
const avg = (a: number[]) => a.reduce((s, x) => s + x, 0) / a.length;

export function visible(e: Entity | undefined, v: View): boolean {
  if (!e) return false;
  if (v.project && e.project !== v.project) return false;
  if ((e.kind === "job" || e.kind === "task") && !v.layers.jobs) return false;
  if (e.kind === "volume" && !v.layers.volumes) return false;
  if (e.kind === "domain" && !v.layers.domains) return false;
  return true;
}

export function layout(m: Model, v: View): Layout {
  const items: Record<string, Item> = {};
  const edges: Edge[] = [];
  const add = (it: Omit<Item, "op">) => (items[it.key] = { op: 1, ...it });
  const edge = (e: Edge) => {
    if (!items[e.a] || !items[e.b] || e.a === e.b) return;
    if (edges.some((x) => x.a === e.a && x.b === e.b && x.kind === e.kind)) return;
    edges.push(e);
  };
  const t = m.topology;
  const traffic = v.lens === "traffic";
  const projects = t.projects.filter((p) => !v.project || p.name === v.project);
  /** Where an entity (or endpoint) is drawn now. */
  const at = (id: string): string | undefined => {
    if (id === "internet" || id === "out" || id === "gateway") return id;
    if (id.startsWith("prj:")) {
      const p = id.slice(4);
      if (!traffic) return undefined;
      return v.collapsed.has(p) ? id : `box:${p}`;
    }
    const e = m.byId.get(id);
    if (!visible(e, v)) return undefined;
    return traffic && v.collapsed.has(e!.project) && e!.kind !== "domain" ? `prj:${e!.project}` : id;
  };
  let bottom = 0, midY = 0;
  const firewall = v.layers.firewall && t.firewall ? t.firewall.filter((r) => !r.disabled) : [];

  if (traffic) {
    let top = 24;
    for (const p of projects) {
      const all = m.entities.filter((e) => e.project === p.name && e.kind !== "domain");
      const members = all.filter((e) => (e.kind === "app" || e.kind === "job" || e.kind === "task") && visible(e, v));
      if (!members.length) continue;
      const n = (k: EntityKind[]) => all.filter((e) => k.includes(e.kind)).length;
      const counts = [plural(n(["app"]), "app"), plural(n(["job", "task"]), "job"), plural(n(["volume"]), "volume")].join(" · ");
      const [x0, x1] = X.box;
      if (v.collapsed.has(p.name)) {
        add({ key: `box:${p.name}`, kind: "pbox", x: (x0 + x1) / 2, y: top + 46, w: x1 - x0, h: 92, sel: null, label: p.name, project: p.name, counts, collapsed: true });
        add({ key: `prj:${p.name}`, kind: "pchip", x: X.cols[1], y: top + 58, w: 210, h: CHIP.h, sel: `prj:${p.name}`, label: p.name, project: p.name, counts, status: worst(all.map((e) => e.status)) });
        top += 92 + BOX_GAP;
        continue;
      }
      // Apps in their columns, compacted after filtering; jobs and tasks in a
      // grid below them, so a project with many jobs stays compact. A project
      // without links among its apps is one grid already (assignColumns).
      const linked = members.some((e) => e.kind === "app" && e.col > 0);
      const grid = linked ? members.filter((e) => e.kind !== "app").sort((a, b) => a.order - b.order || a.name.localeCompare(b.name)) : [];
      const rows = [0, 1, 2].map((c) => members.filter((e) => e.col === c && !grid.includes(e)).sort((a, b) => a.order - b.order));
      const appDepth = Math.max(...rows.map((r) => r.length));
      const place = (e: Entity, c: number, i: number) =>
        add({ key: e.id, kind: e.kind, x: X.cols[c], y: top + BOX_HEAD + i * ROW, w: CHIP.w, h: CHIP.h, sel: e.id, label: e.name, status: e.status, entity: e });
      rows.forEach((list, c) => list.forEach((e, i) => place(e, c, i)));
      grid.forEach((e, k) => place(e, k % 3, appDepth + Math.floor(k / 3)));
      const depth = appDepth + Math.ceil(grid.length / 3);
      const h = BOX_HEAD + (depth - 1) * ROW + 40;
      add({ key: `box:${p.name}`, kind: "pbox", x: (x0 + x1) / 2, y: top + h / 2, w: x1 - x0, h, sel: null, label: p.name, project: p.name, counts });
      top += h + BOX_GAP;
    }
    bottom = Math.max(top - BOX_GAP, 140);

    // Volumes beside who mounts them; unmounted ones at the bottom.
    const mounters = new Map<string, string[]>();
    for (const l of m.links) if (l.kind === "mount") mounters.set(l.to, [...(mounters.get(l.to) ?? []), l.from]);
    const vols = m.entities.filter((e) => e.kind === "volume" && visible(e, v) && !v.collapsed.has(e.project)).map((e) => {
      const ys = (mounters.get(e.id) ?? []).map((id) => items[id]?.y).filter((y): y is number => y !== undefined);
      return { e, y: ys.length ? avg(ys) : bottom };
    });
    for (const o of resolve(vols, 56)) add({ key: o.e.id, kind: "volume", x: X.vol, y: o.y, w: 180, h: 46, sel: o.e.id, label: o.e.name, status: o.e.status, entity: o.e });

    // Domains level with their app.
    const doms = m.entities.filter((e) => e.kind === "domain" && visible(e, v)).map((e) => {
      const target = e.domain?.app ? at(appId(e.project, e.domain.app)) : undefined;
      return { e, y: target && items[target] ? items[target].y : bottom };
    });
    for (const o of resolve(doms, 38)) add({ key: o.e.id, kind: "domain", x: X.dom, y: o.y, w: 150, h: 30, sel: o.e.id, label: o.e.name, status: o.e.status, entity: o.e });
    const domYs = Object.values(items).filter((i) => i.kind === "domain").map((i) => i.y);
    midY = domYs.length ? avg(domYs) : (24 + bottom) / 2;
    bottom = Math.max(bottom, ...Object.values(items).map((i) => i.y + i.h / 2));
  } else {
    // Room above the server cards for the gateway's and SSH's lanes (geom).
    const top = 24 + LANE.ssh + 16;
    const servers = [...t.nodes];
    type Slot = { key: string; e: Entity; pod?: Item["pod"]; dashed?: boolean; vol?: boolean };
    const per = new Map<string, Slot[]>(servers.map((s) => [s.name, []]));
    const unplaced: Slot[] = [];
    const put = (node: string | undefined, o: Slot) => {
      const list = node ? per.get(node) : undefined;
      if (list) list.push(o);
      else if (!o.dashed) unplaced.push(o);
    };
    for (const e of m.entities) {
      if (!visible(e, v) || e.kind === "domain") continue;
      if (e.kind === "app") {
        const pods = e.app!.pods.filter((p) => p.node);
        if (!pods.length) put(undefined, { key: e.id, e });
        pods.forEach((pod, i) => put(pod.node, { key: i ? `${e.id}~${i}` : e.id, e, pod: { i, n: pods.length, pod } }));
      } else if (e.kind === "job" || e.kind === "task") {
        put(e.schedule?.node ?? e.task?.node, { key: e.id, e, dashed: true });
      }
    }
    for (const e of m.entities) if (e.kind === "volume" && visible(e, v)) put(e.volume!.node, { key: e.id, e, vol: true });
    const columns = [...servers.map((s) => ({ s, list: per.get(s.name)! })), ...(unplaced.length ? [{ s: undefined, list: unplaced }] : [])];
    for (const c of columns) c.list.sort((a, b) => Number(!!a.vol) - Number(!!b.vol) || Number(!!a.dashed) - Number(!!b.dashed) || a.e.project.localeCompare(b.e.project) || a.e.name.localeCompare(b.e.name));
    const maxN = Math.max(1, ...columns.map((c) => c.list.length + (c.list.some((o) => o.vol) ? 0.3 : 0)));
    const bh = 92 + maxN * SLOT + 8;
    columns.forEach((c, ci) => {
      const cx = X.box[0] + ci * (SERVER_W + SERVER_GAP) + SERVER_W / 2;
      const key = c.s ? `srv:${c.s.name}` : "srv:";
      add({ key, kind: "server", x: cx, y: top + bh / 2, w: SERVER_W, h: bh, sel: c.s ? key : null, label: c.s?.name ?? "Not on a server", server: c.s, status: c.s ? (c.s.ready ? "ok" : "bad") : "mute" });
      let y = top + 100;
      c.list.forEach((o, k) => {
        if (o.vol && k > 0 && !c.list[k - 1].vol) y += 14;
        add({ key: o.key, kind: o.e.kind, x: cx, y: y + (o.vol ? 3 : 0), w: o.vol ? 180 : CHIP.w, h: o.vol ? 46 : CHIP.h, sel: o.e.id, label: o.e.name,
          status: o.pod?.pod ? podTone(o.pod.pod) : o.e.status, entity: o.e, pod: o.pod, dashed: o.dashed, node: c.s?.name });
        y += SLOT;
      });
    });
    bottom = top + bh;
    midY = top + 46;
  }

  // Gateway, internet, firewall.
  add({ key: "gateway", kind: "gateway", x: X.gw, y: midY, w: 104, h: CHIP.h, sel: "gateway", label: "gateway" });
  add({ key: "internet", kind: "net", x: X.net, y: midY, w: 52, h: 52, sel: "internet", label: "internet" });
  const web = firewall.filter((r) => isWeb(r) && openToAll(r));
  if (firewall.length) {
    const top = 16, step = 44;
    const h = Math.max(bottom - 8, midY + step * firewall.length + 40) - top;
    add({ key: "fwband", kind: "fwband", x: X.fw, y: top + h / 2, w: 84, h, sel: "fwband", label: "firewall" });
    // The web ports level with the gateway, the rest below.
    const ordered = [...web, ...firewall.filter((r) => !web.includes(r))];
    let y0 = midY - (step * (web.length - 1)) / 2;
    if (y0 + step * (ordered.length - 1) > top + h - 24) y0 = top + h - 24 - step * (ordered.length - 1);
    ordered.forEach((r, k) => add({ key: `fw:${r.name}`, kind: "fw", x: X.fw, y: y0 + step * k, w: 76, h: 30, sel: `fw:${r.name}`, label: fwLabel(r), fw: r, status: openToAll(r) ? "ok" : "mute" }));
    for (const r of web) {
      const tip = `${fwLabel(r)} open to anyone · ${r.description}`;
      edge({ id: `pub:in:${r.name}`, a: "internet", b: `fw:${r.name}`, kind: "pub", sel: `fw:${r.name}`, traffic: 1, tip });
      edge({ id: `pub:gw:${r.name}`, a: `fw:${r.name}`, b: "gateway", kind: "pub", sel: `fw:${r.name}`, traffic: 1, tip });
    }
  }
  if (!web.length) edge({ id: "pub:direct", a: "internet", b: "gateway", kind: "pub", sel: "gateway", traffic: 1, tip: "Web traffic (80, 443) to the gateway" });

  if (traffic) {
    const outFrom = m.links.filter((l) => l.to === "out" && (v.layers.rules || l.kind !== "rule")).map((l) => at(l.from)).filter((k): k is string => !!k && !!items[k]);
    if (v.layers.rules && outFrom.length) add({ key: "out", kind: "net", x: X.out, y: avg(outFrom.map((k) => items[k].y)), w: 46, h: 46, sel: "out", label: "internet outbound", out: true });
    for (const l of m.links) {
      if ((l.kind === "rule" || l.kind === "drop") && !v.layers.rules) continue;
      let a = at(l.from), b = at(l.to);
      // Without domains on the map, routes go from the gateway to the app.
      if (l.kind === "route" && !v.layers.domains) {
        if (l.from !== "gateway") continue;
        const d = m.byId.get(l.to)?.domain;
        if (!d?.app) continue;
        a = "gateway";
        b = at(appId(d.project, d.app));
      }
      if (!a || !b) continue;
      const { from: _f, to: _t, ...rest } = l;
      edge({ ...rest, a, b });
    }
  } else {
    for (const s of t.nodes) {
      const key = `srv:${s.name}`;
      edge({ id: `gs:${s.name}`, a: "gateway", b: key, kind: "route", sel: "gateway", traffic: 1, label: "80·443", tip: `The gateway runs on ${s.name}; it terminates TLS and routes to app pods` });
      const ssh = firewall.find((r) => r.name === "ssh");
      if (ssh) edge({ id: `ss:${s.name}`, a: `fw:${ssh.name}`, b: key, kind: "ssh", sel: `fw:${ssh.name}`, tip: `SSH (22/tcp) from ${ssh.sources.join(", ") || "anywhere"}` });
    }
    const ssh = firewall.find((r) => r.name === "ssh");
    if (ssh) edge({ id: "pub:ssh", a: "internet", b: `fw:${ssh.name}`, kind: "ssh", sel: `fw:${ssh.name}`, tip: `SSH (22/tcp) from ${ssh.sources.join(", ") || "anywhere"}` });
    // Mounts from the pod on the volume's server.
    for (const l of m.links) {
      if (l.kind !== "mount" || !items[l.to]) continue;
      const vol = items[l.to];
      const pod = Object.values(items).find((i) => i.sel === l.from && i.node === vol.node) ?? items[l.from];
      if (!pod) continue;
      const { from: _f, to: _t, ...rest } = l;
      edge({ ...rest, a: pod.key, b: l.to });
    }
  }
  return { items, edges };
}

export const podTone = (p: TopoPod): Tone => (p.tone === "mute" ? "mute" : p.tone);

// ---- geometry ------------------------------------------------------------------

export type ViewBox = { x: number; y: number; w: number; h: number };

/** The view box that shows every item, at least minW wide. */
export function fit(l: Layout, minW = 1000): ViewBox {
  let x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
  for (const i of Object.values(l.items)) {
    if (i.op <= 0.01) continue;
    x0 = Math.min(x0, i.x - i.w / 2); x1 = Math.max(x1, i.x + i.w / 2);
    y0 = Math.min(y0, i.y - i.h / 2); y1 = Math.max(y1, i.y + i.h / 2 + (i.kind === "net" ? 30 : 0));
  }
  if (!isFinite(x0)) return { x: 0, y: 0, w: minW, h: 600 };
  // Room at the sides too, so a little panning does not clip the internet or the outbound node.
  const pad = 22, padX = 48;
  const vb = { x: x0 - padX, y: y0 - pad, w: x1 - x0 + 2 * padX, h: y1 - y0 + 2 * pad + 44 };
  if (vb.w < minW) { vb.x -= (minW - vb.w) / 2; vb.w = minW; }
  return vb;
}

export type Pt = [number, number];
/** An edge's path, a point at t (0 at a, 1 at b), its two ends, and a fixed spot for its label if it has one. */
export type Geom = { d: string; at: (t: number) => Pt; ends: [Pt, Pt]; label?: Pt };

const r1 = (n: number) => Math.round(n * 10) / 10;
const anchorY = (i: Item) => (i.kind === "server" || i.kind === "pbox" ? i.y - i.h / 2 + 30 : i.y);

/**
 * A curve from a to b: sideways between columns, round the right side within one.
 * In Placement, the gateway's and SSH's lines to a server run in a lane above the
 * cards and drop into the server from the top, so they cross no other server's header.
 */
export function geom(a: Item, b: Item, kind?: Edge["kind"]): Geom {
  if (b.kind === "server" && a.kind !== "server" && (kind === "route" || kind === "ssh")) {
    const top = b.y - b.h / 2, ly = top - LANE[kind], tx = TURN[kind];
    const g = polyline([[r1(a.x + a.w / 2), r1(a.y)], [tx, r1(a.y)], [tx, r1(ly)], [r1(b.x), r1(ly)], [r1(b.x), r1(top)]]);
    return { ...g, label: [b.x, ly] };
  }
  const dx = b.x - a.x;
  let p1: Pt, p2: Pt, c1: Pt, c2: Pt;
  if (Math.abs(dx) < 30) {
    p1 = [r1(a.x + a.w / 2), r1(anchorY(a))]; p2 = [r1(b.x + b.w / 2), r1(anchorY(b))];
    const k = 30 + Math.min(30, Math.abs(p2[1] - p1[1]) / 8);
    c1 = [p1[0] + k, p1[1]]; c2 = [p2[0] + k, p2[1]];
  } else {
    const dir = dx > 0 ? 1 : -1;
    p1 = [r1(a.x + (dir * a.w) / 2), r1(anchorY(a))]; p2 = [r1(b.x - (dir * b.w) / 2), r1(anchorY(b))];
    const k = Math.max(28, Math.abs(p2[0] - p1[0]) * 0.45);
    c1 = [r1(p1[0] + dir * k), p1[1]]; c2 = [r1(p2[0] - dir * k), p2[1]];
  }
  const at = (t: number): Pt => {
    const u = 1 - t;
    return [0, 1].map((k) => u * u * u * p1[k] + 3 * u * u * t * c1[k] + 3 * u * t * t * c2[k] + t * t * t * p2[k]) as Pt;
  };
  return { d: `M${p1}C${c1} ${c2} ${p2}`, at, ends: [p1, p2] };
}

/** Straight segments with rounded corners; at(t) goes by length along them. */
function polyline(pts: Pt[], r = 8): Geom {
  const len = (p: Pt, q: Pt) => Math.hypot(q[0] - p[0], q[1] - p[1]);
  const toward = (p: Pt, q: Pt, d: number): Pt => { const l = len(p, q) || 1; return [r1(p[0] + ((q[0] - p[0]) * d) / l), r1(p[1] + ((q[1] - p[1]) * d) / l)]; };
  let d = `M${pts[0]}`;
  for (let i = 1; i < pts.length - 1; i++) {
    const c = pts[i], k = Math.min(r, len(pts[i - 1], c) / 2, len(c, pts[i + 1]) / 2);
    d += `L${toward(c, pts[i - 1], k)}Q${c} ${toward(c, pts[i + 1], k)}`;
  }
  d += `L${pts[pts.length - 1]}`;
  const segs = pts.slice(1).map((q, i) => len(pts[i], q));
  const total = segs.reduce((s, x) => s + x, 0);
  const at = (t: number): Pt => {
    let rest = Math.max(0, Math.min(1, t)) * total;
    for (let i = 0; i < segs.length; i++) {
      if (rest <= segs[i] || i === segs.length - 1) return toward(pts[i], pts[i + 1], Math.min(rest, segs[i]));
      rest -= segs[i];
    }
    return pts[pts.length - 1];
  };
  return { d, at, ends: [pts[0], pts[pts.length - 1]] };
}

/**
 * Where an edge's label (w wide, 18 high) goes: its fixed spot if it has one; else
 * the first point along the edge, from `prefer` outwards, where it covers none of
 * `chips`; else in the gap between two rows beside the edge's start (apps in
 * neighbouring columns leave no room between them); else at `prefer`.
 */
export function labelAt(g: Geom, w: number, prefer: number, chips: Item[]): Pt {
  if (g.label) return g.label;
  const covers = ([x, y]: Pt) => chips.some((i) => Math.abs(i.x - x) < (i.w + w) / 2 + 1 && Math.abs(i.y - y) < (i.h + 18) / 2 + 1);
  for (const d of [0, 0.08, -0.08, 0.16, -0.16, 0.24, -0.24, 0.32, -0.32]) {
    const t = prefer + d;
    if (t < 0.1 || t > 0.9) continue;
    const p = g.at(t);
    if (!covers(p)) return p;
  }
  const [p1, p2] = g.ends;
  for (const dy of p2[1] < p1[1] ? [-ROW / 2, ROW / 2] : [ROW / 2, -ROW / 2]) {
    const p: Pt = [(p1[0] + p2[0]) / 2, p1[1] + dy];
    if (!covers(p)) return p;
  }
  return g.at(prefer);
}

/** Linear interpolation of two layouts, for the lens change. */
export function between(from: Layout, to: Layout, t: number): Layout {
  const lerp = (a: number, b: number) => a + (b - a) * t;
  const parent = (l: Layout, key: string) => l.items[key.split("~")[0]];
  const items: Record<string, Item> = {};
  for (const key of new Set([...Object.keys(from.items), ...Object.keys(to.items)])) {
    let a = from.items[key], b = to.items[key];
    if (!a) { const p = parent(from, key); a = { ...b, ...(p ? { x: p.x, y: p.y } : {}), op: 0 }; }
    if (!b) { const p = parent(to, key); b = { ...a, ...(p ? { x: p.x, y: p.y } : {}), op: 0 }; }
    items[key] = { ...(t < 0.5 ? a : b), x: lerp(a.x, b.x), y: lerp(a.y, b.y), w: lerp(a.w, b.w), h: lerp(a.h, b.h), op: lerp(a.op, b.op) };
  }
  const ea = new Map(from.edges.map((e) => [e.id, e])), eb = new Map(to.edges.map((e) => [e.id, e]));
  const edges = [...new Set([...ea.keys(), ...eb.keys()])].map((id) => {
    const a = ea.get(id), b = eb.get(id);
    return { ...(b ?? a)!, op: a && b ? 1 : a ? Math.max(0, 1 - 2 * t) : Math.max(0, 2 * t - 1) };
  });
  return { items, edges };
}

// ---- focus ---------------------------------------------------------------------

export type Focus = { keys: Set<string>; edges: Set<string> };

/** What lights up when sel is hovered or selected: it, its links and what they reach. */
export function focusOf(l: Layout, sel: string): Focus {
  const keys = new Set<string>(), edges = new Set<string>();
  const node = sel.startsWith("srv:") ? sel.slice(4) : undefined;
  for (const i of Object.values(l.items)) if (i.sel === sel || (node && i.node === node)) keys.add(i.key);
  for (const e of l.edges) if (e.sel === sel || keys.has(e.a) || keys.has(e.b)) edges.add(e.id);
  for (const e of l.edges) if (edges.has(e.id)) { keys.add(e.a); keys.add(e.b); }
  return { keys, edges };
}

/** Everything that needs attention, and the dropped connections. */
export function problemsOf(l: Layout): Focus {
  const keys = new Set<string>(), edges = new Set<string>();
  for (const i of Object.values(l.items)) if (i.status === "bad" || i.status === "warn") keys.add(i.key);
  for (const e of l.edges) if (e.tone) { edges.add(e.id); keys.add(e.a); keys.add(e.b); }
  return { keys, edges };
}

export function searchOf(l: Layout, q: string): Focus {
  const keys = new Set<string>();
  const needle = q.trim().toLowerCase();
  for (const i of Object.values(l.items)) if (i.label.toLowerCase().includes(needle)) keys.add(i.key);
  return { keys, edges: new Set() };
}

/** The map object a Needs-attention item is about, by its key (alerts.ts). */
export function attentionTarget(key: string, project?: string, app?: string): string | undefined {
  const [kind, rest] = [key.slice(0, key.indexOf(":")), key.slice(key.indexOf(":") + 1)];
  switch (kind) {
    case "app":
    case "build":
      return `app:${rest}`;
    case "schedule":
      return `job:${rest}`;
    case "domain":
      return `dom:${rest}`;
    case "alert":
      return project && app ? appId(project, app) : undefined;
    default:
      return undefined;
  }
}
