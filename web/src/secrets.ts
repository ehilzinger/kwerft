// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Secret sets (Phase 6): a project's write-only values. The server writes
// them as the signed-in user; nothing here ever receives a value except
// reveal (owners and admins, after their password).
import { request } from "./api";
import type { EnvVar } from "./workloads";

export type SecretSource = "Set" | "Generated" | "Derived";
export type SecretKey = { name: string; updatedAt?: string; updatedBy?: string; source?: SecretSource };
export type DerivedKey = { key: string; template: string };
export type SecretSet = {
  name: string;
  project: string;
  description?: string;
  /** The App this is the own set of (<app>-env), deleted with it. */
  app?: string;
  generate: string[];
  derived: DerivedKey[];
  keys: SecretKey[];
  /** "App/api", "Schedule/nightly", "Task/run-x". */
  usedBy: string[];
  /** "App/notifier: SMTP_USER": referenced, not set. */
  missing: string[];
  phase: "ready" | "pending" | "missing" | "failed";
  reason?: string;
  message?: string;
  created: string;
  resourceVersion: string;
};
export type NewSecretSet = { name?: string; description?: string; generate?: string[]; derived?: DerivedKey[]; app?: string };
export type SecretSetChange = { description?: string; generate?: string[]; derived?: DerivedKey[]; resourceVersion?: string };

const enc = encodeURIComponent;
const setsPath = (project: string, set?: string) => `/projects/${enc(project)}/secret-sets${set ? `/${enc(set)}` : ""}`;
const keyPath = (project: string, set: string, key: string) => `${setsPath(project, set)}/keys/${enc(key)}`;

export const secretsApi = {
  sets: (project: string) => request<SecretSet[]>(setsPath(project)),
  set: (project: string, name: string) => request<SecretSet>(setsPath(project, name)),
  createSet: (project: string, set: NewSecretSet) => request<SecretSet>(setsPath(project), { method: "POST", json: set }),
  updateSet: (project: string, name: string, change: SecretSetChange) => request<SecretSet>(setsPath(project, name), { method: "PATCH", json: change }),
  deleteSet: (project: string, name: string) => request<void>(setsPath(project, name), { method: "DELETE" }),
  /** Writes a value; the answer is the key's record, never the value. A missing <app>-env set of an existing App is created. */
  setKey: (project: string, set: string, key: string, value: string) => request<SecretKey>(keyPath(project, set, key), { method: "PUT", json: { value } }),
  generateKey: (project: string, set: string, key: string) => request<SecretKey>(keyPath(project, set, key), { method: "PUT", json: { generate: true } }),
  removeKey: (project: string, set: string, key: string) => request<void>(keyPath(project, set, key), { method: "DELETE" }),
  /** Owners and admins, with their password (or an authenticator code); audited. */
  reveal: (project: string, set: string, key: string, password: string) =>
    request<{ key: string; value: string }>(`${keyPath(project, set, key)}/reveal`, { method: "POST", json: { password } }),
  copySet: (project: string, set: string, to: string, name?: string) =>
    request<SecretSet>(`${setsPath(project, set)}/copy`, { method: "POST", json: { project: to, ...(name ? { name } : {}) } }),
};

export const secretKeys = {
  sets: (project: string) => ["secret-sets", project] as const,
};

export const KEY_RE = /^[-._a-zA-Z0-9]{1,253}$/;
export const SET_NAME_RE = /^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$/;

export const appEnvSet = (app: string) => `${app}-env`;

export function setNameProblem(name: string): string | undefined {
  if (!name) return "Enter a name.";
  if (name.length > 63 || !SET_NAME_RE.test(name)) return "Lowercase letters, digits, - and ., starting and ending with a letter or digit (at most 63).";
  return undefined;
}

export function keyProblem(key: string): string | undefined {
  if (!key) return "Enter a key, like STRIPE_KEY.";
  if (!KEY_RE.test(key)) return "Letters, digits, -, _ and . only.";
  return undefined;
}

/** The key written last, for "Updated … by …". */
export function lastUpdate(set: Pick<SecretSet, "keys">): SecretKey | undefined {
  let newest: SecretKey | undefined;
  for (const k of set.keys) {
    if (k.updatedAt && (!newest?.updatedAt || k.updatedAt > newest.updatedAt)) newest = k;
  }
  return newest;
}

/** Keys that something references but the set does not have. */
export function missingKeys(set: Pick<SecretSet, "missing">): string[] {
  const out: string[] = [];
  for (const m of set.missing) {
    const key = m.slice(m.indexOf(": ") + 2);
    if (key && !out.includes(key)) out.push(key);
  }
  return out;
}

/** "App/api" → kind and name. */
export function parseUser(u: string): { kind: string; name: string } {
  const i = u.indexOf("/");
  return i < 0 ? { kind: "", name: u } : { kind: u.slice(0, i), name: u.slice(i + 1) };
}

/** How a key came to be, for its hint. */
export function describeKey(k: SecretKey, set: Pick<SecretSet, "derived">): string {
  const d = set.derived.find((x) => x.key === k.name);
  if (d) return `Derived from ${d.template.match(/\$\{[^}]+\}/g)?.join(", ") ?? "other keys"}`;
  if (k.source === "Generated") return k.updatedBy ? `Generated by ${k.updatedBy}` : "Generated by Kwerft";
  if (k.updatedBy) return `Set by ${k.updatedBy}`;
  return "Set";
}

