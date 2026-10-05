// Clusters (docs/phase5.md, W3): the clusters this console manages. A remote
// cluster runs Kwerft in agent mode and connects to the console; the console
// reaches its API through that connection. Owners and admins only.
import { request } from "./api";
import type { ClusterDNS, HCloudStatus } from "./settings";

export type ClusterProvider = "local" | "hetzner-cloud" | "adopted";
export type ClusterPhase = "Pending" | "Provisioning" | "Connected" | "Disconnected" | "Failed" | "";

export type Cluster = {
  name: string;
  displayName?: string;
  provider: ClusterProvider;
  hetznerCloud?: { location: string; serverType: string; controlPlanes: number };
  /** A Hetzner Cloud cluster's Cloud Firewall and Load Balancer. */
  cloud?: ClusterCloud;
  phase: ClusterPhase;
  message?: string;
  /** The agent is connected now (always for the local cluster). */
  connected: boolean;
  deleting?: boolean;
  /** An agent token exists (its hash; the token itself is shown once). */
  hasToken: boolean;
  nodes: number;
  readyNodes: number;
  kubernetesVersion?: string;
  agentVersion?: string;
  lastSeen?: string;
  createdAt: string;
  agent?: { remote: string; since: string; error?: string };
  pools?: { name: string; role: string; serverType: string; location: string; count: number; readyNodes: number }[];
  /** A remote cluster: where its ingress is reached. */
  publicAddresses?: string[];
  /** A remote cluster: the records the console keeps for its hostnames under the apps domain. */
  dns?: ClusterDNS;
  conditions?: { type: string; status: string; reason: string; message: string }[];
};

/** Settings of a Hetzner Cloud cluster's own Cloud Firewall and Load
 * Balancer, and what Kwerft in that cluster last reported. */
export type ClusterCloud = {
  firewall: "sync" | "off";
  loadBalancer: { enabled: boolean; type?: string; location?: string };
  status?: HCloudStatus;
};

export type ClusterInput =
  | { name: string; displayName?: string; provider: "adopted" }
  | { name: string; displayName?: string; provider: "hetzner-cloud"; hetznerCloud: { location: string; serverType: string; controlPlanes: 1 | 3 } };

/** The token and the command that installs the agent with it: shown once. */
export type AgentInstall = { token: string; installCommand: string };

export const clustersKey = ["clusters"] as const;

export const clustersApi = {
  list: () => request<{ clusters: Cluster[] }>("/clusters").then((r) => r.clusters),
  get: (name: string) => request<Cluster>(`/clusters/${encodeURIComponent(name)}`),
  create: (c: ClusterInput) => request<{ cluster: Cluster } & Partial<AgentInstall>>("/clusters", { method: "POST", json: c }),
  remove: (name: string) => request<void>(`/clusters/${encodeURIComponent(name)}`, { method: "DELETE" }),
  rotateToken: (name: string) => request<AgentInstall>(`/clusters/${encodeURIComponent(name)}/token`, { method: "POST" }),
  saveCloud: (name: string, s: Pick<ClusterCloud, "firewall" | "loadBalancer">) =>
    request<{ cloud: ClusterCloud }>(`/settings/hcloud?cluster=${encodeURIComponent(name)}`, { method: "PUT", json: s }).then((r) => r.cloud),
};

/** Hetzner Cloud locations a cluster's control plane can be created in. */
export const cloudLocations: { id: string; label: string }[] = [
  { id: "fsn1", label: "Falkenstein (fsn1)" },
  { id: "nbg1", label: "Nuremberg (nbg1)" },
  { id: "hel1", label: "Helsinki (hel1)" },
  { id: "ash", label: "Ashburn, VA (ash)" },
  { id: "hil", label: "Hillsboro, OR (hil)" },
  { id: "sin", label: "Singapore (sin)" },
];

export const providerLabel: Record<ClusterProvider, string> = {
  local: "This server",
  "hetzner-cloud": "Hetzner Cloud",
  adopted: "Adopted",
};

/** A problem with a cluster name, or undefined; the server checks again. */
export function clusterNameProblem(name: string): string | undefined {
  if (!name) return "Enter a name.";
  if (name.length > 40) return "At most 40 characters.";
  if (!/^[a-z]([-a-z0-9]*[a-z0-9])?$/.test(name)) return "Lowercase letters, digits and dashes, starting with a letter and not ending with a dash.";
  if (name === "local" || name === "connect") return "This name is reserved.";
  return undefined;
}

export type PhaseTone = "ok" | "warn" | "bad" | "info";

/** How a cluster's state reads in the list: a label and a tone. */
export function clusterState(c: Pick<Cluster, "phase" | "connected" | "deleting" | "provider">): { label: string; tone: PhaseTone } {
  if (c.deleting) return { label: "Deleting", tone: "warn" };
  if (c.connected) return { label: c.provider === "local" ? "Running" : "Connected", tone: "ok" };
  switch (c.phase) {
    case "Disconnected": return { label: "Disconnected", tone: "bad" };
    case "Failed": return { label: "Failed", tone: "bad" };
    case "Provisioning": return { label: "Creating", tone: "info" };
    default: return { label: "Waiting for agent", tone: "info" };
  }
}

/** "3 / 3" nodes, or a dash before the first report. */
export function nodesText(c: Pick<Cluster, "nodes" | "readyNodes" | "lastSeen" | "connected">): string {
  if (!c.lastSeen && !c.connected) return "—";
  return c.readyNodes === c.nodes ? String(c.nodes) : `${c.readyNodes} / ${c.nodes} ready`;
}
