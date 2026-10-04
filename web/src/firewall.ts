// Server firewall (docs/phase4.md, W3): FirewallRules applied on every node
// by the node agent, and the pending change that rolls back unless it is
// confirmed. The server decides and checks everything (lock-out protection
// included); the helpers here only phrase rules and pre-check forms.
import { request } from "./api";

export type FirewallProtocol = "TCP" | "UDP" | "ICMP";

export type FirewallRule = {
  name: string;
  port: number;
  endPort?: number;
  protocol: FirewallProtocol;
  sources: string[];
  nodes: "all" | "control-plane";
  description: string;
  disabled: boolean;
  /** One of the installer's base rules. */
  required: boolean;
  /** What the console may change: custom rules all, the SSH rule its sources. */
  editable: "all" | "sources" | "none";
  ready: boolean;
  reason?: string;
  message?: string;
};

export type FirewallRuleInput = {
  name: string;
  port: number;
  endPort?: number;
  protocol: "TCP" | "UDP";
  sources: string[];
  nodes: "all" | "control-plane";
  description: string;
  disabled: boolean;
};

export type FirewallNode = { name: string; controlPlane: boolean; state: string; message?: string; updatedAt?: string };

export type FirewallState = "in-sync" | "pending" | "rolled-back" | "failed" | "applying" | "no-agents" | "unavailable";

export type Firewall = {
  rules: FirewallRule[];
  revision: string;
  confirmed: string;
  state: FirewallState;
  pending?: { revision: string; deadline: string; remainingSeconds: number; nodes: number };
  rolledBack?: { revision: string; at?: string };
  canRollBack: boolean;
  nodes: FirewallNode[];
  client: { ip: string; verifiable: boolean; ssh: boolean };
  problems?: string[];
};

export const firewallKey = ["firewall"] as const;

export const firewallApi = {
  get: () => request<Firewall>("/firewall"),
  create: (r: FirewallRuleInput) => request<FirewallRule>("/firewall/rules", { method: "POST", json: r }),
  update: (name: string, r: Partial<FirewallRuleInput>) =>
    request<FirewallRule>(`/firewall/rules/${encodeURIComponent(name)}`, { method: "PUT", json: r }),
  remove: (name: string) => request<void>(`/firewall/rules/${encodeURIComponent(name)}`, { method: "DELETE" }),
  confirm: (revision: string) => request<{ confirmed: string }>("/firewall/confirm", { method: "POST", json: { revision } }),
  rollback: () => request<{ revision: string; changed: string[] }>("/firewall/rollback", { method: "POST" }),
  retry: (revision: string) => request<{ attempt: number }>("/firewall/retry", { method: "POST", json: { revision } }),
};

/** "22", "8000–8100", "—" for ICMP, "all" for the whole range. */
export function describePorts(r: Pick<FirewallRule, "port" | "endPort" | "protocol">): string {
  if (r.protocol === "ICMP") return "—";
  if (r.endPort && r.endPort > r.port) return r.port === 1 && r.endPort === 65535 ? "all" : `${r.port}–${r.endPort}`;
  return String(r.port);
}

export function describeSources(sources: string[]): string {
  return sources.length ? sources.join(", ") : "any";
}

/** Splits what someone typed (commas, spaces, new lines) into sources. */
export function parseSourcesText(text: string): string[] {
  return text.split(/[\s,;]+/).map((s) => s.trim()).filter(Boolean);
}

const RULE_NAME = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;
const REQUIRED_NAMES = ["ssh", "http", "https", "wireguard", "icmp", "cluster-private"];

/** A quick check before the server's; undefined when the form looks fine. */
export function ruleProblem(f: { name: string; port: string; endPort: string }, creating: boolean): { field: string; message: string } | undefined {
  if (creating) {
    if (!RULE_NAME.test(f.name) || f.name.length > 63) return { field: "name", message: "Use lowercase letters, digits and dashes, like game-server." };
    if (REQUIRED_NAMES.includes(f.name)) return { field: "name", message: "That name belongs to a required rule." };
  }
  const port = Number(f.port);
  if (!/^\d+$/.test(f.port) || port < 1 || port > 65535) return { field: "port", message: "Enter a port from 1 to 65535." };
  if (f.endPort.trim()) {
    const end = Number(f.endPort);
    if (!/^\d+$/.test(f.endPort) || end < port || end > 65535) return { field: "endPort", message: `The range ends at or after ${port}, at most 65535.` };
  }
  return undefined;
}

/** Whether a source (IPv4/IPv6 address or CIDR) contains ip; a rough client-side check. */
export function sourceContains(source: string, ip: string): boolean {
  const [net, bitsText] = source.split("/");
  const a = toBits(net), b = toBits(ip);
  if (!a || !b || a.length !== b.length) return false;
  const bits = bitsText === undefined ? a.length : Number(bitsText);
  if (!Number.isInteger(bits) || bits < 0 || bits > a.length) return false;
  return a.slice(0, bits) === b.slice(0, bits);
}

function toBits(ip: string): string | undefined {
  if (/^\d+\.\d+\.\d+\.\d+$/.test(ip)) {
    const parts = ip.split(".").map(Number);
    if (parts.some((p) => p > 255)) return undefined;
    return parts.map((p) => p.toString(2).padStart(8, "0")).join("");
  }
  if (!ip.includes(":")) return undefined;
  const [head, tail, ...rest] = ip.split("::");
  if (rest.length) return undefined;
  const h = head ? head.split(":") : [];
  const t = tail !== undefined ? (tail ? tail.split(":") : []) : [];
  const groups = tail !== undefined ? [...h, ...Array(8 - h.length - t.length).fill("0"), ...t] : h;
  if (groups.length !== 8 || groups.some((g) => !/^[0-9a-fA-F]{1,4}$/.test(g))) return undefined;
  return groups.map((g) => parseInt(g, 16).toString(2).padStart(16, "0")).join("");
}

/** Whether ip reaches SSH with these sources (empty: anyone). */
export function sshCovers(sources: string[], ip: string): boolean {
  return sources.length === 0 || sources.some((s) => sourceContains(s, ip));
}

/** "0:42" */
export function countdown(seconds: number): string {
  const s = Math.max(0, Math.round(seconds));
  return `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
}

export const nodeStateText: Record<string, string> = {
  "in-sync": "Applied",
  pending: "Waiting for confirmation",
  "rolled-back": "Rolled back",
  error: "Failed",
  paused: "Paused on the node",
  unprepared: "Installer too old",
  waiting: "Applying",
  "no-agent": "No agent",
  stale: "Agent not reporting",
};
