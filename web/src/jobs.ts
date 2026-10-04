// Client for the jobs API: shared Volumes, Tasks (one-off runs), Schedules and
// the read-only Domains list. Like workloads.ts, the server acts as the
// signed-in user, so a 403 is Kubernetes RBAC talking.
import { request } from "./api";
import type { Condition, EnvVar, Size } from "./workloads";

// ---- volumes ---------------------------------------------------------------

export type VolumeClass = "local-nvme" | "hcloud-volume";

export type Volume = {
  name: string;
  project: string;
  size: string;
  class: VolumeClass;
  capacity?: string;
  phase: "bound" | "pending" | "lost" | "failed" | "deleting";
  reason?: string;
  message?: string;
  /** "App/api", "Schedule/backup", "Task/import-1". */
  usedBy: string[];
  created: string;
};

/** Answer to deleting a Volume that is still mounted: deletion waits for these. */
export type VolumeInUse = { usedBy: string[]; reason: "InUse"; message: string };

/** A mount in an App or Task: a disk per replica (size) or a shared Volume (volume). */
export type AppVolume = { path: string; size?: string; class?: VolumeClass; volume?: string; readOnly?: boolean };

// ---- tasks -----------------------------------------------------------------

export type TaskPhase = "pending" | "running" | "succeeded" | "failed";

export type TaskSpec = {
  fromApp?: string;
  source?: { image?: { ref: string; pullSecret?: string } };
  size?: Size;
  resources?: unknown;
  command?: string[];
  env?: EnvVar[];
  envOverrides?: EnvVar[];
  egress?: "none" | "https" | "all";
  volumes?: AppVolume[];
  /** A Go duration: 30m, 1h30m. */
  timeout?: string;
  retries?: number;
  ttlSecondsAfterFinished?: number;
  onSuccess?: { restart?: string[] };
};

export type TaskSummary = {
  name: string;
  project: string;
  phase: TaskPhase;
  reason?: string;
  message?: string;
  fromApp?: string;
  schedule?: string;
  image?: string;
  /** The console user who started it; empty for a run its Schedule started on time. */
  startedBy?: string;
  scheduledAt?: string;
  /** Names of the env overrides; the values are only in the Task itself. */
  overrides: string[];
  restart: string[];
  created: string;
  started?: string;
  finished?: string;
  exitCode?: number;
};

type Meta = {
  name: string;
  namespace: string;
  resourceVersion: string;
  generation: number;
  creationTimestamp: string;
  labels?: Record<string, string>;
  annotations?: Record<string, string>;
  ownerReferences?: { kind: string; name: string; controller?: boolean }[];
};

export type Task = {
  apiVersion: string;
  kind: "Task";
  metadata: Meta;
  spec: TaskSpec;
  status?: {
    phase?: "Pending" | "Running" | "Succeeded" | "Failed";
    image?: string;
    job?: string;
    pod?: string;
    startTime?: string;
    completionTime?: string;
    exitCode?: number;
    conditions?: Condition[];
  };
};

// ---- schedules -------------------------------------------------------------

export type Concurrency = "Forbid" | "Replace" | "Allow";

export type ScheduleSpec = {
  schedule: string;
  timeZone?: string;
  suspend?: boolean;
  concurrency?: Concurrency;
  startingDeadline?: string;
  history?: { succeeded?: number; failed?: number };
  task: TaskSpec;
};

export type Schedule = {
  apiVersion: string;
  kind: "Schedule";
  metadata: Meta;
  spec: ScheduleSpec;
  status?: {
    lastScheduleTime?: string;
    nextScheduleTime?: string;
    lastSuccessTime?: string;
    lastFailureTime?: string;
    active?: string[];
    conditions?: Condition[];
  };
};

export type ScheduleSummary = {
  name: string;
  project: string;
  schedule: string;
  timeZone?: string;
  suspend: boolean;
  concurrency: Concurrency;
  fromApp?: string;
  image?: string;
  restart: string[];
  nextRun?: string;
  lastScheduled?: string;
  lastSuccess?: string;
  lastFailure?: string;
  active: string[];
  lastRun?: TaskSummary;
  phase: "scheduled" | "suspended" | "waiting" | "pending" | "failed";
  reason?: string;
  message?: string;
  created: string;
};

export type SchedulePreview = { next: string[]; timeZone: string };

// ---- domains ---------------------------------------------------------------

export type Domain = {
  name: string;
  project: string;
  hostname: string;
  app?: string;
  listener?: string;
  certificate: "valid" | "issuing" | "pending" | "failed" | "disabled";
  reason?: string;
  message?: string;
  notAfter?: string;
  created: string;
};

// ---- client ----------------------------------------------------------------

