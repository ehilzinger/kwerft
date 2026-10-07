// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Settings › Updates (docs/phase6-upgrades.md › API and UI): the versions
// each cluster runs, what could be installed, the update policy, upgrades
// with their live progress, and the history. Owners start and cancel
// upgrades and change the policy; admins read. The server enforces it.

import { ApiError, request } from "./api";
import { LOCAL, clusterQuery } from "./clusters";

export type Component = "Kwerft" | "Kubernetes";
export type Policy = "Off" | "Notify" | "AutoPatch";

export type UpgradePhase = "Pending" | "Queued" | "Preflight" | "Backup" | "Running" | "Verifying" | "RollingBack"
  | "Succeeded" | "RolledBack" | "Failed" | "Cancelled";

export type UpgradeCheck = { check: string; ok: boolean; warning?: boolean; message?: string };
export type UpgradeStep = { id: string; label: string; state: "Running" | "Done" | "Skipped" | "Failed" | string; detail?: string; at?: string };
export type UpgradeNode = { name: string; version?: string; state: "Waiting" | "Draining" | "Upgrading" | "Done" | "Failed" | string; message?: string };
export type Versions = { kwerft?: string; kubernetes?: string };

export type Upgrade = {
  name: string;
  cluster: string;
  component: Component;
  version: string;
  /** The owner's email, or "auto-update". */
  requestedBy?: string;
  auto: boolean;
  acceptDataRollback?: boolean;
  phase: UpgradePhase;
  from?: Versions;
  preflight: UpgradeCheck[];
  steps: UpgradeStep[];
  nodes: UpgradeNode[];
  backup?: { etcdSnapshot?: string; database?: string; helmRevisions?: Record<string, number> };
  reason?: string;
  message?: string;
  cancelRequestedBy?: string;
  /** The "Upgrade all" an agent cluster's Upgrade belongs to; held: waiting for its turn. */
  fleet?: string;
  held?: boolean;
  /** On the console's Upgrade of an "Upgrade all": how its agent clusters fare. */
  agentClusters?: { finished: boolean; stopped: boolean; message: string };
  cancellable: boolean;
  finished: boolean;
  createdAt: string;
  startedAt?: string;
  finishedAt?: string;
};

export type Window = { days: string[]; start: string; duration: string; timeZone?: string };
export type UpdatePolicy = { policy: Policy; channel: "stable" | "edge"; kubernetesPatches: boolean; window?: Window };

export type Available = {
  component: Component;
  version: string;
  kind: "Patch" | "Minor";
  /** Markdown, without the installer's "Install" section. */
  notes?: string;
  allowed: boolean;
  reason?: string;
};

export type ClusterUpdates = {
  name: string;
  connected: boolean;
  kwerft?: string;
  kubernetes?: string;
  available: Available[];
  active?: Upgrade;
  upgradable: boolean;
  message?: string;
};

export type Updates = {
  policy: UpdatePolicy;
  checkedAt?: string;
  checking: boolean;
  error?: string;
  autoPatchPausedBy?: string;
  nextWindow?: string;
  windowOpen: boolean;
  current: Versions;
  available: Available[];
  clusters: ClusterUpdates[];
  canUpgrade: boolean;
  /** What "Upgrade all" would do now; absent when no agent cluster needs it. */
  upgradeAll?: UpgradeAll;
};

/** A cluster an "Upgrade all" leaves out; warning: it should be upgraded but cannot be now. */
export type FleetSkip = { cluster: string; reason: string; warning?: boolean };

/** The console to version (when console), then the agent clusters one after another. */
export type UpgradeAll = { version: string; console: boolean; clusters: string[]; skipped: FleetSkip[] };

export type UpgradeAllAnswer = { upgrade?: Upgrade; preflight?: Preflight; fleet: string; members: Upgrade[]; skipped: FleetSkip[] };

export type UpgradeRequest = {
  cluster?: string;
  component: Component;
  version: string;
  acceptDataRollback?: boolean;
  password?: string;
  confirmVersion?: string;
};

