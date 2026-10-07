// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Settings: the console's hostname, the apps base domain and how app
// certificates are issued (ConsoleSettings in the cluster). Everyone reads
// them; owners and admins change them. The DNS token is write-only.

import { request } from "./api";
import { clusterQuery } from "./clusters";

export type CertificateState = {
  name: string;
  purpose: "console" | "console-next" | "console-previous" | "apps-wildcard";
  hostnames: string[];
  state: "valid" | "issuing" | "failed";
  message?: string;
  notAfter?: string;
};

export type DNSRecordState = "Managed" | "External" | "Conflict" | "TakenOver" | "NoZone" | "Error" | "Pending" | "Unsupported";

/** A hostname whose A/AAAA records Kwerft keeps (spec.dns.manageRecords).
 * Purpose "app": a remote cluster's hostname under the console's apps domain. */
export type DNSRecord = {
  hostname: string;
  purpose: "console" | "console-next" | "console-previous" | "apps" | "app";
  /** Purpose app: the Domain's project. */
  project?: string;
  zone?: string;
  state: DNSRecordState;
  values: string[];
  message?: string;
};

export type Settings = {
  /** Where the console is served now. */
  consoleDomain: string;
  /** Requested, waiting for its certificate before the console moves there. */
  pendingConsoleDomain?: string;
  /** Redirects to consoleDomain for a day after a move. */
  previousConsoleDomain?: string;
  appsDomain?: string;
  tls: "http01" | "dns01";
  dnsProvider?: "hetzner";
  tokenSet: boolean;
  /** Kwerft keeps the records of the console hostname and *.<appsDomain>. */
  manageRecords: boolean;
  dnsRecords: DNSRecord[];
  /** Why the last sync with the DNS provider could not run. */
  dnsMessage?: string;
  dnsSyncedAt?: string;
  /** "*.<appsDomain>" once the wildcard listener serves apps. */
  wildcardDomain?: string;
  publicAddresses: string[];
  certificates: CertificateState[];
  ready?: { status: boolean; reason: string; message: string };
  hcloud: HCloud;
  /** The cluster whose settings these are. */
  cluster: string;
  /** A remote cluster: the console's apps domain, whether the console keeps
   * DNS records for the cluster's hostnames under it, and those records. */
  consoleAppsDomain?: string;
  consoleRecords?: boolean;
  clusterDNS?: ClusterDNS;
};

/** The records the console keeps for a remote cluster's hostnames. */
export type ClusterDNS = { records: DNSRecord[]; message?: string; syncedAt?: string };

// ---- Hetzner Cloud API (api_hcloud.go) -------------------------------------

export type CloudFirewallStatus = {
  /** InSync, Applying, Off, Error. */
  state: string;
  id?: number;
  name?: string;
  rules?: number;
  servers?: number;
  revision?: string;
  message?: string;
};

export type LoadBalancerStatus = {
  /** Creating, Waiting, Active, Draining, Error. */
  state: string;
  /** DNS points at the Load Balancer. */
  active?: boolean;
  id?: number;
  name?: string;
  type?: string;
  location?: string;
  ipv4?: string;
  ipv6?: string;
  targets?: number;
  healthyTargets?: number;
  drainingSince?: string;
  message?: string;
};

export type CloudServer = { node: string; id: number; name: string; location?: string; labelled?: boolean };

/** What a cluster's Hetzner Cloud reconciler last reported. */
export type HCloudStatus = {
  servers?: CloudServer[];
  firewall?: CloudFirewallStatus;
  loadBalancer?: LoadBalancerStatus;
  message?: string;
  syncedAt?: string;
};

export type HCloud = {
  tokenSet: boolean;
  platform: "cloud" | "dedicated" | string;
  /** The hcloud cloud-controller-manager runs (chosen at install time). */
  ccm: boolean;
  /** The hcloud-volumes StorageClass exists (CSI driver). */
  volumes: boolean;
  /** The ingress accepts the PROXY protocol from the private network. */
  loadBalancerReady: boolean;
  firewall: "sync" | "off";
  loadBalancer: { enabled: boolean; type?: string; location?: string };
  status?: HCloudStatus;
};

export type HCloudCheck = { servers: number; nodes: { node: string; server: string; location: string }[]; locations: string[] };

export type HCloudTokenSaved = { settings: Settings; check: HCloudCheck; warning?: string };

export type VolumeClassInfo = { id: "local-nvme" | "hcloud-volume"; available: boolean; reason?: string };

/** managed: no record yet, but Kwerft creates it. */
export type DNSCheck = { hostname: string; addresses: string[]; expected: string[]; ok: boolean; managed?: boolean; message: string };

export type PasskeyHolder = { email: string; name: string; passkeys: number; totp: boolean; recoveryCodes: number; stranded: boolean };

export type AppsSaved = { settings: Settings; zone?: string; warning?: string };

export const settingsApi = {
  get: (cluster?: string) => request<Settings>(`/settings${clusterQuery(cluster)}`),
  dnsCheck: (hostname: string, wildcard = false) =>
    request<DNSCheck>("/settings/dns-check", { method: "POST", json: { hostname, wildcard } }),
  passkeyHolders: () => request<PasskeyHolder[]>("/settings/passkeys"),
  moveConsole: (hostname: string, confirm: string) =>
    request<Settings>("/settings/console-domain", { method: "PUT", json: { hostname, confirm } }),
  saveApps: (s: { appsDomain: string; tls: "http01" | "dns01"; manageRecords: boolean; token?: string }, cluster?: string) =>
    request<AppsSaved>(`/settings/apps${clusterQuery(cluster)}`, { method: "PUT", json: { ...s, provider: s.tls === "dns01" || s.manageRecords ? "hetzner" : "" } }),
  saveHCloudToken: (token: string) => request<HCloudTokenSaved>("/settings/hcloud-token", { method: "PUT", json: { token } }),
  removeHCloudToken: () => request<{ settings: Settings }>("/settings/hcloud-token", { method: "DELETE" }),
  saveHCloud: (s: { firewall: "sync" | "off"; loadBalancer: { enabled: boolean; type?: string; location?: string } }) =>
    request<Settings>("/settings/hcloud", { method: "PUT", json: s }),
  volumeClasses: () => request<VolumeClassInfo[]>("/volume-classes"),
};

export const isTemporaryHost = (host: string) => host.endsWith(".sslip.io");

/** A hostname covered by the wildcard certificate: exactly one label below the apps domain. */
export function underWildcard(host: string, appsDomain?: string) {
  if (!appsDomain || !host.endsWith("." + appsDomain)) return false;
  const label = host.slice(0, -appsDomain.length - 1);
  return label !== "" && !label.includes(".");
}
