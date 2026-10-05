// Compose import and templates (Phase 6): both answer with a plan — the
// Apps, Volumes and secret sets to create, and warnings — first as a dry run,
// then created as the signed-in user in one request (undone if a step
// fails). Values from a Compose file are sent once and never come back:
// the plan names secret keys only.
import { request } from "./api";
import type { AppSpec } from "./workloads";

export type PlanWarning = { level: "warning" | "info"; service?: string; key?: string; message: string };
export type PlanApp = { name: string; service?: string; spec: AppSpec };
export type PlanVolume = { name: string; spec: { size: string; class?: string } };
export type PlanSecretSet = {
  name: string;
  /** The App this set belongs to (<app>-env), deleted with it. */
  app?: string;
  description?: string;
  generate?: string[];
  derived?: { key: string; template: string }[];
  /** Keys written from the Compose file (names only). */
  keys?: string[];
  /** Keys referenced without a value: set them on the Secrets page. */
  missing?: string[];
};
export type PlanProblem = { object: string; field?: string; message: string };
export type ImportPlan = {
  apps: PlanApp[];
  volumes: PlanVolume[];
  secretSets: PlanSecretSet[];
  warnings: PlanWarning[];
  renames: { kind: "App" | "Volume"; from: string; to: string }[];
  /** What to do next (a template's). */
  notes?: string;
  dryRun: boolean;
  /** What stops the plan: names taken, values Kubernetes refuses. */
  problems: PlanProblem[];
  /** After an apply: "App/web", "Volume/db-data", … */
  created: string[];
};

export type TemplateParam = {
  key: string;
  label: string;
  type: "name" | "identifier" | "text" | "enum" | "size" | "hostname" | "apps";
  hint?: string;
  default?: string;
  options?: string[];
  required?: boolean;
  pattern?: string;
  maxLength?: number;
  min?: string;
  max?: string;
  /** A hostname's suggestion is <name><suffix>.<apps domain>. */
  suffix?: string;
};
export type Template = {
  id: string;
  title: string;
  category: string;
  description: string;
  images: string[];
  /** The date the images were pinned. */
  pinned: string;
  parameters: TemplateParam[];
};

const enc = encodeURIComponent;

export const importApi = {
  compose: (project: string, compose: string, env: Record<string, string>, dryRun: boolean) =>
    request<ImportPlan>(`/projects/${enc(project)}/import/compose`, { method: "POST", json: { compose, env, dryRun } }),
  templates: () => request<Template[]>("/templates"),
  template: (project: string, id: string, parameters: Record<string, string>, dryRun: boolean) =>
    request<ImportPlan>(`/projects/${enc(project)}/templates/${enc(id)}`, { method: "POST", json: { parameters, dryRun } }),
};

