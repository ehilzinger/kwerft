// Clusters › Nodes (docs/phase5.md, W2): node pools of Hetzner Cloud
// servers, the cluster's nodes, join commands for dedicated servers. The
// server checks everything (control-plane counts included); the helpers
// here only phrase states and pre-check forms.
import { request } from "./api";

export type PoolRole = "worker" | "control-plane" | "builds";

export type PoolServer = {
  name: string;
  serverId?: number;
  publicIp?: string;
  privateIp?: string;
  serverType?: string;
  phase: "Creating" | "Joining" | "Ready" | "Draining" | "Deleting" | "Failed" | string;
  message?: string;
};

export type Pool = {
  /** The NodePool object. */
  name: string;
  /** Its name within the cluster. */
  pool: string;
  role: PoolRole;
  serverType: string;
  location: string;
  count: number;
  desired: number;
  ready: number;
  labels: Record<string, string>;
  scaleDownAfterMinutes?: number;
  deleting: boolean;
  state: "ready" | "progressing" | "failed";
  reason?: string;
  message?: string;
  servers: PoolServer[];
};

/** Health of a disk, a RAID array or a node's disks, judged by the server like the disk alerts. */
export type DiskHealthLevel = "ok" | "warn" | "bad" | "unknown";

/** A software RAID (md) array, from node-exporter. */
export type RaidArray = {
  device: string;
  state: "active" | "inactive" | "recovering" | "resync" | "check" | string;
  active: number;
  required: number;
  failed: number;
  spare: number;
  syncedPercent?: number;
  health: DiskHealthLevel;
  problem?: string;
};

/** A disk's SMART readings (dedicated servers). */
export type Disk = {
  device: string;
  model?: string;
  serial?: string;
  interface?: string;
  smartPassed?: boolean;
  criticalWarning?: number;
  /** Percent of the rated endurance used (NVMe); may pass 100. */
  percentageUsed?: number;
  availableSpare?: number;
  availableSpareThreshold?: number;
  mediaErrors?: number;
  unreadable?: boolean;
  health: DiskHealthLevel;
  problems: string[];
};

export type DiskHealth = {
  health: DiskHealthLevel;
  /** "2 disks failing · RAID md1 degraded". */
  summary: string;
  /** The node's smartctl exporter answers and found disks. */
  smart: boolean;
  arrays: RaidArray[];
  disks: Disk[];
};

export type ClusterNode = {
  name: string;
  roles: string[];
  pool?: string;
  ready: boolean;
  status: "Ready" | "NotReady" | "Draining" | "Drained" | "Removing" | "Cordoned" | string;
  message?: string;
  internalIp?: string;
  externalIp?: string;
  kubeletVersion?: string;
  os?: string;
  cpu?: string;
  memory?: string;
  platform?: string;
  unschedulable: boolean;
  created?: string;
  /** RAID and SMART readings: nodes with any, and every dedicated server. */
  diskHealth?: DiskHealth;
};

export type ClusterNodes = {
  cluster: string;
  provider: "local" | "hetzner-cloud" | "adopted" | string;
  reachable: boolean;
  problem?: string;
  pools: Pool[];
  nodes: ClusterNode[];
  joinable: boolean;
  cloud: boolean;
  controlPlanes: number;
  /** The cluster's metrics answered: nodes without diskHealth have no RAID or SMART readings. */
  diskReadings: boolean;
};

export type ServerType = {
  name: string;
  description: string;
  cores: number;
  memory: number;
  disk: number;
  cpuType: string;
  architecture: "x86" | "arm" | string;
  locations: string[];
  prices: Record<string, string>;
};

export type Location = { name: string; city: string; country: string; networkZone: string };

export type Catalog = { locations: Location[]; serverTypes: ServerType[] };

export type PoolInput = {
  name?: string;
  role?: PoolRole;
  serverType?: string;
  location?: string;
  count?: number;
  labels?: Record<string, string>;
  scaleDownAfterMinutes?: number;
};

export type JoinCommand = { command: string; role: "worker" | "control-plane"; expiresAt: string };

export const nodesKey = (cluster: string) => ["clusters", cluster, "nodes"] as const;
export const catalogKey = ["hetzner", "catalog"] as const;

const c = (cluster: string) => `/clusters/${encodeURIComponent(cluster)}`;

export const nodesApi = {
  get: (cluster: string) => request<ClusterNodes>(`${c(cluster)}/nodes`),
  catalog: () => request<Catalog>("/hetzner/catalog"),
  createPool: (cluster: string, p: PoolInput) => request<Pool>(`${c(cluster)}/pools`, { method: "POST", json: p }),
  updatePool: (cluster: string, pool: string, p: PoolInput) =>
    request<Pool>(`${c(cluster)}/pools/${encodeURIComponent(pool)}`, { method: "PATCH", json: p }),
  deletePool: (cluster: string, pool: string) => request<void>(`${c(cluster)}/pools/${encodeURIComponent(pool)}`, { method: "DELETE" }),
  removeServer: (cluster: string, pool: string, server: string) =>
    request<void>(`${c(cluster)}/pools/${encodeURIComponent(pool)}/servers/${encodeURIComponent(server)}/remove`, { method: "POST" }),
  drain: (cluster: string, node: string) => request<ClusterNode>(`${c(cluster)}/nodes/${encodeURIComponent(node)}/drain`, { method: "POST" }),
  uncordon: (cluster: string, node: string) => request<ClusterNode>(`${c(cluster)}/nodes/${encodeURIComponent(node)}/uncordon`, { method: "POST" }),
  remove: (cluster: string, node: string, force: boolean) =>
    request<ClusterNode>(`${c(cluster)}/nodes/${encodeURIComponent(node)}/remove`, { method: "POST", json: { force } }),
  joinCommand: (cluster: string, role: "worker" | "control-plane", ttlMinutes: number) =>
    request<JoinCommand>(`${c(cluster)}/join-command`, { method: "POST", json: { role, ttlMinutes } }),
};