export type Preflight = {
  cluster: string;
  component: Component;
  version: string;
  from?: string;
  kind: "Patch" | "Minor";
  checks: UpgradeCheck[];
  blocked: boolean;
  /** Type the version again (Kubernetes minor). */
  confirmVersion: boolean;
  /** Not rollback-safe: starting needs acceptDataRollback. */
  dataRollback: boolean;
};

export const updateKeys = {
  overview: ["updates"] as const,
  history: ["upgrades"] as const,
  upgrade: (cluster: string, name: string) => ["upgrade", cluster, name] as const,
  log: (cluster: string, name: string) => ["upgrade-log", cluster, name] as const,
};

const enc = encodeURIComponent;

export const updatesApi = {
  overview: () => request<Updates>("/updates"),
  check: () => request<{ requestedAt: string }>("/updates/check", { method: "POST" }),
  resumeAutoPatch: () => request<{ resumed: string }>("/updates/resume-autopatch", { method: "POST" }),
  /** Turning AutoPatch on takes the password (enablesAutoPatch). */
  setPolicy: (p: UpdatePolicy & { password?: string }) => request<UpdatePolicy>("/settings/updates", { method: "PUT", json: p }),
  history: (cluster?: string) => request<Upgrade[]>(`/upgrades${clusterQuery(cluster)}`),
  upgrade: (name: string, cluster?: string) => request<Upgrade>(`/upgrades/${enc(name)}${clusterQuery(cluster)}`),
  log: (name: string, cluster?: string) => request<{ log: string; truncated: boolean }>(`/upgrades/${enc(name)}/log${clusterQuery(cluster)}`),
  preflight: (r: UpgradeRequest) => request<Preflight>("/upgrades/preflight", { method: "POST", json: r }),
  start: (r: UpgradeRequest) => request<{ upgrade: Upgrade; preflight: Preflight }>("/upgrades", { method: "POST", json: r }),
  startAll: (r: { version: string; acceptDataRollback?: boolean; password: string }) =>
    request<UpgradeAllAnswer>("/upgrades/all", { method: "POST", json: r }),
  cancel: (name: string, cluster?: string) => request<Upgrade>(`/upgrades/${enc(name)}${clusterQuery(cluster)}`, { method: "DELETE" }),
};

// ---- the live status stream --------------------------------------------------------------

export type UpgradeEvent = { type: "upgrade"; upgrade: Upgrade } | { type: "end"; phase?: string; reason?: string } | { type: "gone" };

/** Splits Server-Sent Events off a buffer; returns the events and what is left. */
export function parseSSE(buf: string): { events: { name: string; data: string }[]; rest: string } {
  const events: { name: string; data: string }[] = [];
  let cut: number;
  while ((cut = buf.indexOf("\n\n")) >= 0) {
    const block = buf.slice(0, cut);
    buf = buf.slice(cut + 2);
    let name = "";
    let data = "";
    for (const line of block.split("\n")) {
      if (line.startsWith("event: ")) name = line.slice(7);
      else if (line.startsWith("data: ")) data += line.slice(6);
    }
    if (name && data) events.push({ name, data }); // comments (": ping") carry neither
  }
  return { events, rest: buf };
}

/**
 * streamUpgrade follows an Upgrade's status (GET …/events) until the stream
 * ends. It resolves when it ends (finished, or the server closed it) and
 * throws when it could not connect; the caller reconnects.
 */
export async function streamUpgrade(name: string, cluster: string, onEvent: (e: UpgradeEvent) => void, signal: AbortSignal): Promise<void> {
  const res = await fetch(`/api/v1/upgrades/${enc(name)}/events${clusterQuery(cluster)}`, {
    headers: { Accept: "text/event-stream" }, credentials: "same-origin", signal,
  });
  if (!res.ok || !res.body) {
    let message = `${res.status} ${res.statusText}`;
    try {
      message = ((await res.json()) as { error?: string }).error ?? message;
    } catch {
      /* not JSON */
    }
    throw new ApiError(res.status, message);
  }
  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
  let buf = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) return;
    const parsed = parseSSE(buf + value);
    buf = parsed.rest;
    for (const ev of parsed.events) {
      const data = JSON.parse(ev.data);
      if (ev.name === "upgrade") onEvent({ type: "upgrade", upgrade: data as Upgrade });
      else if (ev.name === "end") onEvent({ type: "end", ...(data as { phase?: string; reason?: string }) });
      else if (ev.name === "gone") onEvent({ type: "gone" });
    }
  }
}