const enc = encodeURIComponent;
const projectPath = (project: string) => `/projects/${enc(project)}`;
const qs = (params: Record<string, string | number | undefined>) => {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== "") q.set(k, String(v));
  const s = q.toString();
  return s ? `?${s}` : "";
};

export type TaskFilter = { project?: string; app?: string; schedule?: string; phase?: TaskPhase; limit?: number };

export const jobs = {
  volumes: (project?: string) => request<Volume[]>(`/volumes${qs({ project })}`),
  createVolume: (project: string, v: { name: string; size: string; class: VolumeClass }) =>
    request<Volume>(`${projectPath(project)}/volumes`, { method: "POST", json: v }),
  resizeVolume: (project: string, name: string, size: string) =>
    request<Volume>(`${projectPath(project)}/volumes/${enc(name)}`, { method: "PATCH", json: { size } }),
  /** Resolves to the users it waits for when the Volume is still mounted, else undefined. */
  deleteVolume: (project: string, name: string) =>
    request<VolumeInUse | undefined>(`${projectPath(project)}/volumes/${enc(name)}`, { method: "DELETE" }),

  tasks: (f: TaskFilter = {}) => request<TaskSummary[]>(`/tasks${qs(f)}`),
  task: (project: string, name: string) => request<Task>(`${projectPath(project)}/tasks/${enc(name)}`),
  /** Leave out name for a generated one (app-run-x7k2p). */
  createTask: (project: string, spec: TaskSpec, name?: string) =>
    request<Task>(`${projectPath(project)}/tasks`, { method: "POST", json: { name, spec } }),
  cancelTask: (project: string, name: string) => request<Task>(`${projectPath(project)}/tasks/${enc(name)}/cancel`, { method: "POST" }),
  deleteTask: (project: string, name: string) => request<void>(`${projectPath(project)}/tasks/${enc(name)}`, { method: "DELETE" }),

  schedules: (project?: string) => request<ScheduleSummary[]>(`/schedules${qs({ project })}`),
  schedule: (project: string, name: string) => request<Schedule>(`${projectPath(project)}/schedules/${enc(name)}`),
  createSchedule: (project: string, name: string, spec: ScheduleSpec) =>
    request<Schedule>(`${projectPath(project)}/schedules`, { method: "POST", json: { name, spec } }),
  // generation guards against overwriting someone else's change.
  updateSchedule: (project: string, name: string, spec: ScheduleSpec, generation: number) =>
    request<Schedule>(`${projectPath(project)}/schedules/${enc(name)}`, { method: "PUT", json: { spec, generation } }),
  deleteSchedule: (project: string, name: string) => request<void>(`${projectPath(project)}/schedules/${enc(name)}`, { method: "DELETE" }),
  suspend: (project: string, name: string, suspend: boolean) =>
    request<Schedule>(`${projectPath(project)}/schedules/${enc(name)}/${suspend ? "suspend" : "resume"}`, { method: "POST" }),
  runSchedule: (project: string, name: string, envOverrides: EnvVar[]) =>
    request<Task>(`${projectPath(project)}/schedules/${enc(name)}/run`, { method: "POST", json: { envOverrides } }),
  preview: (schedule: string, timeZone: string) => request<SchedulePreview>(`/schedules/preview${qs({ schedule, timeZone })}`),

  domains: (project?: string) => request<Domain[]>(`/domains${qs({ project })}`),
};

// ---- presentation helpers -------------------------------------------------

/** "4m 12s", "38 s", "3 h 41m"; from start to end (or now). */
export function duration(start?: string, end?: string, now = Date.now()) {
  if (!start) return "—";
  const s = Math.max(0, Math.round(((end ? new Date(end).getTime() : now) - new Date(start).getTime()) / 1000));
  if (s < 60) return `${s} s`;
  if (s < 3600) return `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, "0")}s`;
  return `${Math.floor(s / 3600)} h ${String(Math.floor((s % 3600) / 60)).padStart(2, "0")}m`;
}

const dayNames = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];
const monthNames = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];

/** A time in the future, the way a person says it: "in 12 min", "today 14:30", "tomorrow 02:30", "Mon 1 Nov 04:00". */
export function when(iso?: string, now = new Date()) {
  if (!iso) return "—";
  const t = new Date(iso);
  const mins = Math.round((t.getTime() - now.getTime()) / 60000);
  const hm = t.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
  if (mins >= 0 && mins < 60) return mins <= 1 ? "in a minute" : `in ${mins} min`;
  const day = (d: Date) => new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
  const days = Math.round((day(t) - day(now)) / 86400000);
  if (days === 0) return `${t.getHours() >= 18 && mins > 0 ? "tonight" : "today"} ${hm}`;
  if (days === 1) return `tomorrow ${hm}`;
  if (days === -1) return `yesterday ${hm}`;
  return `${t.getDate()} ${monthNames[t.getMonth()]}${t.getFullYear() !== now.getFullYear() ? ` ${t.getFullYear()}` : ""} ${hm}`;
}

