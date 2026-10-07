// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Backups (docs/phase6.md): Settings › Backups (the bucket, its write-only
// access keys, the recovery key shown once, etcd snapshots) and the Backups
// page (plans, "Back up now", Velero's backups, restores). Owners and
// admins only; the server enforces it.

import { request } from "./api";
import { ago } from "./workloads";

export type TargetState = "NotConfigured" | "Pending" | "Ready" | "Error";

export type EtcdSnapshots = { enabled: boolean; schedule?: string; retention?: number };

/** One etcd node's uploads of k3s's snapshots, as its node agent reports them. */
export type EtcdUpload = {
  node: string;
  /** The newest snapshot in the bucket. */
  name?: string;
  uploadedAt?: string;
  checkedAt?: string;
  /** How many of the node's snapshots the bucket holds. */
  stored: number;
  /** What went wrong in the agent's last pass. */
  message?: string;
};

export type BackupTarget = {
  configured: boolean;
  endpoint?: string;
  region?: string;
  bucket?: string;
  prefix?: string;
  /** What an empty prefix becomes: the console's hostname. */
  defaultPrefix: string;
  credentialsSet: boolean;
  recoveryKeySet: boolean;
  recoveryKeyCreatedAt?: string;
  etcdSnapshots: EtcdSnapshots;
  state: TargetState;
  message?: string;
  checkedAt?: string;
  lastSuccessfulAt?: string;
  /** Per etcd node; empty until the agents report (or while etcd snapshots stay local). */
  etcdUploads?: EtcdUpload[];
};

export type TargetInput = {
  endpoint: string;
  region?: string;
  bucket: string;
  prefix?: string;
  /** Write-only; empty keeps the stored keys. */
  accessKey?: string;
  secretKey?: string;
  /** The key of earlier backups, for a console restored by hand. */
  recoveryKey?: string;
  etcdSnapshots?: EtcdSnapshots;
};

/** hasBackups: Velero backups under the prefix already (another console's, or this one's before a reinstall). */
export type BucketCheck = { ok: boolean; hasObjects: boolean; hasBackups: boolean };

/** recoveryKey: only in the answer to the save that created it — shown once. */
export type SaveAnswer = { settings: BackupTarget; recoveryKey?: string; planCreated: boolean; check?: BucketCheck };

export type BackupPhase = "New" | "Queued" | "ReadyToStart" | "InProgress" | "WaitingForPluginOperations" | "Finalizing"
  | "Completed" | "PartiallyFailed" | "Failed" | "FailedValidation" | "Deleting" | string;

export type BackupRun = {
  name: string;
  phase: BackupPhase;
  startedAt?: string;
  completedAt?: string;
  items: number;
  warnings: number;
  errors: number;
  message?: string;
};

export type BackupScope = "Cluster" | "Projects";

export type BackupPlan = {
  name: string;
  scope: BackupScope;
  projects: string[];
  schedule: string;
  /** "14d", "72h". */
  retention: string;
  volumes: boolean;
  paused: boolean;
  nextRunAt?: string;
  lastSuccessfulAt?: string;
  lastBackup?: BackupRun;
  backups: number;
  ready: boolean;
  reason?: string;
  message?: string;
  runRequestedAt?: string;
};

export type PlanInput = {
  name?: string;
  scope: BackupScope;
  projects?: string[];
  schedule: string;
  retention?: string;
  volumes: boolean;
  paused: boolean;
};

export type Backup = {
  name: string;
  plan?: string;
  scope?: BackupScope;
  phase: BackupPhase;
  /** Project namespaces it holds (Kwerft's own left out). */
  projects: string[];
  volumes: boolean;
  startedAt?: string;
  completedAt?: string;
  expiresAt?: string;
  items: number;
  totalItems: number;
  warnings: number;
  errors: number;
  /** Volume data in bytes; 0 when unknown. */
  bytes: number;
  message?: string;
  requestedBy?: string;
  restorable: boolean;
};

export type BackupList = { velero: boolean; backups: Backup[] };

