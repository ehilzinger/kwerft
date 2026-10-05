import { Link } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { jobs } from "../jobs";
import { Icon } from "./Icon";
import { DEFAULT_SECRET_MODE, isSecretMount, octal, type Mount } from "../mounts";

export { checkMounts, mountsOf, volumesOf, type Mount } from "../mounts";

// Permissions offered for a Secret's files. Files belong to root, so a
// container running as another user needs them world-readable; ssh run as
// root refuses a key others can read, and needs 0400.
const secretModes: { mode: number; label: string }[] = [
  { mode: DEFAULT_SECRET_MODE, label: "Readable by all (0444)" },
  { mode: 0o400, label: "Owner only (0400)" },
];

// Rows that mount shared Volumes of a project (path, volume, read-only) or
// Secrets as files (path, Secret name, permission). Volumes are disks Apps
// and Tasks of the same project share; every pod that mounts one runs on the
// same node. A Secret is mounted read-only, one file per key.
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
          <input className="input mono" aria-label={`Mount ${i + 1} path`} placeholder={isSecretMount(m) ? "/etc/secret" : "/data"} value={m.path} spellCheck={false} autoComplete="off"
            aria-invalid={errorAt?.[0] === i} onChange={(e) => set(i, { path: e.target.value })} id={`${idPrefix}-path-${i}`} />
          {isSecretMount(m) ? (
            <>
              <input className="input mono" aria-label={`Mount ${i + 1} secret`} placeholder="Secret name" value={m.secret} spellCheck={false} autoComplete="off"
                onChange={(e) => set(i, { secret: e.target.value })} />
              <select className="input" aria-label={`Mount ${i + 1} file permission`} value={m.mode ?? DEFAULT_SECRET_MODE}
                onChange={(e) => set(i, { mode: Number(e.target.value) === DEFAULT_SECRET_MODE ? undefined : Number(e.target.value) })}>
                {secretModes.map((o) => <option key={o.mode} value={o.mode}>{o.label}</option>)}
                {m.mode !== undefined && !secretModes.some((o) => o.mode === m.mode) && <option value={m.mode}>{octal(m.mode)}</option>}
              </select>
            </>
          ) : (
            <>
              <select className="input" aria-label={`Mount ${i + 1} volume`} value={m.volume} onChange={(e) => set(i, { volume: e.target.value })}>
                <option value="">Choose a volume…</option>
                {available.map((v) => <option key={v.name} value={v.name}>{v.name} · {v.size}</option>)}
                {m.volume && !available.some((v) => v.name === m.volume) && <option value={m.volume}>{m.volume} (not found)</option>}
              </select>
              <label className="check"><input type="checkbox" checked={m.readOnly} onChange={(e) => set(i, { readOnly: e.target.checked })} />Read-only</label>
            </>
          )}
          <button type="button" className="btn ghost sm danger" onClick={() => onChange(mounts.filter((_, j) => j !== i))} aria-label={`Remove mount ${m.path || i + 1}`}>Remove</button>
          {errorAt?.[0] === i && <span className="field-error full" role="alert">{errorAt[1]}</span>}
        </div>
      ))}
      <div className="mount-foot">
        <button type="button" className="btn sm" disabled={!project} onClick={() => onChange([...mounts, { path: "", volume: available.length === 1 ? available[0]!.name : "", readOnly: false }])}>
          <Icon name="plus" />Mount a volume
        </button>
        <button type="button" className="btn sm" disabled={!project} onClick={() => onChange([...mounts, { path: "", volume: "", readOnly: true, secret: "" }])}>
          <Icon name="plus" />Mount a secret as files
        </button>
        {project && volumes.isSuccess && available.length === 0 && (
          <span className="hint">No volumes in {project} yet. <Link to="/apps/volumes" search={{ project }}>Create one</Link>.</span>
        )}
      </div>
    </div>
  );
}
