// Alerting (docs/phase3.md, W3's API): firing, silenced and resolved alerts,
// alert rules and notification channels, plus the helpers the Monitoring tabs
// and the Overview's "needs attention" feed share. The server is the
// authority on defaults and expressions; the defaults here only fill
// placeholders and plain-language previews.
import { ApiError, request } from "./api";
import type { Build } from "./builds";
import type { Domain, ScheduleSummary } from "./jobs";
import { words, type AppSummary } from "./workloads";

// ---- types -------------------------------------------------------------------

export type Severity = "critical" | "warning" | "info";
export type AlertState = "firing" | "silenced" | "resolved";

export type Alert = {
  fingerprint: string;
  rule: string;
  severity: Severity;
  state: AlertState;
  summary: string;
  description: string;
  project?: string;
  app?: string;
  labels: Record<string, string>;
  startsAt: string;
  endsAt?: string;
  silencedUntil?: string;
  /** Deep link into the console, usually the app's Logs tab. */
  consoleURL?: string;
  /** IDs of the silences that mute it (Alertmanager's status.silencedBy); needed to unsilence. */
  silencedBy?: string[];
};

export type Silence = { id: string; endsAt?: string; comment?: string; createdBy?: string };
export type SilenceInput = { fingerprint: string; duration: string; comment: string };

export type AlertCondition =
  | "CrashLooping" | "Restarts" | "MemoryHigh" | "CPUHigh" | "VolumeFillingUp" | "NodeMemoryPressure" | "NodeDiskPressure"
  | "CertificateExpiring" | "ScheduleFailing" | "BuildFailing" | "HTTPErrorRate" | "HTTPLatency" | "Custom";

export type AlertScope = { projects: string[]; apps: string[] };

export type Rule = {
  name: string;
  condition: AlertCondition;
  threshold?: number;
  /** Go durations, as Kubernetes stores them: "15m0s", "168h0m0s". */
  window?: string;
  for?: string;
  expr?: string;
  scope: AlertScope;
  severity: Severity;
  channels: string[];
  disabled: boolean;
  default: boolean;
  firing: number;
  ready: boolean;
  message?: string;
  effectiveExpr: string;
};

export type RuleInput = Omit<Rule, "default" | "firing" | "ready" | "message" | "effectiveExpr">;

export type ChannelType = "slack" | "email" | "webhook" | "ntfy";

export type Channel = {
  name: string;
  type: ChannelType;
  slack?: { channel?: string };
  email?: { to: string[]; from: string; smtpHost: string; username?: string };
  ntfy?: { server: string; topic: string };
  sendResolved: boolean;
  secretSet: boolean;
  ready: boolean;
  message?: string;
  lastTest?: string;
  lastTestError?: string;
};

/** Secrets are write-only: send them only when typed, so an edit keeps the stored ones. */
export type ChannelInput = Omit<Channel, "secretSet" | "ready" | "message" | "lastTest" | "lastTestError"> & {
  webhook?: Record<string, never>;
  url?: string;
  password?: string;
  token?: string;
};

export type TestResult = { ok: boolean; message: string };

// ---- client ------------------------------------------------------------------

const enc = encodeURIComponent;

export const alertsApi = {
  alerts: (state: AlertState) => request<Alert[]>(`/alerts?state=${state}`),
  silence: (s: SilenceInput) => request<Silence>("/alerts/silences", { method: "POST", json: s }),
  unsilence: (id: string) => request<void>(`/alerts/silences/${enc(id)}`, { method: "DELETE" }),

  rules: () => request<Rule[]>("/alerts/rules"),
  createRule: (r: RuleInput) => request<Rule>("/alerts/rules", { method: "POST", json: r }),
  updateRule: (r: RuleInput) => request<Rule>(`/alerts/rules/${enc(r.name)}`, { method: "PUT", json: r }),
  deleteRule: (name: string) => request<void>(`/alerts/rules/${enc(name)}`, { method: "DELETE" }),

  channels: () => request<Channel[]>("/alerts/channels"),
  createChannel: (c: ChannelInput) => request<Channel>("/alerts/channels", { method: "POST", json: c }),
  updateChannel: (c: ChannelInput) => request<Channel>(`/alerts/channels/${enc(c.name)}`, { method: "PUT", json: c }),
  deleteChannel: (name: string) => request<void>(`/alerts/channels/${enc(name)}`, { method: "DELETE" }),
  testChannel: (name: string) => request<TestResult>(`/alerts/channels/${enc(name)}/test`, { method: "POST" }),
};

