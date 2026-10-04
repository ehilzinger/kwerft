import { Link } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { jobs } from "../jobs";
import { Icon } from "./Icon";

export type Mount = { path: string; volume: string; readOnly: boolean };

/** The first problem with a set of mounts, as [index, message]. */
export function checkMounts(mounts: Mount[]): [number, string] | undefined {
  const seen = new Set<string>();
  for (const [i, m] of mounts.entries()) {
    if (!m.path.startsWith("/")) return [i, "A mount path is absolute, like /data."];
    if (!m.volume) return [i, "Choose a volume."];
    if (seen.has(m.path)) return [i, `${m.path} is mounted twice.`];
    seen.add(m.path);
  }
  return undefined;
}

// Rows that mount shared Volumes of a project: path, volume, read-only.
// Volumes are disks Apps and Tasks of the same project share; every pod that
// mounts one runs on the same node.
export function VolumeMounts({ project, mounts, onChange, errorAt, idPrefix }: {
  project: string;
  mounts: Mount[];
  onChange: (m: Mount[]) => void;
  errorAt?: [number, string];
  idPrefix: string;
}) {
  const volumes = useQuery({ queryKey: ["volumes", project], queryFn: () => jobs.volumes(project), enabled: !!project });
  const available = (volumes.data ?? []).filter((v) => v.phase !== "deleting");
  const set = (i: number, patch: Partial<Mount>) => onChange(mounts.map((m, j) => (j === i ? { ...m, ...patch } : m)));
  return (
    <div className="mounts">
      {mounts.map((m, i) => (
        <div className="mount" key={i}>
          <input className="input mono" aria-label={`Mount ${i + 1} path`} placeholder="/data" value={m.path} spellCheck={false} autoComplete="off"
            aria-invalid={errorAt?.[0] === i} onChange={(e) => set(i, { path: e.target.value })} id={`${idPrefix}-path-${i}`} />
          <select className="input" aria-label={`Mount ${i + 1} volume`} value={m.volume} onChange={(e) => set(i, { volume: e.target.value })}>
            <option value="">Choose a volume…</option>
            {available.map((v) => <option key={v.name} value={v.name}>{v.name} · {v.size}</option>)}
            {m.volume && !available.some((v) => v.name === m.volume) && <option value={m.volume}>{m.volume} (not found)</option>}
          </select>
          <label className="check"><input type="checkbox" checked={m.readOnly} onChange={(e) => set(i, { readOnly: e.target.checked })} />Read-only</label>
          <button type="button" className="btn ghost sm danger" onClick={() => onChange(mounts.filter((_, j) => j !== i))} aria-label={`Remove mount ${m.path || i + 1}`}>Remove</button>
          {errorAt?.[0] === i && <span className="field-error full" role="alert">{errorAt[1]}</span>}
        </div>
      ))}
      <div className="mount-foot">
        <button type="button" className="btn sm" disabled={!project} onClick={() => onChange([...mounts, { path: "", volume: available.length === 1 ? available[0]!.name : "", readOnly: false }])}>
          <Icon name="plus" />Mount a volume
        </button>
        {project && volumes.isSuccess && available.length === 0 && (
          <span className="hint">No volumes in {project} yet. <Link to="/apps/volumes" search={{ project }}>Create one</Link>.</span>
        )}
      </div>
    </div>
  );
}

export const mountsOf = (vols?: { path: string; volume?: string; readOnly?: boolean }[]): Mount[] =>
  (vols ?? []).filter((v) => v.volume).map((v) => ({ path: v.path, volume: v.volume!, readOnly: !!v.readOnly }));

export const volumesOf = (mounts: Mount[]) =>
  mounts.map((m) => ({ path: m.path.trim(), volume: m.volume, ...(m.readOnly ? { readOnly: true } : {}) }));
