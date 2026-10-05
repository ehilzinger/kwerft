// Client for the workload API (projects and apps). The server acts as the
// signed-in user against Kubernetes, so a 403 here is Kubernetes RBAC talking.
import { request, type User } from "./api";
import type { Build } from "./builds";

export type Phase = "running" | "deploying" | "stopped" | "pending" | "failed";

export type Project = {
  name: string;
  displayName?: string;
  phase: "ready" | "pending" | "failed";
  reason?: string;
  message?: string;
  apps: number;
  created: string;
  /** Team: every console member with their console role. Members: only the listed users. */
  access: ProjectAccess;
  /** The signed-in user's role here; in a Members project the one given there. */
  role?: User["role"];
  /** Owners and admins only. */
  members?: ProjectMember[];
  /** The cluster the project lives in ("local": the console's own). */
  cluster?: string;
};

export type ProjectAccess = "Team" | "Members";
export type ProjectMember = { user: string; name?: string; role: "developer" | "viewer" };
export type ProjectAccessInfo = { project: string; access: ProjectAccess; members: ProjectMember[] };

export type AppSummary = {
  name: string;
  project: string;
  cluster?: string;
  source: {
    type: "image" | "git";
    image?: string;
    repository?: string;
    branch?: string;
    // Git apps, with defaults filled in.
    path?: string;
    builder?: "dockerfile" | "railpack";
    dockerfile?: string;
    connection?: string;
    autoDeploy?: boolean;
    pinnedImage?: string;
  };
  image: string;
  readyReplicas: number;
  replicas: number;
  stateful: boolean;
  phase: Phase;
  reason?: string;
  message?: string;
  urls: string[];
  revision: number;
  updated: string;
};

export type EnvVar = {
  name: string;
  value?: string;
  valueFrom?: {
    secretKeyRef?: { name: string; key: string };
    configMapKeyRef?: { name: string; key: string };
    fieldRef?: { fieldPath: string };
  };
};
export type AppPort = { container: number; public?: string; protocol?: "TCP" | "UDP" };
export type HealthCheck = { http?: string; port: number };
export type Size = "small" | "medium" | "large" | "custom";

export type AppSpec = {
  source: {
    image?: { ref: string; pullSecret?: string };
    git?: GitSource;
  };
  replicas?: number;
  size?: Size;
  resources?: unknown;
  command?: string[];
  env?: EnvVar[];
  ports?: AppPort[];
  allowFrom?: string[];
  egress?: "none" | "https" | "all";
  // A disk per replica (size), or a shared Volume of the project (volume, see jobs.ts).
  volumes?: { path: string; size?: string; class?: string; volume?: string; readOnly?: boolean }[];
  healthCheck?: HealthCheck;
  /** Seconds a stopping replica keeps serving before SIGTERM (default 5; only Apps with ports). */
  drainSeconds?: number;
};

export type GitSource = {
  repository: string;
  branch?: string;
  /** Build context inside the repository. */
  path?: string;
  builder?: "dockerfile" | "railpack";
  /** Relative to path. */
  dockerfile?: string;
  /** GitConnection name; empty for public repositories. */
  connection?: string;
  autoDeploy?: boolean;
  /** Set by a rollback: runs this image instead of the newest build. */
  pinnedImage?: string;
};

/** build and commit: the Build that produced image, for Git apps. */
export type Revision = { number: number; image: string; generation: number; build?: string; commit?: string; time: string };
export type Condition = { type: string; status: "True" | "False" | "Unknown"; reason: string; message: string; observedGeneration?: number; lastTransitionTime: string };

export type App = {
  apiVersion: string;
  kind: "App";
  metadata: { name: string; namespace: string; resourceVersion: string; generation: number; creationTimestamp: string; annotations?: Record<string, string> };
  spec: AppSpec;
  status?: {
    observedGeneration?: number;
    image?: string;
    revision?: number;
    history?: Revision[];
    readyReplicas?: number;
    urls?: string[];
    conditions?: Condition[];
  };
  /** Git apps: the newest build. */
  latestBuild?: Build;
};

const appPath = (project: string, name: string) => `/projects/${encodeURIComponent(project)}/apps/${encodeURIComponent(name)}`;