/** Query keys shared by the Monitoring tabs, the Overview and the sidebar badge. */
export const alertKeys = {
  alerts: (state: AlertState) => ["alerts", state] as const,
  rules: ["alert-rules"] as const,
  channels: ["alert-channels"] as const,
};

/**
 * The console does not offer alerting (yet): the endpoint is missing or the
 * monitoring stack is not reachable. Pages then show what they can without it.
 */
export const alertingUnavailable = (e: unknown) => e instanceof ApiError && [404, 405, 501, 502, 503].includes(e.status);

/** Who may do what; the server enforces it, the UI only avoids dead ends. */
export function alertAbilities(role?: string) {
  const admin = role === "owner" || role === "admin";
  return {
    /** Silence and unsilence alerts, create and change rules from the built-in conditions. */
    act: admin || role === "developer",
    /** Custom expressions and notification channels. */
    admin,
  };
}

// ---- severities --------------------------------------------------------------

export const severities: Severity[] = ["critical", "warning", "info"];
const severityRank: Record<string, number> = { critical: 0, warning: 1, info: 2 };
export const severityTone: Record<Severity, "bad" | "warn" | "info"> = { critical: "bad", warning: "warn", info: "info" };

/** Critical first, then warning, then info; unknown severities last. */
export function compareSeverity(a: string, b: string) {
  return (severityRank[a] ?? 3) - (severityRank[b] ?? 3);
}

/** Most severe first; within a severity, the newest first. */
export function sortAlerts<T extends Pick<Alert, "severity" | "startsAt">>(list: T[]): T[] {
  return [...list].sort((a, b) => compareSeverity(a.severity, b.severity) || b.startsAt.localeCompare(a.startsAt));
}

// ---- durations ---------------------------------------------------------------

const units: Record<string, number> = { ms: 0.001, s: 1, m: 60, h: 3600, d: 86400, w: 604800 };

/**
 * Seconds in a Go duration ("1h30m0s", "168h") or a duration as people type
 * it ("15m", "7d", "2 h"). undefined when it is not one.
 */
export function parseDuration(s?: string): number | undefined {
  const t = (s ?? "").replace(/\s+/g, "").toLowerCase();
  if (!t) return undefined;
  if (!/^(\d+(\.\d+)?(ms|s|m|h|d|w))+$/.test(t)) return undefined;
  let total = 0;
  for (const [, n, u] of t.matchAll(/(\d+(?:\.\d+)?)(ms|s|m|h|d|w)/g)) total += Number(n) * units[u!]!;
  return total;
}