/** Reads a .env file: KEY=value lines, # comments, optional export and quotes. */
export function parseDotEnv(text: string): { env: Record<string, string>; bad?: number } {
  const env: Record<string, string> = {};
  for (const [i, raw] of text.split("\n").entries()) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    const m = /^(?:export\s+)?([A-Za-z_][A-Za-z0-9_.-]*)\s*=\s*(.*)$/.exec(line);
    if (!m) return { env, bad: i + 1 };
    let value = m[2]!;
    const q = value[0];
    if ((q === '"' || q === "'") && value.length >= 2 && value.endsWith(q)) {
      value = value.slice(1, -1);
      if (q === '"') value = value.replace(/\\n/g, "\n").replace(/\\(["\\])/g, "$1");
    } else {
      value = value.replace(/\s+#.*$/, "");
    }
    env[m[1]!] = value;
  }
  return { env };
}

/** Warnings by service, file-wide ones ("") first, then in the plan's order. */
export function warningsByService(warnings: PlanWarning[]): [string, PlanWarning[]][] {
  const groups = new Map<string, PlanWarning[]>([["", []]]);
  for (const w of warnings) {
    const k = w.service ?? "";
    if (!groups.has(k)) groups.set(k, []);
    groups.get(k)!.push(w);
  }
  return [...groups.entries()].filter(([, ws]) => ws.length > 0);
}

export function countWarnings(warnings: PlanWarning[]) {
  return warnings.filter((w) => w.level === "warning").length;
}

/** One line per object, in creation order, for the review. */
export type PlanLine = { kind: "App" | "Volume" | "SecretSet"; name: string; detail: string; public: string[] };

export function planLines(plan: Pick<ImportPlan, "apps" | "volumes" | "secretSets">): PlanLine[] {
  const out: PlanLine[] = [];
  for (const v of plan.volumes) out.push({ kind: "Volume", name: v.name, detail: `${v.spec.size} ${v.spec.class ?? "local-nvme"}`, public: [] });
  const set = (s: PlanSecretSet): PlanLine => {
    const parts: string[] = [];
    if (s.keys?.length) parts.push(`${s.keys.join(", ")} from the file`);
    if (s.generate?.length) parts.push(`${s.generate.join(", ")} generated`);
    if (s.derived?.length) parts.push(`${s.derived.map((d) => d.key).join(", ")} derived`);
    if (s.missing?.length) parts.push(`${s.missing.join(", ")} to set`);
    return { kind: "SecretSet", name: s.name, detail: (parts.join(" · ") || "no keys yet") + (s.app ? `; deleted with ${s.app}` : ""), public: [] };
  };
  for (const s of plan.secretSets) if (!s.app) out.push(set(s));
  for (const a of plan.apps) {
    const src = a.spec.source.image ? a.spec.source.image.ref : a.spec.source.git ? `build of ${a.spec.source.git.repository}` : "";
    const ports = (a.spec.ports ?? []).map((p) => p.container);
    const replicas = a.spec.replicas ?? 1;
    const bits = [src, a.spec.size ?? "small", ...(replicas !== 1 ? [`${replicas} replicas`] : [])];
    if (ports.length) bits.push(`port ${[...new Set(ports)].join(", ")}`);
    const vols = (a.spec.volumes ?? []).map((v) => (v.volume ? `${v.volume} at ${v.path}` : v.size ? `${v.size} disk at ${v.path}` : ""));
    if (vols.some(Boolean)) bits.push(vols.filter(Boolean).join(", "));
    out.push({ kind: "App", name: a.name, detail: bits.join(" · "), public: (a.spec.ports ?? []).flatMap((p) => (p.public ? [p.public] : [])) });
  }
  for (const s of plan.secretSets) if (s.app) out.push(set(s));
  return out;
}

/** The form's starting values: defaults, hostnames under the apps domain. */
export function templateValues(t: Template, appsDomain: string | undefined, current: Record<string, string> = {}, touched: Set<string> = new Set()) {
  const out: Record<string, string> = {};
  for (const p of t.parameters) {
    if (touched.has(p.key) && p.key in current) {
      out[p.key] = current[p.key]!;
    } else if (p.type === "hostname" && !p.default) {
      out[p.key] = appsDomain ? `${out.name || "app"}${p.suffix ?? ""}.${appsDomain}` : "";
    } else {
      out[p.key] = p.default ?? "";
    }
  }
  return out;
}

const NAME = /^[a-z]([-a-z0-9]*[a-z0-9])?$/;
const HOST = /^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)+[a-z]([-a-z0-9]*[a-z0-9])?$/;

/** A quick check before the server's; it has the last word. */
export function paramProblem(p: TemplateParam, v: string): string | undefined {
  const value = v.trim();
  if (!value) {
    if (p.required) return p.type === "hostname" ? "Enter a hostname, like app.example.com." : `${p.label} is required.`;
    return ["hostname", "apps", "text"].includes(p.type) ? undefined : `${p.label} is required.`;
  }
  switch (p.type) {
    case "name": {
      const max = p.maxLength ?? 59;
      return NAME.test(value) && value.length <= max ? undefined : `Use lowercase letters, digits and dashes, starting with a letter (at most ${max}).`;
    }
    case "identifier":
      return /^[a-z_][a-z0-9_]{0,62}$/.test(value) ? undefined : "Use lowercase letters, digits and _, starting with a letter.";
    case "hostname":
      return HOST.test(value.toLowerCase()) ? undefined : "Enter a hostname, like app.example.com.";
    case "size":
      return /^\d+(\.\d+)?(Ki|Mi|Gi|Ti|K|M|G|T)?$/.test(value) ? undefined : "Enter a size like 10Gi.";
    case "enum":
      return p.options?.includes(value) ? undefined : `Choose one of ${p.options?.join(", ")}.`;
    case "apps":
      return value.split(/[\s,]+/).filter(Boolean).every((a) => /^([a-z0-9]([-a-z0-9]*[a-z0-9])?\/)?[a-z]([-a-z0-9]*[a-z0-9])?$/.test(a))
        ? undefined : "Apps by name (api) or project/app, separated by commas.";
  }
  return undefined;
}

/** "App/web" → the app's name, for links to what was created. */
export function createdApps(created: string[]) {
  return created.filter((o) => o.startsWith("App/")).map((o) => o.slice(4));
}