/** Lines "KEY=template" ↔ derived keys, for the edit dialog. */
export function derivedOf(text: string): { derived: DerivedKey[]; bad?: number } {
  const derived: DerivedKey[] = [];
  for (const [i, line] of text.split("\n").entries()) {
    if (!line.trim()) continue;
    const eq = line.indexOf("=");
    const key = line.slice(0, eq).trim();
    const template = line.slice(eq + 1).trim();
    if (eq < 1 || !KEY_RE.test(key) || !/\$\{[-._a-zA-Z0-9]+\}/.test(template)) return { derived, bad: i };
    derived.push({ key, template });
  }
  return { derived };
}
export const derivedText = (d: DerivedKey[]) => d.map((x) => `${x.key}=${x.template}`).join("\n");

/** "PASSWORD, ADMIN_PASSWORD" → keys; the index of the first bad one. */
export function keyList(text: string): { keys: string[]; bad?: string } {
  const keys = [...new Set(text.split(/[\s,]+/).map((k) => k.trim()).filter(Boolean))];
  return { keys, bad: keys.find((k) => !KEY_RE.test(k)) };
}

// ---- the App env editor -----------------------------------------------------------

/**
 * One row of an App's environment:
 *  - plain: a value in the App spec
 *  - own: a key of the App's own set <app>-env (key: the existing key; value:
 *    a new value to write, "" to keep the stored one)
 *  - ref: a key of a shared set of the project
 *  - other: any other reference (a ConfigMap, a field, a Secret Kwerft does not manage), kept as is
 */
export type EnvRow = {
  name: string;
  kind: "plain" | "own" | "ref" | "other";
  value: string;
  set?: string;
  key?: string;
  from?: EnvVar["valueFrom"];
};

export function envRowsOf(env: EnvVar[] | undefined, app: string): EnvRow[] {
  return (env ?? []).map((e): EnvRow => {
    const ref = e.valueFrom?.secretKeyRef;
    if (!e.valueFrom) return { name: e.name, kind: "plain", value: e.value ?? "" };
    if (ref && ref.name === appEnvSet(app)) return { name: e.name, kind: "own", value: "", key: ref.key };
    if (ref) return { name: e.name, kind: "ref", value: "", set: ref.name, key: ref.key };
    return { name: e.name, kind: "other", value: "", from: e.valueFrom };
  });
}

export type EnvPlan = {
  env: EnvVar[];
  /** Values to write into <app>-env before the App is saved. */
  put: { key: string; value: string }[];
  /** Keys of <app>-env nothing references any more, removed after saving. */
  remove: string[];
  error?: { index: number; message: string };
};

/** The App's env from the rows, and what to write to (and remove from) its own set. */
export function envPlan(rows: EnvRow[], app: string, before: EnvVar[] | undefined): EnvPlan {
  const own = appEnvSet(app);
  const plan: EnvPlan = { env: [], put: [], remove: [] };
  const refs = (name: string, key: string) => ({ secretKeyRef: { name, key } });
  for (const [index, r] of rows.entries()) {
    const name = r.name.trim();
    if (!name && !r.value && r.kind === "plain") continue;
    switch (r.kind) {
      case "plain":
        plan.env.push({ name, value: r.value });
        break;
      case "own": {
        const key = r.key ?? name;
        if (!r.key && !r.value) return { ...plan, error: { index, message: "Enter the value: it is stored write-only and nobody can read it back." } };
        if (!KEY_RE.test(key)) return { ...plan, error: { index, message: "A secret variable's name is its key: letters, digits, -, _ and . only." } };
        if (r.value) {
          if (plan.put.some((p) => p.key === key)) return { ...plan, error: { index, message: `${key} is set twice.` } };
          plan.put.push({ key, value: r.value });
        }
        plan.env.push({ name, valueFrom: refs(own, key) });
        break;
      }
      case "ref":
        if (!r.set || !r.key) return { ...plan, error: { index, message: "Choose a key of a secret set." } };
        plan.env.push({ name, valueFrom: refs(r.set, r.key) });
        break;
      case "other":
        plan.env.push({ name, valueFrom: r.from });
        break;
    }
  }
  const used = new Set(plan.env.flatMap((e) => (e.valueFrom?.secretKeyRef?.name === own ? [e.valueFrom.secretKeyRef.key] : [])));
  for (const e of before ?? []) {
    const ref = e.valueFrom?.secretKeyRef;
    if (ref?.name === own && !used.has(ref.key) && !plan.remove.includes(ref.key)) plan.remove.push(ref.key);
  }
  return plan;
}

/** The source select's value for a row: "plain", "own", "ref:<set>/<key>" or "other". */
export const sourceOf = (r: EnvRow) => (r.kind === "ref" ? `ref:${r.set}/${r.key}` : r.kind);

/** Applies a choice of the source select to a row. */
export function withSource(r: EnvRow, source: string): EnvRow {
  if (source === "plain") return { name: r.name, kind: "plain", value: r.kind === "own" ? r.value : r.kind === "plain" ? r.value : "" };
  if (source === "own") return { name: r.name, kind: "own", value: r.kind === "plain" ? r.value : r.kind === "own" ? r.value : "", key: r.kind === "own" ? r.key : undefined };
  if (source.startsWith("ref:")) {
    const [set, ...key] = source.slice(4).split("/");
    return { name: r.name || key.join("/"), kind: "ref", value: "", set, key: key.join("/") };
  }
  return r;
}
