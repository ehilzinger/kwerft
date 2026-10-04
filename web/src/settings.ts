// Settings: the console's hostname, the apps base domain and how app
// certificates are issued (ConsoleSettings in the cluster). Everyone reads
// them; owners and admins change them. The DNS token is write-only.

import { request } from "./api";

export type CertificateState = {
  name: string;
  purpose: "console" | "console-next" | "console-previous" | "apps-wildcard";
  hostnames: string[];
  state: "valid" | "issuing" | "failed";
  message?: string;
  notAfter?: string;
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
  /** "*.<appsDomain>" once the wildcard listener serves apps. */
  wildcardDomain?: string;
  publicAddresses: string[];
  certificates: CertificateState[];
  ready?: { status: boolean; reason: string; message: string };
};

export type DNSCheck = { hostname: string; addresses: string[]; expected: string[]; ok: boolean; message: string };

export type PasskeyHolder = { email: string; name: string; passkeys: number; totp: boolean; recoveryCodes: number; stranded: boolean };

export type AppsSaved = { settings: Settings; zone?: string; warning?: string };

export const settingsApi = {
  get: () => request<Settings>("/settings"),
  dnsCheck: (hostname: string, wildcard = false) =>
    request<DNSCheck>("/settings/dns-check", { method: "POST", json: { hostname, wildcard } }),
  passkeyHolders: () => request<PasskeyHolder[]>("/settings/passkeys"),
  moveConsole: (hostname: string, confirm: string) =>
    request<Settings>("/settings/console-domain", { method: "PUT", json: { hostname, confirm } }),
  saveApps: (s: { appsDomain: string; tls: "http01" | "dns01"; token?: string }) =>
    request<AppsSaved>("/settings/apps", { method: "PUT", json: { ...s, provider: s.tls === "dns01" ? "hetzner" : "" } }),
};

export const isTemporaryHost = (host: string) => host.endsWith(".sslip.io");

/** A hostname covered by the wildcard certificate: exactly one label below the apps domain. */
export function underWildcard(host: string, appsDomain?: string) {
  if (!appsDomain || !host.endsWith("." + appsDomain)) return false;
  const label = host.slice(0, -appsDomain.length - 1);
  return label !== "" && !label.includes(".");
}
