// Mounts of an App or Task that the console edits: shared Volumes of the
// project and Secrets as files. Disks per replica (size) are kept as they are.

/** An AppVolume as Apps (workloads.ts) and Tasks (jobs.ts) carry it. */
type AppVolume = { path: string; size?: string; class?: string; volume?: string; readOnly?: boolean; secret?: string; mode?: number };

/** One row of the mounts editor. secret set means a Secret as files, else a shared Volume. */
export type Mount = { path: string; volume: string; readOnly: boolean; secret?: string; mode?: number };

/** File permission of a Secret's files when mode is not set (AppVolume.mode). */
export const DEFAULT_SECRET_MODE = 0o444;

/** The DNS-1123 subdomain a Secret's name is. */
const SECRET_RE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$/;

export const isSecretMount = (m: Mount) => m.secret !== undefined;

/** The first problem with a set of mounts, as [index, message]. */
export function checkMounts(mounts: Mount[]): [number, string] | undefined {
  const seen = new Set<string>();
  for (const [i, m] of mounts.entries()) {
    const path = m.path.trim();
    if (!path.startsWith("/")) return [i, "A mount path is absolute, like /data."];
    if (isSecretMount(m)) {
      const name = m.secret!.trim();
      if (!name) return [i, "Enter the name of a Secret in the project."];
      if (name.length > 253 || !SECRET_RE.test(name)) return [i, `"${name}" is not a valid Secret name. Use lowercase letters, digits, - and ., like ssh-key.`];
    } else if (!m.volume) return [i, "Choose a volume."];
    if (seen.has(path)) return [i, `${path} is mounted twice.`];
    seen.add(path);
  }
  return undefined;
}

/** The mounts the editor shows: shared Volumes and Secrets, in spec order. */
export const mountsOf = (vols?: AppVolume[]): Mount[] =>
  (vols ?? []).filter((v) => v.volume || v.secret).map((v) =>
    v.secret
      ? { path: v.path, volume: "", readOnly: true, secret: v.secret, ...(v.mode !== undefined ? { mode: v.mode } : {}) }
      : { path: v.path, volume: v.volume!, readOnly: !!v.readOnly });

export const volumesOf = (mounts: Mount[]) =>
  mounts.map((m) =>
    isSecretMount(m)
      ? { path: m.path.trim(), secret: m.secret!.trim(), ...(m.mode !== undefined ? { mode: m.mode } : {}) }
      : { path: m.path.trim(), volume: m.volume, ...(m.readOnly ? { readOnly: true } : {}) });

/** Disks per replica: the volumes neither a shared Volume nor a Secret. */
export const ownDisks = <V extends AppVolume>(vols?: V[]) => (vols ?? []).filter((v) => !v.volume && !v.secret);

/** A permission as the octal people know: 0444. */
export const octal = (mode: number) => "0" + mode.toString(8).padStart(3, "0");

/** One mount for a details list: "/keys ← secret ssh-key (0400)". */
export function describeMount(v: AppVolume): string {
  if (v.secret) return `${v.path} ← secret ${v.secret}${v.mode !== undefined ? ` (${octal(v.mode)})` : ""}`;
  if (v.volume) return `${v.path} ← volume ${v.volume}${v.readOnly ? " (read-only)" : ""}`;
  return `${v.path} (${v.size}${v.class ? `, ${v.class}` : ""})`;
}
