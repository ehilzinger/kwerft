// Pods of Apps and Tasks: replicas, live logs and shells. The server reaches
// Kubernetes as the signed-in user, so what a role may do is Kubernetes RBAC
// talking; `access` in the replicas answer says so ahead of time.
import { ApiError, request } from "./api";

export type Tone = "ok" | "warn" | "bad" | "mute";

export type ContainerInfo = {
  name: string;
  ready: boolean;
  state: "running" | "waiting" | "terminated";
  reason?: string;
  message?: string;
  restarts: number;
  /** Why the previous instance ended: its logs are there with `previous`. */
  lastTermination?: { reason: string; exitCode: number; finishedAt: string };
};

export type Replica = {
  name: string;
  node: string;
  phase: string;
  /** What `kubectl get pods` would show: Running, CrashLoopBackOff, … */
  status: string;
  tone: Tone;
  message?: string;
  ready: boolean;
  restarts: number;
  created: string;
  /** null without metrics-server. */
  cpuMillis: number | null;
  memoryBytes: number | null;
  containers: ContainerInfo[];
};

export type Replicas = { pods: Replica[]; metrics: boolean; access: { logs: boolean; exec: boolean } };

export type Recording = {
  id: string;
  user: string;
  project: string;
  app: string;
  pod: string;
  container: string;
  shell: string;
  ip: string;
  started: string;
  ended?: string;
  durationSeconds: number;
  reason?: string;
  exitCode?: number;
  bytes: number;
  inputRecorded: boolean;
};

const enc = encodeURIComponent;
const appBase = (project: string, app: string) => `/projects/${enc(project)}/apps/${enc(app)}`;
const taskBase = (project: string, task: string) => `/projects/${enc(project)}/tasks/${enc(task)}`;

export const podsApi = {
  appPods: (project: string, app: string) => request<Replicas>(`${appBase(project, app)}/pods`),
  taskPods: (project: string, task: string) => request<Replicas>(`${taskBase(project, task)}/pods`),
  /** Owners and admins only. */
  recordings: () => request<Recording[]>("/recordings"),
  recordingURL: (id: string) => `/api/v1/recordings/${enc(id)}`,
};

/** Log stream paths for LogViewer. */
export const appLogsPath = (project: string, app: string) => `${appBase(project, app)}/logs`;
export const taskLogsPath = (project: string, task: string) => `${taskBase(project, task)}/logs`;

export type ShellKind = "auto" | "bash" | "sh";

export function shellURL(project: string, app: string, pod: string, o: { shell: ShellKind; container?: string; cols: number; rows: number }) {
  const scheme = location.protocol === "https:" ? "wss:" : "ws:";
  const q = new URLSearchParams({ shell: o.shell, cols: String(o.cols), rows: String(o.rows) });
  if (o.container) q.set("container", o.container);
  return `${scheme}//${location.host}/api/v1${appBase(project, app)}/pods/${enc(pod)}/shell?${q}`;
}

// ---- log streams -------------------------------------------------------------

export type LogLine = { pod: string; container: string; ts?: string; text: string; truncated?: boolean };

export type LogEvent =
  | { type: "start"; pods: string[]; container: string; follow: boolean; limits: { tail: number; maxLine: number; linesPerSecond: number; maxPods: number } }
  | { type: "line"; line: LogLine }
  | { type: "status"; pod: string; state: "streaming" | "waiting" | "ended" | "error"; message?: string }
  | { type: "dropped"; lines: number }
  | { type: "end"; reason: "complete" | "idle" | "maxDuration" | "signedOut"; message?: string };

export type LogQuery = { pod?: string; container?: string; follow?: boolean; tail?: number; since?: number; previous?: boolean };

/**
 * streamLogs reads a Server-Sent Events log stream with fetch (not
 * EventSource), so an error before the stream is a normal ApiError and the
 * caller decides about reconnecting. It resolves when the stream ends.
 */
export async function streamLogs(path: string, query: LogQuery, onEvent: (e: LogEvent) => void, signal: AbortSignal): Promise<void> {
  const q = new URLSearchParams();
  for (const [k, v] of Object.entries(query)) {
    if (v === undefined || v === "" || v === false) continue;
    q.set(k, v === true ? "1" : String(v));
  }
  const res = await fetch(`/api/v1${path}?${q}`, { headers: { Accept: "text/event-stream" }, credentials: "same-origin", signal });
  if (!res.ok || !res.body) {
    let message = `${res.status} ${res.statusText}`;
    let field: string | undefined;
    try {
      const body = (await res.json()) as { error?: string; field?: string };
      message = body.error ?? message;
      field = body.field;
    } catch {
      /* not JSON */
    }
    throw new ApiError(res.status, message, field);
  }
  const reader = res.body.pipeThrough(new TextDecoderStream()).getReader();
  let buf = "";
  for (;;) {
    const { value, done } = await reader.read();
    if (done) return;
    buf += value;
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
      if (!name || !data) continue; // comments (": ping")
      const payload = JSON.parse(data);
      onEvent(name === "line" ? { type: "line", line: payload as LogLine } : ({ type: name, ...payload } as LogEvent));
    }
  }
}

// ---- formatting ----------------------------------------------------------------

/** "web-7c9d8-x2kq" → "x2kq", as the blueprint tags log lines. */
export const shortPod = (name: string) => name.slice(name.lastIndexOf("-") + 1) || name;

export function cpuText(m: number | null) {
  if (m === null) return "—";
  return m >= 1000 ? `${(m / 1000).toFixed(m >= 10000 ? 0 : 1)}` : `${m}m`;
}

export function memText(b: number | null) {
  if (b === null) return "—";
  const mi = b / (1 << 20);
  return mi >= 1024 ? `${(mi / 1024).toFixed(1)}Gi` : `${Math.round(mi)}Mi`;
}

/** Age the way kubectl prints it: 45s, 12m, 3h, 2d. */
export function age(iso: string, now = Date.now()) {
  const s = Math.max(0, Math.floor((now - new Date(iso).getTime()) / 1000));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m`;
  if (s < 86400) return `${Math.floor(s / 3600)}h`;
  return `${Math.floor(s / 86400)}d`;
}