export type RestorePhase = "Pending" | "InProgress" | "Completed" | "PartiallyFailed" | "Failed";

export type Restore = {
  name: string;
  backup: string;
  project: string;
  targetProject?: string;
  apps: string[];
  phase: RestorePhase;
  message?: string;
  warnings: number;
  errors: number;
  createdAt: string;
  startedAt?: string;
  completedAt?: string;
  requestedBy?: string;
};

export type RestoreInput = { backup: string; project: string; targetProject?: string; apps?: string[] };

const plan = (name: string) => `/backups/plans/${encodeURIComponent(name)}`;

export const backupKeys = {
  target: ["backups", "target"] as const,
  plans: ["backups", "plans"] as const,
  list: ["backups", "list"] as const,
  restores: ["backups", "restores"] as const,
};

export const backupsApi = {
  target: () => request<BackupTarget>("/settings/backups"),
  saveTarget: (t: TargetInput) => request<SaveAnswer>("/settings/backups", { method: "PUT", json: t }),
  /** Without keys: the saved target with the stored keys. */
  check: (t: Partial<TargetInput>) => request<BucketCheck>("/settings/backups/check", { method: "POST", json: t }),

  plans: () => request<BackupPlan[]>("/backups/plans"),
  createPlan: (p: PlanInput & { name: string }) => request<BackupPlan>("/backups/plans", { method: "POST", json: p }),
  updatePlan: (name: string, p: PlanInput) => request<BackupPlan>(plan(name), { method: "PUT", json: p }),
  deletePlan: (name: string) => request<void>(plan(name), { method: "DELETE" }),
  run: (name: string) => request<{ plan: BackupPlan; backup: string }>(`${plan(name)}/run`, { method: "POST" }),

  backups: () => request<BackupList>("/backups"),
  restores: () => request<Restore[]>("/backups/restores"),
  restore: (r: RestoreInput) => request<Restore>("/backups/restores", { method: "POST", json: r }),
};

// ---- presentation ------------------------------------------------------------------

type Pill = { pill: "ok" | "warn" | "bad" | "off"; label: string };

/** The target's state as a pill. */
export function targetPill(t: Pick<BackupTarget, "state" | "configured">): Pill {
  switch (t.state) {
    case "Ready": return { pill: "ok", label: "Ready" };
    case "Pending": return { pill: "warn", label: "Checking the bucket" };
    case "Error": return { pill: "bad", label: "Not working" };
  }
  return { pill: "off", label: t.configured ? "Incomplete" : "Not set up" };
}

/** One node's etcd snapshot uploads as a pill. */
export function etcdUploadPill(u: EtcdUpload): Pill {
  if (u.message) return { pill: "bad", label: "Failing" };
  if (!u.name) return { pill: "off", label: "None yet" };
  return { pill: "ok", label: "Uploaded" };
}

/** What a node's uploads amount to, in one line. */
export function describeEtcdUpload(u: EtcdUpload, now = Date.now()): string {
  if (u.message) return u.message;
  if (!u.name) return "No snapshot in the bucket yet: the agent uploads each one k3s takes.";
  const kept = `${u.stored} snapshot${u.stored === 1 ? "" : "s"} in the bucket`;
  return u.uploadedAt ? `${u.name}, uploaded ${ago(u.uploadedAt, now)} · ${kept}` : `${u.name} · ${kept}`;
}

/** The command that fetches a node's newest snapshot back from the bucket, decrypted. */
export function etcdFetchCommand(node?: string): string {
  return `kwerft etcd-snapshot fetch --config kwerft.yaml${node ? ` --node ${node}` : ""} --name latest`;
}

/** A Velero backup's phase as a pill. */
export function backupPill(phase: BackupPhase): Pill {
  switch (phase) {
    case "Completed": return { pill: "ok", label: "Completed" };
    case "PartiallyFailed": case "FinalizingPartiallyFailed": case "WaitingForPluginOperationsPartiallyFailed":
      return { pill: "warn", label: "Partially failed" };
    case "Failed": return { pill: "bad", label: "Failed" };
    case "FailedValidation": return { pill: "bad", label: "Invalid" };
    case "Deleting": return { pill: "off", label: "Deleting" };
    case "New": case "Queued": case "ReadyToStart": return { pill: "off", label: "Waiting" };
  }
  return { pill: "warn", label: "Running" };
}