// ---- presentation --------------------------------------------------------------------------

export type Pill = { pill: "ok" | "warn" | "bad" | "off" | "info"; label: string };

export function phasePill(phase: UpgradePhase | string): Pill {
  switch (phase) {
    case "Pending":
    case "Queued": return { pill: "off", label: "Queued" };
    case "Preflight": return { pill: "info", label: "Checking" };
    case "Backup": return { pill: "info", label: "Backing up" };
    case "Running": return { pill: "info", label: "Upgrading" };
    case "Verifying": return { pill: "info", label: "Verifying" };
    case "RollingBack": return { pill: "warn", label: "Rolling back" };
    case "Succeeded": return { pill: "ok", label: "Succeeded" };
    case "RolledBack": return { pill: "warn", label: "Rolled back" };
    case "Failed": return { pill: "bad", label: "Failed" };
    case "Cancelled": return { pill: "off", label: "Cancelled" };
  }
  return { pill: "info", label: phase };
}

/** One line of the installer-style progress view. */
export type Line = { mark: "ok" | "run" | "fail" | "skip" | "wait"; label: string; detail?: string };

const waiting = (u: Upgrade) => u.phase === "Pending" || u.phase === "Queued";
/** The run got to the installer (or the nodes): Running and everything after it. */
const ranInstaller = (u: Upgrade) =>
  ["Running", "Verifying", "Succeeded", "RollingBack", "RolledBack"].includes(u.phase) ||
  (u.phase === "Failed" && (u.steps.length > 0 || u.nodes.length > 0 || !!u.backup?.etcdSnapshot));

function checksLine(u: Upgrade): Line {
  const failed = u.preflight.filter((c) => !c.ok && !c.warning);
  const warnings = u.preflight.filter((c) => c.warning).length;
  const detail = u.preflight.length
    ? `${u.preflight.length - failed.length - warnings} of ${u.preflight.length} passed${warnings ? ` · ${warnings} ${warnings === 1 ? "warning" : "warnings"}` : ""}`
    : undefined;
  if (u.phase === "Failed" && u.reason === "Preflight") {
    return { mark: "fail", label: "Checks", detail: failed.map((c) => c.message || c.check).join(" · ") || u.message };
  }
  if (waiting(u)) return { mark: "wait", label: "Checks", detail: u.message };
  if (u.phase === "Preflight") return { mark: "run", label: "Checks", detail: "nodes, disk, image and chart, other operations" };
  if (u.phase === "Cancelled" && !u.preflight.length) return { mark: "skip", label: "Checks", detail: "cancelled before they ran" };
  return { mark: "ok", label: "Checks", detail };
}

function backupLine(u: Upgrade): Line {
  const b = u.backup;
  const parts = [b?.database && "database copy", b?.etcdSnapshot && `etcd snapshot ${b.etcdSnapshot}`,
    b?.helmRevisions && `${Object.keys(b.helmRevisions).length} Helm releases recorded`].filter(Boolean) as string[];
  const detail = parts.join(" · ") || undefined;
  if (u.phase === "Backup") return { mark: "run", label: "Backup", detail: detail ?? "copying the database, taking an etcd snapshot" };
  if (u.phase === "Failed" && u.reason === "Backup") return { mark: "fail", label: "Backup", detail: u.message };
  if (ranInstaller(u)) return { mark: "ok", label: "Backup", detail };
  if (u.phase === "Cancelled" || u.phase === "Failed") return { mark: "skip", label: "Backup", detail: u.phase === "Cancelled" ? "cancelled" : "not taken" };
  return { mark: "wait", label: "Backup" };
}

const stepMark = (state: string): Line["mark"] =>
  state === "Done" ? "ok" : state === "Skipped" ? "skip" : state === "Failed" ? "fail" : state === "Running" ? "run" : "wait";