export const roleText: Record<string, string> = { worker: "Workers", "control-plane": "Control plane", builds: "Builds" };

/** Tone of a pool server's phase. */
export function phaseTone(phase: string): "ok" | "warn" | "bad" | "mute" {
  switch (phase) {
    case "Ready":
      return "ok";
    case "Failed":
      return "bad";
    case "Creating":
    case "Joining":
    case "Draining":
      return "warn";
  }
  return "mute";
}

export function nodeTone(n: ClusterNode): "ok" | "warn" | "bad" | "mute" {
  if (!n.ready) return "bad";
  if (n.status === "Ready") return "ok";
  return "warn";
}

/** How a pool stands, in a few words. */
export function poolSummary(p: Pool): string {
  if (p.deleting) return "Removing its servers";
  if (p.role === "builds") {
    if (p.desired === 0 && p.servers.length === 0) return `Idle · up to ${p.count}`;
    return `${p.ready} of ${p.desired} ready · up to ${p.count}`;
  }
  return `${p.ready} of ${p.count} ready`;
}

/** The control-plane counts a pool may have, given the others. */
export function controlPlaneCountProblem(outside: number, count: number): string | undefined {
  const total = outside + count;
  if (total === 0) return "The cluster needs at least one control-plane node.";
  if (total % 2 === 0) return `That makes ${total} control-plane nodes; etcd needs an odd number (1, 3 or 5).`;
  return undefined;
}

/** Parses "key=value" lines into labels; returns an error for a bad line. */
export function parseLabels(text: string): { labels: Record<string, string>; error?: string } {
  const labels: Record<string, string> = {};
  for (const raw of text.split(/[\n,]/)) {
    const line = raw.trim();
    if (!line) continue;
    const i = line.indexOf("=");
    if (i <= 0) return { labels, error: `"${line}" is not key=value.` };
    labels[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  return { labels };
}

export function labelsText(labels: Record<string, string>): string {
  return Object.entries(labels).map(([k, v]) => `${k}=${v}`).join("\n");
}

/** "€4.70/month" style price for a server type in a location. */
export function priceText(t: ServerType | undefined, location: string): string {
  const p = t?.prices[location];
  if (!p) return "";
  const n = Number(p);
  return Number.isFinite(n) ? `€${n.toFixed(2)}/month` : "";
}

export function serverTypeText(t: ServerType): string {
  const arch = t.architecture === "arm" ? " · Arm" : "";
  const cpu = t.cpuType === "dedicated" ? "dedicated vCPU" : "vCPU";
  return `${t.name} — ${t.cores} ${cpu}, ${t.memory} GB, ${t.disk} GB${arch}`;
}

// ---- disk health -----------------------------------------------------------------

export function diskTone(h: DiskHealthLevel): "ok" | "warn" | "bad" | "mute" {
  switch (h) {
    case "ok": return "ok";
    case "bad": return "bad";
    case "warn":
    case "unknown": return "warn";
  }
  return "mute";
}

/** The pill text for a node's disks, an array or a disk. */
export function diskHealthText(h: DiskHealthLevel): string {
  switch (h) {
    case "ok": return "Healthy";
    case "warn": return "Watch";
    case "bad": return "Failing";
    case "unknown": return "No readings";
  }
  return h;
}

/** "RAID md1 · 2 of 2 disks active", plus spares and the rebuild. */
export function raidText(a: RaidArray): string {
  let t = `${a.active} of ${a.required} ${a.required === 1 ? "disk" : "disks"} active`;
  if (a.failed > 0) t += ` · ${a.failed} failed`;
  if (a.spare > 0) t += ` · ${a.spare} ${a.spare === 1 ? "spare" : "spares"}`;
  if (a.state !== "active") t += ` · ${a.state}`;
  return t;
}

/** "115 %", "—" when the drive does not report it. */
export function percentText(v?: number): string {
  return v === undefined ? "—" : `${Math.round(v)} %`;
}

/** The SMART verdict in a word. */
export function smartText(d: Disk): string {
  if (d.unreadable) return "Unreadable";
  if (d.smartPassed === undefined) return "—";
  return d.smartPassed ? "Passed" : "FAILED";
}

/** Nodes whose disks the console shows in detail. */
export function nodesWithDisks(nodes: ClusterNode[]): ClusterNode[] {
  return nodes.filter((n) => n.diskHealth && (n.diskHealth.arrays.length > 0 || n.diskHealth.disks.length > 0 || n.platform === "dedicated"));
}