export function restorePill(phase: RestorePhase): Pill {
  switch (phase) {
    case "Completed": return { pill: "ok", label: "Restored" };
    case "PartiallyFailed": return { pill: "warn", label: "Partially restored" };
    case "Failed": return { pill: "bad", label: "Failed" };
    case "Pending": return { pill: "off", label: "Preparing" };
  }
  return { pill: "warn", label: "Restoring" };
}

/** Bytes for people: 512 B, 4.2 MiB, 1.3 GiB. */
export function bytes(n: number): string {
  if (!n || n < 0) return "—";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let i = 0;
  let v = n;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i++;
  }
  return i === 0 ? `${v} B` : `${v < 10 ? v.toFixed(1) : Math.round(v)} ${units[i]}`;
}

/** "14d" → "14 days", "72h" → "72 hours", "1d12h" → "1d12h". */
export function retentionLabel(r: string): string {
  const m = /^(\d+)([dh])$/.exec(r);
  if (!m) return r;
  const n = Number(m[1]);
  const unit = m[2] === "d" ? "day" : "hour";
  return `${n} ${unit}${n === 1 ? "" : "s"}`;
}

/** The recovery key as a file to keep, with what it is for. */
export function recoveryKeyFile(key: string, host: string, at: Date): string {
  return [
    "Kwerft recovery key",
    "",
    key,
    "",
    `Console: ${host}`,
    `Created: ${at.toISOString()}`,
    "",
    "Backups of this console cannot be read without this key: everything Kwerft",
    "and Velero write to the bucket is encrypted with it (volume data with Kopia,",
    "all other objects and the etcd snapshots with a key derived from it, SSE-C).",
    "Keep it somewhere safe outside this server (a password manager). To rebuild",
    "the console on a new server, give this file to install.sh --restore as",
    "backups.recoveryKeyFile; kwerft etcd-snapshot fetch reads it the same way.",
    "",
  ].join("\n");
}

export const recoveryKeyFileName = (host: string) => `kwerft-recovery-key-${host.replace(/[^a-z0-9.-]/gi, "_")}.txt`;

/** The key in groups of 4, as the server writes it (a pasted key may lack the dashes). */
export function groupKey(key: string): string[] {
  return key.replace(/[\s-]/g, "").toUpperCase().match(/.{1,4}/g) ?? [];
}

/** A plain check of a typed recovery key before the server's: 52 base32 characters. */
export function recoveryKeyProblem(key: string): string | undefined {
  const raw = key.replace(/[\s-]/g, "").toUpperCase();
  if (!raw) return undefined;
  if (!/^[A-Z2-7]+$/.test(raw)) return "A recovery key has only the letters A–Z and the digits 2–7.";
  if (raw.length !== 52) return `A recovery key has 52 letters and digits; this has ${raw.length}.`;
  return undefined;
}

/** What a restore will do, in one sentence. */
export function describeRestore(r: RestoreInput): string {
  const what = r.apps?.length ? `${r.apps.length === 1 ? "the app" : "the apps"} ${r.apps.join(", ")} of ${r.project}` : `the project ${r.project}`;
  return r.targetProject && r.targetProject !== r.project
    ? `Restores ${what} into the new project ${r.targetProject}, next to the original.`
    : `Restores ${what} in place: only what no longer exists comes back; nothing that exists is overwritten.`;
}

/** The name a restore under a new name suggests. */
export function suggestedTarget(project: string, existing: string[]): string {
  const base = `${project}-restored`.slice(0, 40).replace(/-+$/, "");
  if (!existing.includes(base)) return base;
  for (let i = 2; i < 100; i++) {
    const name = `${base.slice(0, 37)}-${i}`;
    if (!existing.includes(name)) return name;
  }
  return base;
}