const nodeMark = (state: string): Line["mark"] =>
  state === "Done" ? "ok" : state === "Failed" ? "fail" : state === "Waiting" ? "wait" : "run";

/**
 * The progress of an Upgrade as the installer prints it: the console's
 * checks and backup, then each installer stage (Kwerft) or each node
 * (Kubernetes), then verification and a rollback when there was one.
 */
export function progressLines(u: Upgrade): Line[] {
  const lines: Line[] = [checksLine(u), backupLine(u)];
  if (u.component === "Kubernetes") {
    for (const n of u.nodes) {
      lines.push({ mark: nodeMark(n.state), label: n.name, detail: [n.version, n.state !== "Done" && n.state, n.message].filter(Boolean).join(" · ") || undefined });
    }
    if (!u.nodes.length && u.phase === "Running") lines.push({ mark: "run", label: "Nodes", detail: "waiting for the first node" });
  } else {
    for (const s of u.steps) lines.push({ mark: stepMark(s.state), label: s.label || s.id, detail: s.detail });
    if (!u.steps.length && u.phase === "Running") lines.push({ mark: "run", label: "Installer", detail: "starting" });
  }
  const verifyFailed = u.reason === "Verify";
  if (u.phase === "Verifying") lines.push({ mark: "run", label: "Verify", detail: u.message });
  else if (u.phase === "Succeeded") lines.push({ mark: "ok", label: "Verify", detail: "console, CRDs, node agent, certificate and apps are back" });
  else if (verifyFailed) lines.push({ mark: "fail", label: "Verify", detail: u.message });
  if (u.phase === "RollingBack") lines.push({ mark: "run", label: "Rollback", detail: u.message });
  else if (u.phase === "RolledBack") lines.push({ mark: "ok", label: "Rollback", detail: `back on ${u.from?.kwerft ?? "the previous version"}` });
  const fleet = fleetLine(u);
  if (fleet) lines.push(fleet);
  return lines;
}

/** The agent clusters of an "Upgrade all", on the console's Upgrade (its AgentClusters condition). */
export function fleetLine(u: Upgrade): Line | undefined {
  const a = u.agentClusters;
  if (!a) return undefined;
  const detail = a.message.replace(/^Agent clusters \((.*)\)\.(.*)$/, "$1.$2");
  return { mark: a.stopped ? "fail" : a.finished ? "ok" : "run", label: "Agent clusters", detail };
}

/** The steps of an "Upgrade all", in order. */
export function fleetSteps(ua: UpgradeAll, consoleVersion?: string): string[] {
  const out = ua.console ? [`The console: Kwerft ${consoleVersion ?? "?"} → ${ua.version}`] : [];
  for (const c of ua.clusters) out.push(`${c}: its agent to ${ua.version}`);
  return out;
}

/** Turning AutoPatch (or its Kubernetes patches) on asks for the password, as the server does; turning it off does not. */
export function enablesAutoPatch(before: Pick<UpdatePolicy, "policy" | "kubernetesPatches">, after: Pick<UpdatePolicy, "policy" | "kubernetesPatches">): boolean {
  return (after.policy === "AutoPatch" && before.policy !== "AutoPatch") || (after.kubernetesPatches && !before.kubernetesPatches);
}

/** "3m 41s", "52s", "1h 4m". */
export function durationText(ms: number): string {
  const s = Math.max(0, Math.round(ms / 1000));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}

/** The closing line: "Done in 4m 12s." or what went wrong. */
export function summaryLine(u: Upgrade): { tone: "ok" | "warn" | "bad" | "dim"; text: string } | undefined {
  const took = u.startedAt && u.finishedAt ? ` in ${durationText(new Date(u.finishedAt).getTime() - new Date(u.startedAt).getTime())}` : "";
  switch (u.phase) {
    case "Succeeded": return { tone: "ok", text: `Done${took}. ${u.component} runs ${u.version}.` };
    case "RolledBack": return { tone: "warn", text: `Rolled back${took}. ${u.message ?? ""}`.trim() };
    case "Failed": return { tone: "bad", text: `Failed${u.reason ? ` (${u.reason})` : ""}${took}. ${u.message ?? ""}`.trim() };
    case "Cancelled": return { tone: "dim", text: u.message ?? "Cancelled before anything changed." };
  }
  return undefined;
}