/** A duration for the API: Go's syntax, which has no days ("7d" → "168h"). */
export function goDuration(seconds: number): string {
  if (seconds <= 0) return "0s";
  const h = Math.floor(seconds / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  const s = Math.round(seconds % 60);
  return `${h ? `${h}h` : ""}${m ? `${m}m` : ""}${s ? `${s}s` : ""}` || "0s";
}

/** How people say a duration: "15 minutes", "1 hour", "7 days", "1 h 30 min". */
export function humanDuration(seconds: number): string {
  const plural = (n: number, w: string) => `${n} ${w}${n === 1 ? "" : "s"}`;
  if (seconds % 86400 === 0 && seconds >= 86400) return plural(seconds / 86400, "day");
  if (seconds % 3600 === 0 && seconds >= 3600) return plural(seconds / 3600, "hour");
  if (seconds % 60 === 0 && seconds >= 60) return plural(seconds / 60, "minute");
  if (seconds < 60) return plural(Math.round(seconds), "second");
  const parts: string[] = [];
  const d = Math.floor(seconds / 86400);
  const h = Math.floor((seconds % 86400) / 3600);
  const m = Math.floor((seconds % 3600) / 60);
  if (d) parts.push(`${d} d`);
  if (h) parts.push(`${h} h`);
  if (m) parts.push(`${m} min`);
  return parts.join(" ");
}

/** A stored duration shortened for an input: "15m0s" → "15m", "168h0m0s" → "7d". */
export function shortDuration(go?: string): string {
  const s = parseDuration(go);
  if (s === undefined) return go ?? "";
  if (s >= 86400 && s % 86400 === 0) return `${s / 86400}d`;
  if (s >= 3600 && s % 3600 === 0) return `${s / 3600}h`;
  if (s >= 60 && s % 60 === 0) return `${s / 60}m`;
  return goDuration(s);
}

// ---- conditions --------------------------------------------------------------

/** What a rule's scope can name: apps (and their projects), projects only, or nothing (nodes, custom). */
export type ScopeKind = "apps" | "projects" | "none";

export type ConditionInfo = {
  label: string;
  /** What it watches, in a few words, for the condition picker. */
  hint: string;
  /** The threshold's unit and its default; absent: the condition has none. */
  threshold?: { unit: "count" | "percent" | "ms"; default: number; label: string; suffix: string };
  window?: { default: string; label: string };
  for?: { default: string; label: string };
  scope: ScopeKind;
  /** "All projects", "All nodes": what an empty scope means. */
  everything: string;
  severity: Severity;
};

// Defaults mirror W3's (internal/alerting); the server applies its own when a field is empty.
export const conditions: Record<AlertCondition, ConditionInfo> = {
  CrashLooping: {
    label: "Crash looping", hint: "A container keeps crashing and restarting", scope: "apps", everything: "All apps", severity: "critical",
    for: { default: "1m", label: "Fires after" },
  },
  Restarts: {
    label: "Restarts", hint: "Containers restart often", scope: "apps", everything: "All apps", severity: "warning",
    threshold: { unit: "count", default: 5, label: "More than", suffix: "restarts" }, window: { default: "15m", label: "Within" },
  },
  MemoryHigh: {
    label: "Memory high", hint: "Memory use close to the app's limit", scope: "apps", everything: "All apps", severity: "warning",
    threshold: { unit: "percent", default: 90, label: "Above", suffix: "% of the limit" }, for: { default: "10m", label: "For" },
  },
  CPUHigh: {
    label: "CPU high", hint: "CPU use close to the app's limit", scope: "apps", everything: "All apps", severity: "warning",
    threshold: { unit: "percent", default: 90, label: "Above", suffix: "% of the limit" }, for: { default: "15m", label: "For" },
  },
  VolumeFillingUp: {
    label: "Volume filling up", hint: "A volume is almost full, or will be soon", scope: "projects", everything: "All volumes", severity: "warning",
    threshold: { unit: "percent", default: 85, label: "More than", suffix: "% used" }, window: { default: "7d", label: "Or full within" },
  },
  NodeMemoryPressure: {
    label: "Node memory low", hint: "A server is running out of memory", scope: "none", everything: "All nodes", severity: "critical",
    threshold: { unit: "percent", default: 10, label: "Less than", suffix: "% available" }, for: { default: "10m", label: "For" },
  },
  NodeDiskPressure: {
    label: "Node disk low", hint: "A server is running out of disk space", scope: "none", everything: "All nodes", severity: "critical",
    threshold: { unit: "percent", default: 10, label: "Less than", suffix: "% free" }, for: { default: "5m", label: "For" },
  },
  CertificateExpiring: {
    label: "Certificate expiring", hint: "A certificate is not renewed in time", scope: "projects", everything: "All certificates", severity: "warning",
    window: { default: "14d", label: "Expires within" },
  },
  ScheduleFailing: {
    label: "Schedule failing", hint: "A scheduled job failed or stopped succeeding", scope: "projects", everything: "All schedules", severity: "warning",
    window: { default: "", label: "Or no success within" },
  },
  BuildFailing: {
    label: "Build failing", hint: "An app's latest build failed", scope: "apps", everything: "All apps", severity: "warning",
  },
  HTTPErrorRate: {
    label: "HTTP errors", hint: "Too many requests fail with 5xx", scope: "apps", everything: "All apps", severity: "warning",
    threshold: { unit: "percent", default: 5, label: "More than", suffix: "% of requests fail" }, window: { default: "5m", label: "Over" },
  },
  HTTPLatency: {
    label: "Slow responses", hint: "Requests take too long (95th percentile)", scope: "apps", everything: "All apps", severity: "warning",
    threshold: { unit: "ms", default: 1000, label: "Slower than", suffix: "ms" }, window: { default: "5m", label: "Over" },
  },
  Custom: {
    label: "Custom expression", hint: "A MetricsQL expression; each result is an alert", scope: "none", everything: "What the expression returns", severity: "warning",
    for: { default: "", label: "For" },
  },
};

export const conditionOrder = Object.keys(conditions) as AlertCondition[];

export const conditionLabel = (c: string) => conditions[c as AlertCondition]?.label ?? c;

type Measured = Pick<Rule, "condition" | "threshold" | "window" | "for">;

/** The duration a rule uses, in seconds: its own, else the condition's default (0: none). */
function dur(own: string | undefined, def: string | undefined) {
  return parseDuration(own) ?? parseDuration(def) ?? 0;
}

/**
 * A rule's condition in words, defaults filled in: "More than 5 restarts in
 * 15 minutes", "Memory above 90 % of the limit for 10 minutes".
 */
export function describeCondition(r: Measured): string {
  const info = conditions[r.condition];
  if (!info) return r.condition;
  const n = r.threshold ?? info.threshold?.default;
  const w = dur(r.window, info.window?.default);
  const f = dur(r.for, info.for?.default);
  const forText = f ? ` for ${humanDuration(f)}` : "";
  switch (r.condition) {
    case "CrashLooping": return `Crash looping${f ? ` for ${humanDuration(f)}` : ""}`;
    case "Restarts": return `More than ${n} ${n === 1 ? "restart" : "restarts"} in ${humanDuration(w)}`;
    case "MemoryHigh": return `Memory above ${n} % of the limit${forText}`;
    case "CPUHigh": return `CPU above ${n} % of the limit${forText}`;
    case "VolumeFillingUp": return `Volume more than ${n} % full${w ? `, or full within ${humanDuration(w)}` : ""}`;
    case "NodeMemoryPressure": return `Less than ${n} % of a node's memory available${forText}`;
    case "NodeDiskPressure": return `Less than ${n} % of a node's disk free${forText}`;
    case "CertificateExpiring": return `Certificate expires within ${humanDuration(w)}`;
    case "ScheduleFailing": return `A schedule's last run failed${w ? `, or no success within ${humanDuration(w)}` : ""}`;
    case "BuildFailing": return "An app's latest build failed";
    case "HTTPErrorRate": return `More than ${n} % of requests fail (5xx) over ${humanDuration(w)}`;
    case "HTTPLatency": return `95th percentile response time above ${n} ms over ${humanDuration(w)}`;
    case "Custom": return `Custom expression${forText}`;
  }
}

/** "All projects", or the projects and apps a rule is narrowed to. */
export function describeScope(r: Pick<Rule, "condition" | "scope">): string {
  const info = conditions[r.condition];
  const items = [...(r.scope?.projects ?? []), ...(r.scope?.apps ?? [])];
  if (!info || info.scope === "none" || items.length === 0) return info?.everything ?? "Everything";
  return items.join(", ");
}

/** A problem with a duration field, or "" when it is fine (empty: the default). */
export function durationProblem(input: string): string {
  const t = input.trim();
  if (!t) return "";
  const s = parseDuration(t);
  if (s === undefined) return "Write a duration like 30s, 15m, 2h or 7d.";
  if (s < 1) return "Use at least one second.";
  return "";
}

/** A problem with a rule's threshold, or "". */
export function thresholdProblem(condition: AlertCondition, input: string): string {
  const t = input.trim();
  const info = conditions[condition]?.threshold;
  if (!t || !info) return "";
  if (!/^\d+$/.test(t)) return "Enter a whole number.";
  const n = Number(t);
  if (info.unit === "percent" && (n < 1 || n > 100)) return "Enter a percentage between 1 and 100.";
  if (info.unit !== "percent" && n < 1) return "Enter a number above 0.";
  return "";
}

export const RULE_NAME_RE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;

// ---- channels ----------------------------------------------------------------

export const channelTypes: { id: ChannelType; label: string }[] = [
  { id: "slack", label: "Slack" },
  { id: "email", label: "Email" },
  { id: "webhook", label: "Webhook" },
  { id: "ntfy", label: "ntfy" },
];

export const channelTypeLabel = (t: string) => channelTypes.find((c) => c.id === t)?.label ?? t;

/** Where a channel delivers, without its secrets: "#ops-alerts", "ops@acme.dev", "ntfy.sh/acme-alerts". */
export function channelTarget(c: Pick<Channel, "type" | "slack" | "email" | "ntfy" | "secretSet">): string {
  switch (c.type) {
    case "slack": return c.slack?.channel || "the webhook's default channel";
    case "email": return c.email?.to?.length ? c.email.to.join(", ") : "no recipients";
    case "ntfy": return c.ntfy ? `${c.ntfy.server.replace(/^https:\/\//, "").replace(/\/+$/, "")}/${c.ntfy.topic}` : "—";
    case "webhook": return c.secretSet ? "URL stored" : "no URL";
    default: return "—";
  }
}

/** "Slack #ops-alerts · ntfy" for the rules table; names stand in for channels not (yet) known. */
export function describeChannels(names: string[], channels: Channel[] | undefined): string {
  if (names.length === 0) return "Console only";
  return names.map((n) => {
    const c = channels?.find((x) => x.name === n);
    if (!c) return n;
    if (c.type === "slack" && c.slack?.channel) return `Slack ${c.slack.channel}`;
    if (c.type === "email" && c.email?.to?.length === 1) return `Email ${c.email.to[0]}`;
    return `${channelTypeLabel(c.type)} ${c.name}`;
  }).join(" · ");
}

export type ChannelForm = {
  name: string; type: ChannelType; sendResolved: boolean;
  url: string; slackChannel: string;
  to: string; from: string; smtpHost: string; username: string; password: string;
  server: string; topic: string; token: string;
};

export function channelForm(c?: Channel): ChannelForm {
  return {
    name: c?.name ?? "", type: c?.type ?? "slack", sendResolved: c?.sendResolved ?? true,
    url: "", slackChannel: c?.slack?.channel ?? "",
    to: c?.email?.to.join(", ") ?? "", from: c?.email?.from ?? "", smtpHost: c?.email?.smtpHost ?? "", username: c?.email?.username ?? "", password: "",
    server: c?.ntfy?.server ?? "https://ntfy.sh", topic: c?.ntfy?.topic ?? "", token: "",
  };
}

/** The request for a channel form: only its type's settings, and secrets only when typed. */
export function channelInput(f: ChannelForm): ChannelInput {
  const t = (s: string) => s.trim() || undefined;
  const base: ChannelInput = { name: f.name.trim(), type: f.type, sendResolved: f.sendResolved };
  switch (f.type) {
    case "slack": return { ...base, slack: { channel: t(f.slackChannel) }, url: t(f.url) };
    case "webhook": return { ...base, webhook: {}, url: t(f.url) };
    case "email": return {
      ...base,
      email: { to: f.to.split(/[\s,;]+/).map((x) => x.trim()).filter(Boolean), from: f.from.trim(), smtpHost: f.smtpHost.trim(), username: t(f.username) },
      password: t(f.password),
    };
    case "ntfy": return { ...base, ntfy: { server: f.server.trim().replace(/\/+$/, "") || "https://ntfy.sh", topic: f.topic.trim() }, token: t(f.token) };
  }
}

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

/** The first problem with a channel before it is sent, or undefined. secretStored: an edit that keeps the stored secret. */
export function channelProblem(c: ChannelInput, secretStored: boolean): { field: string; message: string } | undefined {
  if (!RULE_NAME_RE.test(c.name)) return { field: "name", message: "Use lowercase letters, digits and dashes, like ops-slack." };
  if (c.name.length > 63) return { field: "name", message: "At most 63 characters." };
  const httpsURL = (field: string, what: string) => {
    if (!c.url) return secretStored ? undefined : { field, message: `Enter the ${what}.` };
    if (!/^https:\/\/[^\s/]+/.test(c.url)) return { field, message: "Use an https:// address." };
    return undefined;
  };
  switch (c.type) {
    case "slack": {
      const p = httpsURL("url", "incoming webhook URL");
      if (p) return p;
      if (c.slack?.channel && !/^[#@]?[\w.-]+$/.test(c.slack.channel)) return { field: "slack.channel", message: "A channel looks like #ops-alerts." };
      return undefined;
    }
    case "webhook": return httpsURL("url", "webhook URL");
    case "email": {
      const e = c.email!;
      if (e.to.length === 0) return { field: "email.to", message: "Enter at least one recipient." };
      const bad = e.to.find((x) => !EMAIL_RE.test(x));
      if (bad) return { field: "email.to", message: `${bad} is not an email address.` };
      if (!EMAIL_RE.test(e.from)) return { field: "email.from", message: "Enter the sender's address, like alerts@example.com." };
      if (!/^[a-z0-9.-]+:\d{1,5}$/i.test(e.smtpHost)) return { field: "email.smtpHost", message: "Enter host and port, like smtp.example.com:587." };
      return undefined;
    }
    case "ntfy": {
      const n = c.ntfy!;
      if (!/^https:\/\/[^\s/]+/.test(n.server)) return { field: "ntfy.server", message: "Use an https:// address, like https://ntfy.sh." };
      if (!/^[\w-]{1,64}$/.test(n.topic)) return { field: "ntfy.topic", message: "A topic is letters, digits, - and _." };
      return undefined;
    }
  }
}

// ---- links -------------------------------------------------------------------

/** The app's Logs tab. */
export const appLogsHref = (project: string, app: string) => `/apps/${enc(project)}/${enc(app)}?tab=logs`;

/**
 * Where an alert's "Logs" action goes: its consoleURL as a path when it points
 * at this console (so the router handles it), the app's Logs tab otherwise.
 * external: a link elsewhere, to open as is.
 */
export function alertLogsLink(a: Pick<Alert, "consoleURL" | "project" | "app">, origin: string): { href: string; external: boolean } | undefined {
  if (a.consoleURL) {
    try {
      const u = new URL(a.consoleURL, origin);
      if (u.protocol === "https:" || u.protocol === "http:") {
        if (u.origin === origin) return { href: u.pathname + u.search + u.hash, external: false };
        // Another name of this console (it moved): the same path here.
        if (u.pathname.startsWith("/apps/")) return { href: u.pathname + u.search + u.hash, external: false };
        return { href: u.href, external: true };
      }
    } catch {
      /* not a URL: fall back */
    }
  }
  if (a.project && a.app) return { href: appLogsHref(a.project, a.app), external: false };
  return undefined;
}

// ---- needs attention ---------------------------------------------------------

export type AttentionTone = "bad" | "warn" | "info";

export type AttentionItem = {
  key: string;
  tone: AttentionTone;
  icon: "alert" | "rocket" | "clock" | "shield" | "disk" | "box";
  title: string;
  /** Short label after the title: "Crash looping", "Build failed". */
  what: string;
  detail: string;
  /** When it started, for "4 min ago" and ordering. */
  since?: string;
  action: { label: string; href: string };
  /** Firing alert behind the item, when there is one. */
  alert?: Alert;
};

export type AttentionInput = {
  alerts?: Alert[];
  apps?: AppSummary[];
  /** Latest builds of Git apps, keyed "<project>/<app>". */
  builds?: Record<string, Build | undefined>;
  schedules?: ScheduleSummary[];
  domains?: Domain[];
  origin: string;
  now?: number;
};

const toneRank: Record<AttentionTone, number> = { bad: 0, warn: 1, info: 2 };
const STUCK_MS = 10 * 60 * 1000;
const CERT_STUCK_MS = 60 * 60 * 1000;
const CERT_SOON_MS = 14 * 86400 * 1000;

/**
 * The Overview's "needs attention" feed: firing alerts, plus what the console
 * knows without the monitoring stack (failed apps and latest builds, failing
 * schedules, domains without a certificate). A firing alert about the same
 * app, schedule or hostname hides the console's own item, so nothing shows twice.
 */
export function attentionItems(input: AttentionInput): AttentionItem[] {
  const now = input.now ?? Date.now();
  const items: AttentionItem[] = [];
  const covered = new Set<string>();

  for (const a of input.alerts ?? []) {
    if (a.state !== "firing") continue;
    const where = a.project && a.app ? `${a.project}/${a.app}` : a.project ?? a.labels?.node ?? a.labels?.hostname ?? "";
    if (a.project && a.app) covered.add(`app:${a.project}/${a.app}`);
    if (a.project && a.labels?.schedule) covered.add(`schedule:${a.project}/${a.labels.schedule}`);
    if (a.labels?.hostname) covered.add(`domain:${a.labels.hostname}`);
    const logs = alertLogsLink(a, input.origin);
    items.push({
      key: `alert:${a.fingerprint}`,
      tone: a.severity === "critical" ? "bad" : a.severity === "warning" ? "warn" : "info",
      icon: "alert",
      title: where || a.rule,
      what: where ? a.rule : conditionLabel(a.rule),
      detail: a.summary || a.description,
      since: a.startsAt,
      action: logs && !logs.external ? { label: a.app ? "Open logs" : "Open", href: logs.href } : { label: "Review", href: "/monitoring" },
      alert: a,
    });
  }

  for (const app of input.apps ?? []) {
    const key = `app:${app.project}/${app.name}`;
    if (covered.has(key)) continue;
    const id = `${app.project}/${app.name}`;
    const appHref = `/apps/${enc(app.project)}/${enc(app.name)}`;
    const build = input.builds?.[id];
    if (build?.phase === "failed") {
      items.push({
        key: `build:${id}`, tone: "bad", icon: "rocket", title: id, what: "Build failed",
        detail: `Build #${build.number || "?"}${build.message ? ` (${build.message})` : ""} failed${build.statusMessage ? `: ${build.statusMessage}` : "."}`,
        since: build.finished ?? build.created,
        action: { label: "Open build", href: `${appHref}?build=${enc(build.name)}` },
      });
      continue;
    }
    if (app.phase === "failed") {
      items.push({
        key, tone: "bad", icon: "box", title: id, what: words(app.reason) || "Failed",
        detail: app.message || "The app is not running.", since: app.updated,
        action: { label: "Open app", href: appHref },
      });
    } else if (app.phase === "deploying" && app.replicas > 0 && now - new Date(app.updated).getTime() > STUCK_MS) {
      items.push({
        key, tone: app.readyReplicas === 0 ? "bad" : "warn", icon: "box", title: id,
        what: app.readyReplicas === 0 ? "Unavailable" : "Not all replicas ready",
        detail: `${app.readyReplicas} of ${app.replicas} ${app.replicas === 1 ? "replica" : "replicas"} ready for over 10 minutes; it may be crash looping.`,
        since: app.updated,
        action: { label: "Open logs", href: appLogsHref(app.project, app.name) },
      });
    }
  }

  for (const s of input.schedules ?? []) {
    const id = `${s.project}/${s.name}`;
    if (covered.has(`schedule:${id}`) || s.suspend) continue;
    const r = s.lastRun;
    if (r && r.phase === "failed" && r.reason !== "Cancelled") {
      items.push({
        key: `schedule:${id}`, tone: "warn", icon: "clock", title: id, what: "Last run failed",
        detail: r.exitCode !== undefined ? `Exit code ${r.exitCode}.` : r.message || (r.reason ? `${words(r.reason)}.` : "The run failed."),
        since: r.finished ?? r.created,
        action: { label: "View the run", href: `/jobs/${enc(r.project)}/tasks/${enc(r.name)}` },
      });
    } else if (s.phase === "failed") {
      items.push({
        key: `schedule:${id}`, tone: "warn", icon: "clock", title: id, what: words(s.reason) || "Not scheduling",
        detail: s.message || "The schedule cannot run.", since: s.created,
        action: { label: "Open schedule", href: `/jobs/${enc(s.project)}/schedules/${enc(s.name)}` },
      });
    }
  }

  for (const d of input.domains ?? []) {
    if (covered.has(`domain:${d.hostname}`)) continue;
    const key = `domain:${d.project}/${d.name}`;
    const age = now - new Date(d.created).getTime();
    if (d.certificate === "failed") {
      items.push({
        key, tone: "bad", icon: "shield", title: d.hostname, what: "No certificate",
        detail: d.message || "Issuing the certificate failed; HTTPS does not work for this hostname.", since: d.created,
        action: { label: "Domains & TLS", href: "/network" },
      });
    } else if ((d.certificate === "issuing" || d.certificate === "pending") && age > CERT_STUCK_MS) {
      items.push({
        key, tone: "warn", icon: "shield", title: d.hostname, what: "Still no certificate",
        detail: d.message || "The certificate has been pending for over an hour. Check the hostname's DNS record.", since: d.created,
        action: { label: "Domains & TLS", href: "/network" },
      });
    } else if (d.certificate === "valid" && d.notAfter && new Date(d.notAfter).getTime() - now < CERT_SOON_MS) {
      items.push({
        key, tone: "warn", icon: "shield", title: d.hostname, what: "Certificate expiring",
        detail: expiryText(new Date(d.notAfter).getTime() - now),
        action: { label: "Domains & TLS", href: "/network" },
      });
    }
  }

  return items.sort((a, b) => toneRank[a.tone] - toneRank[b.tone] || (b.since ?? "").localeCompare(a.since ?? ""));
}

function expiryText(ms: number) {
  if (ms <= 0) return "It has expired: browsers refuse the site until it renews.";
  const days = Math.max(1, Math.round(ms / 86400000));
  return `It expires in ${days} ${days === 1 ? "day" : "days"} and has not renewed yet.`;
}

/** The banner: "2 issues need attention." and the first two, "billing-worker crash looping and postgres-main volume filling up". */
export function attentionHeadline(items: AttentionItem[]): { title: string; detail: string } {
  const n = items.length;
  if (n === 0) return { title: "", detail: "" };
  const say = (i: AttentionItem) => `${i.title} (${i.what.charAt(0).toLowerCase()}${i.what.slice(1)})`;
  const first = items.slice(0, 2).map(say);
  const rest = n - first.length;
  const detail = rest > 0 ? `${first.join(", ")} and ${rest} more.` : `${first.join(" and ")}.`;
  return { title: `${n} ${n === 1 ? "issue needs" : "issues need"} attention.`, detail };
}
