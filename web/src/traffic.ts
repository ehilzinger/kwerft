// Client for the traffic API: a project's TrafficRules with Hubble's counts,
// the connections policies dropped, and the project's isolation. The server
// acts as the signed-in user, so a 403 is Kubernetes RBAC talking.
import { request } from "./api";

/** One side of a rule: exactly one field is set. */
export type TrafficPeer = { app?: string; project?: string; internet?: boolean; cidr?: string };
export type TrafficPort = { port: number; endPort?: number; protocol?: "TCP" | "UDP" };
export type TrafficCounts = { allowed: number; dropped: number; since?: string };

export type TrafficRule = {
  name: string;
  description?: string;
  disabled: boolean;
  from: TrafficPeer[];
  to: TrafficPeer[];
  ports: TrafficPort[];
  phase: "ready" | "pending" | "waiting" | "failed" | "disabled";
  reason?: string;
  message?: string;
  policies: string[];
  counts?: TrafficCounts;
  generation: number;
  created: string;
};

/** Where a dropped connection came from or went: an app (or other pod) of a namespace, or outside the pods. */
export type Side = { namespace?: string; app?: string; kind: string; ip?: string; name?: string };

export type RuleInput = { name: string; description?: string; disabled?: boolean; from: TrafficPeer[]; to: TrafficPeer[]; ports: TrafficPort[] };

export type Suggestion = { project: string; name: string; spec: Omit<RuleInput, "name"> };

export type Drop = {
  from: Side;
  to: Side;
  port?: number;
  protocol?: string;
  egress: boolean;
  count: number;
  first: string;
  last: string;
  suggestion?: Suggestion;
};

export type HubbleStatus = { state: "off" | "connecting" | "ok" | "error"; message?: string; since?: string; lost?: number; window: string };

export type Traffic = { project: string; isolated: boolean; hubble: HubbleStatus; rules: TrafficRule[]; drops: Drop[] };

const base = (project: string) => `/projects/${encodeURIComponent(project)}`;

export const traffic = {
  get: (project: string) => request<Traffic>(`${base(project)}/traffic`),
  drops: () => request<{ hubble: HubbleStatus; drops: Drop[] }>("/traffic/drops"),
  create: (project: string, rule: RuleInput) => request<TrafficRule>(`${base(project)}/trafficrules`, { method: "POST", json: rule }),
  update: (project: string, rule: RuleInput, generation: number) =>
    request<TrafficRule>(`${base(project)}/trafficrules/${encodeURIComponent(rule.name)}`, { method: "PUT", json: { ...rule, generation } }),
  remove: (project: string, name: string) => request<void>(`${base(project)}/trafficrules/${encodeURIComponent(name)}`, { method: "DELETE" }),
  setIsolated: (project: string, isolated: boolean) =>
    request<{ project: string; isolated: boolean }>(`${base(project)}/isolation`, { method: "PUT", json: { isolated } }),
};

/** A peer as the table shows it, relative to the rule's project. */
export function peerLabel(p: TrafficPeer, project: string): string {
  if (p.app) return p.app.includes("/") ? p.app : `${project}/${p.app}`;
  if (p.project) return `${p.project}/*`;
  if (p.internet) return "internet";
  if (p.cidr) return p.cidr;
  return "?";
}

export function portsLabel(ports: TrafficPort[]): string {
  if (ports.length === 0) return "any port";
  const byProto = new Map<string, string[]>();
  for (const p of ports) {
    const proto = p.protocol ?? "TCP";
    const s = p.endPort && p.endPort > p.port ? `${p.port}–${p.endPort}` : `${p.port}`;
    byProto.set(proto, [...(byProto.get(proto) ?? []), s]);
  }
  return [...byProto].map(([proto, list]) => `${proto} ${list.join(", ")}`).join(" · ");
}

/** One end of a drop: "shop/api", "internet 93.184.216.34 (example.com)", "a node". */
export function sideLabel(s: Side): string {
  switch (s.kind) {
    case "pod":
      return s.app ? `${s.namespace}/${s.app}` : `${s.namespace ?? "?"} (not an app)`;
    case "world":
      return `internet${s.ip ? ` ${s.ip}` : ""}${s.name ? ` (${s.name})` : ""}`;
    case "host":
    case "remote-node":
      return "a node (ingress)";
    case "kube-apiserver":
      return "the Kubernetes API";
    default:
      return s.kind;
  }
}

/** Large counts the way the blueprint shows them: 96.4k. */
export function count(n?: number): string {
  if (n === undefined) return "—";
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1000).toFixed(n < 10_000 ? 1 : 0).replace(/\.0$/, "")}k`;
  return `${(n / 1_000_000).toFixed(1).replace(/\.0$/, "")}M`;
}

// ---- the editor's rows ------------------------------------------------------

export type PeerKind = "app" | "project" | "internet" | "cidr";
export type PeerRow = { kind: PeerKind; value: string };

export function toRow(p: TrafficPeer): PeerRow {
  if (p.internet) return { kind: "internet", value: "" };
  if (p.cidr !== undefined) return { kind: "cidr", value: p.cidr };
  if (p.project !== undefined) return { kind: "project", value: p.project };
  return { kind: "app", value: p.app ?? "" };
}

export function fromRow(r: PeerRow): TrafficPeer {
  const v = r.value.trim();
  switch (r.kind) {
    case "internet": return { internet: true };
    case "cidr": return { cidr: v };
    case "project": return { project: v };
    default: return { app: v };
  }
}

/** "8080, 5432, 9000-9010" → ports; a word in the list is an error. */
export function parsePorts(text: string, protocol: "TCP" | "UDP"): TrafficPort[] | string {
  const out: TrafficPort[] = [];
  for (const part of text.split(/[\s,]+/).filter(Boolean)) {
    const m = /^(\d{1,5})(?:[-–](\d{1,5}))?$/.exec(part);
    if (!m) return `“${part}” is not a port or range like 9000-9010.`;
    const port = Number(m[1]);
    const end = m[2] ? Number(m[2]) : undefined;
    if (port < 1 || port > 65535 || (end !== undefined && (end < port || end > 65535))) return `${part} is outside 1–65535.`;
    out.push(end && end > port ? { port, endPort: end, protocol } : { port, protocol });
  }
  return out;
}

export function portsText(ports: TrafficPort[]): string {
  return ports.map((p) => (p.endPort && p.endPort > p.port ? `${p.port}-${p.endPort}` : String(p.port))).join(", ");
}

/** Whether a rule's sources must be apps of its project: it sends out of it. */
export function sendsOut(to: PeerRow[], project: string): boolean {
  return to.some((r) => r.kind === "internet" || r.kind === "cidr" ||
    (r.kind === "project" && r.value.trim() !== project) ||
    (r.kind === "app" && r.value.includes("/") && r.value.split("/")[0] !== project));
}