/** A Kubernetes minor (or an unknown running version) asks for the version typed again. */
export const needsTypedConfirmation = (component: Component, kind?: string) => component === "Kubernetes" && kind !== "Patch";

/** The newest allowed target per component: what the notification and the dot announce. */
export function updateNotices(u?: Pick<Updates, "policy" | "available">): Available[] {
  if (!u || u.policy.policy === "Off") return [];
  const out: Available[] = [];
  for (const c of ["Kwerft", "Kubernetes"] as Component[]) {
    const a = u.available.find((x) => x.component === c && x.allowed);
    if (a) out.push(a);
  }
  return out;
}

export function noticeText(a: Available): { title: string; detail: string } {
  const what = a.component === "Kwerft" ? `Kwerft ${a.version}` : `Kubernetes ${a.version}`;
  const kind = a.kind === "Patch" ? "Patch release" : "Minor release";
  return { title: what, detail: a.component === "Kubernetes" ? `${kind} of k3s, tested with this Kwerft release` : kind };
}

export const DAYS = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"] as const;

/** "Sun 03:00 for 2h (Europe/Berlin)", "every day 03:00 for 2h (UTC)". */
export function windowText(w?: Window): string {
  if (!w) return "no window";
  const days = !w.days.length || w.days.length === 7 ? "every day" : w.days.join(", ");
  return `${days} ${w.start} for ${w.duration || "2h"} (${w.timeZone || "UTC"})`;
}

/** The browser's time zone, for a new window. */
export function localTimeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

// ---- release notes ------------------------------------------------------------------------

export type Block =
  | { type: "h"; level: number; text: string }
  | { type: "p"; text: string }
  | { type: "ul"; items: string[] }
  | { type: "code"; text: string };

/**
 * A small Markdown reader for release notes: headings, paragraphs, bullet
 * lists and fenced code. Everything is rendered as text (React escapes it);
 * links show as their text, images and HTML not at all.
 */
export function notesBlocks(md: string): Block[] {
  const out: Block[] = [];
  const lines = md.replace(/\r\n/g, "\n").split("\n");
  let para: string[] = [];
  let list: string[] | undefined;
  const flush = () => {
    const text = inline(para.join(" "));
    if (text) out.push({ type: "p", text });
    para = [];
    if (list) out.push({ type: "ul", items: list });
    list = undefined;
  };
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i]!;
    if (/^\s*```/.test(line)) {
      flush();
      const code: string[] = [];
      for (i++; i < lines.length && !/^\s*```/.test(lines[i]!); i++) code.push(lines[i]!);
      out.push({ type: "code", text: code.join("\n") });
      continue;
    }
    const h = /^(#{1,6})\s+(.*)$/.exec(line);
    if (h) {
      flush();
      out.push({ type: "h", level: h[1]!.length, text: inline(h[2]!) });
      continue;
    }
    const li = /^\s*[-*+]\s+(.*)$/.exec(line);
    if (li) {
      if (para.length && inline(para.join(" "))) out.push({ type: "p", text: inline(para.join(" ")) });
      para = [];
      (list ??= []).push(inline(li[1]!));
      continue;
    }
    if (!line.trim()) {
      flush();
      continue;
    }
    if (list && /^\s+\S/.test(line)) {
      list[list.length - 1] += " " + inline(line.trim());
      continue;
    }
    if (list) flush();
    para.push(line.trim());
  }
  flush();
  return out;
}

/** Inline Markdown to plain text: links keep their text, emphasis marks go. */
export function inline(s: string): string {
  return s
    .replace(/!\[([^\]]*)\]\([^)]*\)/g, "$1")
    .replace(/\[([^\]]+)\]\([^)]*\)/g, "$1")
    .replace(/<[^>]+>/g, "")
    .replace(/(\*\*|__)(.+?)\1/g, "$2")
    .replace(/(^|[^\w*])[*_]([^*_\s][^*_]*?)[*_](?=[^\w*]|$)/g, "$1$2")
    .trim();
}

export { LOCAL };