/** A Go duration as people write it: 1h0m0s → 1h, 30m0s → 30m. */
export const prettyDuration = (d?: string) => (d ?? "").replace(/m0s$/, "m").replace(/h0m$/, "h");

export function shortDate(iso?: string) {
  if (!iso) return "—";
  const t = new Date(iso);
  return `${t.getDate()} ${monthNames[t.getMonth()]} ${t.getFullYear()}`;
}

const pad = (n: number) => String(n).padStart(2, "0");
const ordinal = (n: number) => `${n}${n % 10 === 1 && n !== 11 ? "st" : n % 10 === 2 && n !== 12 ? "nd" : n % 10 === 3 && n !== 13 ? "rd" : "th"}`;
const num = (s: string) => (/^\d+$/.test(s) ? Number(s) : undefined);

function days(field: string): string | undefined {
  const names = (n: number) => dayNames[n % 7]!.slice(0, 3);
  if (field === "1-5") return "weekdays";
  if (field === "0,6" || field === "6,0") return "weekends";
  const range = /^(\d)-(\d)$/.exec(field);
  if (range) return `${names(Number(range[1]))}–${names(Number(range[2]))}`;
  const list = field.split(",").map(num);
  if (list.every((d) => d !== undefined && d <= 7)) return list.map((d) => (list.length === 1 ? dayNames[d! % 7]! : names(d!))).join(", ");
  return undefined;
}

/**
 * A cron expression in words, for the common shapes ("daily at 03:30",
 * "every 6 hours", "weekdays at 07:00"); "" when it is better read as is.
 * The server's parser (robfig/cron) is the authority; this is only a label.
 */
export function describeCron(expr: string): string {
  const e = expr.trim();
  const every = /^@every\s+(.+)$/.exec(e);
  if (every) return `every ${every[1]}`;
  const descriptors: Record<string, string> = {
    "@hourly": "every hour", "@daily": "daily at 00:00", "@midnight": "daily at 00:00",
    "@weekly": "weekly, Sunday at 00:00", "@monthly": "monthly, the 1st at 00:00", "@yearly": "yearly, 1 Jan at 00:00", "@annually": "yearly, 1 Jan at 00:00",
  };
  if (e.startsWith("@")) return descriptors[e] ?? "";
  const f = e.split(/\s+/);
  if (f.length !== 5) return "";
  const [mi, h, dom, mon, dow] = f as [string, string, string, string, string];
  const m = num(mi);
  if (mi === "*" && h === "*" && dom === "*" && mon === "*" && dow === "*") return "every minute";
  const stepMin = /^\*\/(\d+)$/.exec(mi);
  if (stepMin && h === "*" && dom === "*" && mon === "*" && dow === "*") return `every ${stepMin[1]} minutes`;
  if (m === undefined || m > 59) return "";
  if (h === "*" && dom === "*" && mon === "*" && dow === "*") return m === 0 ? "every hour" : `hourly at :${pad(m)}`;
  const stepH = /^\*\/(\d+)$/.exec(h);
  if (stepH && dom === "*" && mon === "*" && dow === "*") return `every ${stepH[1]} hours${m ? ` at :${pad(m)}` : ""}`;
  const hours = h.split(",").map(num);
  if (hours.some((x) => x === undefined || x > 23)) return "";
  const at = `at ${hours.map((x) => `${pad(x!)}:${pad(m)}`).join(" and ")}`;
  if (dom === "*" && mon === "*" && dow === "*") return `daily ${at}`;
  if (dom === "*" && mon === "*") {
    const d = days(dow);
    return d ? `${d === "weekdays" || d === "weekends" ? d : `weekly, ${d}`} ${at}` : "";
  }
  const day = num(dom);
  if (day !== undefined && mon === "*" && dow === "*") return `monthly, the ${ordinal(day)} ${at}`;
  const month = num(mon);
  if (day !== undefined && month !== undefined && month >= 1 && month <= 12 && dow === "*") return `yearly, ${day} ${monthNames[month - 1]} ${at}`;
  return "";
}

/** Who started a run, for the runs table. */
export function startedBy(t: TaskSummary) {
  if (t.startedBy) return { who: t.startedBy, how: t.schedule ? "run now" : t.fromApp ? "run as job" : "task" };
  return { who: "schedule", how: "" };
}

/** IANA time zones the browser knows, for the time zone picker. */
export function timeZones(): string[] {
  try {
    return (Intl as unknown as { supportedValuesOf?: (k: string) => string[] }).supportedValuesOf?.("timeZone") ?? [];
  } catch {
    return [];
  }
}