export const workloads = {
  projects: () => request<Project[]>("/projects"),
  /** cluster: owners and admins choose; the default is the console's own. */
  createProject: (p: { name: string; displayName?: string; cluster?: string }) => request<Project>("/projects", { method: "POST", json: p }),
  deleteProject: (name: string) => request<void>(`/projects/${encodeURIComponent(name)}`, { method: "DELETE" }),
  // Who reaches a project (owners and admins manage it).
  projectAccess: (name: string) => request<ProjectAccessInfo>(`/projects/${encodeURIComponent(name)}/access`),
  setProjectAccess: (name: string, access: ProjectAccess, members: { user: string; role: ProjectMember["role"] }[]) =>
    request<ProjectAccessInfo>(`/projects/${encodeURIComponent(name)}/access`, { method: "PUT", json: { access, members } }),
  addProjectMember: (name: string, user: string, role: ProjectMember["role"]) =>
    request<ProjectAccessInfo>(`/projects/${encodeURIComponent(name)}/members`, { method: "POST", json: { user, role } }),
  removeProjectMember: (name: string, user: string) =>
    request<ProjectAccessInfo>(`/projects/${encodeURIComponent(name)}/members/${encodeURIComponent(user)}`, { method: "DELETE" }),

  apps: (project?: string) => request<AppSummary[]>(project ? `/apps?project=${encodeURIComponent(project)}` : "/apps"),
  app: (project: string, name: string) => request<App>(appPath(project, name)),
  createApp: (project: string, name: string, spec: AppSpec) =>
    request<App>(`/projects/${encodeURIComponent(project)}/apps`, { method: "POST", json: { name, spec } }),
  // generation guards against overwriting someone else's settings change.
  updateApp: (project: string, name: string, spec: AppSpec, generation: number) =>
    request<App>(appPath(project, name), { method: "PUT", json: { spec, generation } }),
  deleteApp: (project: string, name: string) => request<void>(appPath(project, name), { method: "DELETE" }),
  rollback: (project: string, name: string, revision: number) =>
    request<App>(`${appPath(project, name)}/rollback`, { method: "POST", json: { revision } }),
  restart: (project: string, name: string) => request<App>(`${appPath(project, name)}/restart`, { method: "POST" }),
  scale: (project: string, name: string, replicas: number) =>
    request<App>(`${appPath(project, name)}/scale`, { method: "PATCH", json: { replicas } }),
};

/**
 * What the signed-in user's role allows. The server enforces it; the UI only
 * avoids dead ends. With a project, its role there counts (in a Members
 * project, the one given there); without one, deploy means "somewhere".
 */
export function abilities(user?: User, project?: Project, projects?: Project[]) {
  const role = user?.role;
  const writes = (r?: string) => r === "owner" || r === "admin" || r === "developer";
  return {
    deploy: project ? writes(project.role ?? role) : writes(role) || !!projects?.some((p) => writes(p.role)),
    manageProjects: role === "owner" || role === "admin",
  };
}

export const sizes: { id: Exclude<Size, "custom">; label: string; note: string }[] = [
  { id: "small", label: "Small", note: "0.25 vCPU · 256 MiB" },
  { id: "medium", label: "Medium", note: "0.5 vCPU · 512 MiB" },
  { id: "large", label: "Large", note: "1 vCPU · 2 GiB" },
];

export const NAME_RE = /^[a-z]([-a-z0-9]*[a-z0-9])?$/;
export const PROJECT_RE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;
export const HOST_RE = /^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]([-a-z0-9]*[a-z0-9])?$/;

export function ago(iso?: string, now = Date.now()) {
  if (!iso) return "—";
  const s = Math.max(0, Math.round((now - new Date(iso).getTime()) / 1000));
  if (s < 45) return "just now";
  if (s < 3600) return `${Math.max(1, Math.round(s / 60))} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return `${Math.round(s / 86400)} d ago`;
}

export const hostOf = (url: string) => url.replace(/^https?:\/\//, "");

/** "HostnameInUse" → "Hostname in use". */
export function words(reason?: string) {
  if (!reason) return "";
  const s = reason.replace(/([a-z])([A-Z])/g, "$1 $2").toLowerCase();
  return s.charAt(0).toUpperCase() + s.slice(1);
}

/** A small YAML writer for the deploy wizard's preview. */
export function toYAML(v: unknown, indent = 0): string {
  const pad = "  ".repeat(indent);
  const scalar = (x: unknown) => {
    if (typeof x === "string") return /^[\w./@:+-]+$/.test(x) && !/^(true|false|null|yes|no|on|off|[\d.]+)$/i.test(x) && !x.includes(": ") ? x : JSON.stringify(x);
    return String(x);
  };
  if (Array.isArray(v)) {
    return v
      .map((item) => {
        if (item !== null && typeof item === "object") {
          const body = toYAML(item, indent + 1).trimStart();
          return `${pad}- ${body}`;
        }
        return `${pad}- ${scalar(item)}`;
      })
      .join("\n");
  }
  if (v !== null && typeof v === "object") {
    return Object.entries(v as Record<string, unknown>)
      .filter(([, x]) => x !== undefined && !(Array.isArray(x) && x.length === 0))
      .map(([k, x]) => {
        if (x !== null && typeof x === "object") return `${pad}${k}:\n${toYAML(x, indent + 1)}`;
        return `${pad}${k}: ${scalar(x)}`;
      })
      .join("\n");
  }
  return pad + scalar(v);
}
