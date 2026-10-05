import { useMemo, useState } from "react";
import { Link } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, ApiError } from "../api";
import { Dialog } from "../components/Dialog";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { ago, PROJECT_RE, workloads } from "../workloads";
import { describeCron, duration, when } from "../jobs";
import {
  backupKeys, backupPill, backupsApi, bytes, describeRestore, restorePill, retentionLabel, suggestedTarget, targetPill,
  type Backup, type BackupPlan, type BackupScope, type PlanInput, type Restore,
} from "../backups";
import { errorText } from "./Apps";
import "../styles/workloads.css";
import "../styles/backups.css";

const POLL = 10000;

// Backups: the plans (Velero schedules) with their last and next run and
// "Back up now", the backups Velero holds, and restores of a project or of
// some of its apps. Owners and admins; the target is set in Settings.
export function Backups() {
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const allowed = session.data?.role === "owner" || session.data?.role === "admin";
  const target = useQuery({ queryKey: backupKeys.target, queryFn: backupsApi.target, enabled: allowed, refetchInterval: POLL * 3 });
  const plans = useQuery({ queryKey: backupKeys.plans, queryFn: backupsApi.plans, enabled: allowed, refetchInterval: POLL });
  const list = useQuery({ queryKey: backupKeys.list, queryFn: backupsApi.backups, enabled: allowed, refetchInterval: POLL });
  const restores = useQuery({ queryKey: backupKeys.restores, queryFn: backupsApi.restores, enabled: allowed, refetchInterval: POLL });
  const [editing, setEditing] = useState<BackupPlan | "new">();
  const [restoring, setRestoring] = useState<Backup>();
  const [showAll, setShowAll] = useState(false);

  if (session.data && !allowed) {
    return (
      <section className="view">
        <div className="ph"><div><h1>Backups</h1></div></div>
        <div className="banner info"><Icon name="shield" /><span>Only owners and admins manage backups and restores.</span></div>
      </section>
    );
  }
  const t = target.data;
  const pill = t ? targetPill(t) : undefined;
  const backups = list.data?.backups ?? [];
  const shown = showAll ? backups : backups.slice(0, 20);

  return (
    <section className="view backups">
      <div className="ph">
        <div>
          <h1>Backups</h1>
          <p>Projects, their volumes and Kwerft&apos;s own state, to {t?.bucket ? <code>{t.bucket}/{t.prefix}</code> : "Object Storage"} · times in your time zone, schedules in UTC</p>
        </div>
        <div className="acts">
          <button className="btn" disabled={!t?.configured} onClick={() => setEditing("new")}><Icon name="plus" />New plan</button>
        </div>
      </div>

      {(plans.isError || list.isError || restores.isError) && (
        <div className="banner bad" role="alert"><Icon name="alert" /><span>{errorText(plans.error ?? list.error ?? restores.error)}</span></div>
      )}
      {t && !t.configured && (
        <div className="banner info" role="status"><Icon name="disk" />
          <span>No backup target yet. Set the bucket in <Link to="/settings" hash="backups">Settings › Backups</Link>; saving it starts a daily backup of everything.</span>
        </div>
      )}
      {t?.configured && t.state !== "Ready" && pill && (
        <div className={`banner ${t.state === "Error" ? "bad" : "warn"}`} role="status"><Icon name="alert" />
          <span><b>{pill.label}.</b> {t.message} <Link to="/settings" hash="backups">Settings › Backups</Link></span>
        </div>
      )}
      {list.data && !list.data.velero && (
        <div className="banner warn" role="status"><Icon name="alert" /><span>Velero is not installed on this cluster: re-run the installer (stage Backups; <code>--lite</code> leaves it out).</span></div>
      )}

      <div className="card">
        <div className="ch-h"><h3>Plans</h3></div>
        {plans.isPending ? (
          <p className="loading pad">Loading plans…</p>
        ) : !plans.data?.length ? (
          <div className="li empty-li">No plans. {t?.configured ? "A plan backs up the whole cluster or some projects on a schedule." : "Saving the backup target creates the plan “cluster”."}</div>
        ) : (
          <div className="scroll-x">
            <table className="t">
              <thead><tr><th>Plan</th><th>Schedule (UTC)</th><th>Keeps</th><th>Last backup</th><th>Next run</th><th><span className="sr">Actions</span></th></tr></thead>
              <tbody>{plans.data.map((p) => <PlanRow key={p.name} p={p} onEdit={() => setEditing(p)} />)}</tbody>
            </table>
          </div>
        )}
      </div>

      <div className="card">
        <div className="ch-h"><h3>Backups</h3>{backups.length > 0 && <span className="dim small">{backups.length} in the bucket</span>}</div>
        {list.isPending ? (
          <p className="loading pad">Loading backups…</p>
        ) : backups.length === 0 ? (
          <div className="li empty-li">No backups yet. They appear here when a plan runs; “Back up now” starts one at once.</div>
        ) : (
          <>
            <div className="scroll-x">
              <table className="t">
                <thead><tr><th>Backup</th><th>Started</th><th className="num">Duration</th><th>Projects</th><th className="num">Objects</th><th className="num">Volume data</th><th>Status</th><th>Expires</th><th><span className="sr">Actions</span></th></tr></thead>
                <tbody>{shown.map((b) => <BackupRow key={b.name} b={b} onRestore={() => setRestoring(b)} />)}</tbody>
              </table>
            </div>
            {backups.length > shown.length && <div className="li"><button className="btn sm" onClick={() => setShowAll(true)}>Show all {backups.length} backups</button></div>}
          </>
        )}
      </div>

      <div className="card">
        <div className="ch-h"><h3>Restores</h3></div>
        {restores.isPending ? (
          <p className="loading pad">Loading restores…</p>
        ) : !restores.data?.length ? (
          <div className="li empty-li">No restores. Restore a project, or some of its apps, from a completed backup above. The whole cluster comes back on a new server with <code>install.sh --restore</code>.</div>
        ) : (
          <div className="scroll-x">
            <table className="t">
              <thead><tr><th>Restore</th><th>From</th><th>Requested</th><th>Status</th></tr></thead>
              <tbody>{restores.data.map((r) => <RestoreRow key={r.name} r={r} />)}</tbody>
            </table>
          </div>
        )}
      </div>

      {editing && <PlanDialog plan={editing === "new" ? undefined : editing} onClose={() => setEditing(undefined)} />}
      {restoring && <RestoreDialog backup={restoring} onClose={() => setRestoring(undefined)} />}
    </section>
  );
}

function PlanRow({ p, onEdit }: { p: BackupPlan; onEdit: () => void }) {
  const queryClient = useQueryClient();
  const run = useMutation({
    mutationFn: () => backupsApi.run(p.name),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: backupKeys.plans });
      void queryClient.invalidateQueries({ queryKey: backupKeys.list });
    },
  });
  const last = p.lastBackup;
  const lastPill = last ? backupPill(last.phase) : undefined;
  const desc = describeCron(p.schedule);
  return (
    <tr>
      <td>
        <span className="nm">{p.name}</span>
        <span className="sub">
          {p.scope === "Cluster" ? "everything" : p.projects.join(", ")}{p.volumes ? " · with volumes" : " · objects only"}
          {!p.ready && p.message && <span className="plan-problem" title={p.message}> · {p.message}</span>}
        </span>
      </td>
      <td><span className="mono nowrap">{p.schedule}</span><span className="sub">{desc || "custom"}</span></td>
      <td className="nowrap">{retentionLabel(p.retention)}<span className="sub">{p.backups} kept now</span></td>
      <td>
        {last && lastPill ? (
          <>
            <span className={`pill ${lastPill.pill}`} title={last.message}>{lastPill.label}</span>
            <span className="sub">{ago(last.completedAt ?? last.startedAt)}{last.errors ? ` · ${last.errors} errors` : ""}</span>
          </>
        ) : <span className="dim">never</span>}
        {p.lastSuccessfulAt && last?.phase !== "Completed" && <span className="sub">last success {ago(p.lastSuccessfulAt)}</span>}
      </td>
      <td className={p.paused ? "dim nowrap" : "nowrap"} title={p.nextRunAt ? new Date(p.nextRunAt).toLocaleString() : undefined}>{p.paused ? "paused" : when(p.nextRunAt)}</td>
      <td className="nowrap"><div className="row-acts">
        <button className="btn sm" disabled={run.isPending} onClick={() => run.mutate()} title={run.isError ? errorText(run.error) : "Start a backup of this plan now"}>
          <Icon name="play" />{run.isPending ? "Starting…" : run.isSuccess ? "Started" : "Back up now"}
        </button>
        <button className="btn sm" onClick={onEdit}>Edit</button>
      </div></td>
    </tr>
  );
}

function BackupRow({ b, onRestore }: { b: Backup; onRestore: () => void }) {
  const pill = backupPill(b.phase);
  return (
    <tr>
      <td>
        <span className="nm mono">{b.name}</span>
        <span className="sub">{b.plan ? `plan ${b.plan}` : "not Kwerft's"}{b.requestedBy ? ` · by ${b.requestedBy}` : ""}</span>
      </td>
      <td className="dim nowrap" title={b.startedAt ? new Date(b.startedAt).toLocaleString() : undefined}>{ago(b.startedAt)}</td>
      <td className="num">{b.startedAt ? duration(b.startedAt, b.completedAt) : "—"}</td>
      <td className="ell" title={b.projects.join(", ")}>{b.scope === "Cluster" ? <>everything <span className="dim">({b.projects.length} projects)</span></> : b.projects.join(", ") || "—"}</td>
      <td className="num">{b.totalItems ? b.items : "—"}</td>
      <td className="num">{b.volumes ? bytes(b.bytes) : <span className="dim">none</span>}</td>
      <td><span className={`pill ${pill.pill}`} title={b.message}>{pill.label}</span>{(b.warnings > 0 || b.errors > 0) && <span className="sub">{b.errors} errors · {b.warnings} warnings</span>}</td>
      <td className="dim nowrap">{b.expiresAt ? when(b.expiresAt) : "—"}</td>
      <td><button className="btn sm" disabled={!b.restorable || b.projects.length === 0} onClick={onRestore}
        title={b.restorable ? "Restore a project or some of its apps" : "Only completed backups can be restored."}>Restore…</button></td>
    </tr>
  );
}

function RestoreRow({ r }: { r: Restore }) {
  const pill = restorePill(r.phase);
  const into = r.targetProject && r.targetProject !== r.project ? ` → ${r.targetProject}` : "";
  return (
    <tr>
      <td>
        <span className="nm">{r.project}{into}</span>
        <span className="sub">{r.apps.length ? `apps ${r.apps.join(", ")}` : "whole project"}</span>
      </td>
      <td className="mono small">{r.backup}</td>
      <td className="nowrap"><span className="dim">{ago(r.createdAt)}</span>{r.requestedBy && <span className="sub">{r.requestedBy}</span>}</td>
      <td>
        <span className={`pill ${pill.pill}`}>{pill.label}</span>
        {(r.message || r.errors > 0) && <span className="sub wrap">{r.message}{r.errors > 0 ? ` ${r.errors} errors, ${r.warnings} warnings.` : ""}</span>}
      </td>
    </tr>
  );
}

// ---- dialogs ---------------------------------------------------------------------

function PlanDialog({ plan, onClose }: { plan?: BackupPlan; onClose: () => void }) {
  const queryClient = useQueryClient();
  const projects = useQuery({ queryKey: ["projects"], queryFn: workloads.projects });
  const [name, setName] = useState(plan?.name ?? "");
  const [scope, setScope] = useState<BackupScope>(plan?.scope ?? "Projects");
  const [picked, setPicked] = useState<string[]>(plan?.projects ?? []);
  const [schedule, setSchedule] = useState(plan?.schedule ?? "0 2 * * *");
  const [retention, setRetention] = useState(plan?.retention ?? "14d");
  const [volumes, setVolumes] = useState(plan?.volumes ?? true);
  const [paused, setPaused] = useState(plan?.paused ?? false);
  const [error, setError] = useState<{ field?: string; message: string }>();
  const [confirmDelete, setConfirmDelete] = useState(false);
  const fieldError = (f: string) => (error?.field === f ? error.message : undefined);
  const desc = describeCron(schedule);

  const save = useMutation({
    mutationFn: () => {
      const input: PlanInput = { scope, projects: scope === "Projects" ? picked : undefined, schedule, retention: retention.trim() || undefined, volumes, paused };
      return plan ? backupsApi.updatePlan(plan.name, input) : backupsApi.createPlan({ ...input, name: name.trim() });
    },
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: backupKeys.plans });
      onClose();
    },
    onError: (err) => setError(err instanceof ApiError ? { field: err.field, message: err.message } : { message: errorText(err) }),
  });
  const remove = useMutation({
    mutationFn: () => backupsApi.deletePlan(plan!.name),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: backupKeys.plans });
      onClose();
    },
    onError: (err) => setError({ message: errorText(err) }),
  });
  const toggle = (p: string) => setPicked((cur) => (cur.includes(p) ? cur.filter((x) => x !== p) : [...cur, p].sort()));

  return (
    <Dialog title={plan ? `Plan ${plan.name}` : "New backup plan"} onClose={onClose} wide onSubmit={() => { setError(undefined); save.mutate(); }}
      actions={<>
        {plan && (confirmDelete
          ? <button type="button" className="btn danger" disabled={remove.isPending} onClick={() => remove.mutate()}>{remove.isPending ? "Deleting…" : "Delete the plan; keep its backups"}</button>
          : <button type="button" className="btn start" onClick={() => setConfirmDelete(true)}><Icon name="trash" />Delete…</button>)}
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={save.isPending || (scope === "Projects" && picked.length === 0)}>{save.isPending ? "Saving…" : plan ? "Save" : "Create plan"}</button>
      </>}>
      <div className="stack">
        {!plan && (
          <Field id="plan-name" label="Name" className="mono" value={name} onChange={(e) => setName(e.target.value.toLowerCase())} placeholder="shop-hourly"
            maxLength={40} autoComplete="off" spellCheck={false} error={fieldError("name") ?? (name && !PROJECT_RE.test(name) ? "Lowercase letters, digits and dashes." : undefined)} />
        )}
        <div className="field">
          <span className="label">What it backs up</span>
          <div className="seg" role="group" aria-label="Scope">
            <button type="button" aria-pressed={scope === "Cluster"} onClick={() => setScope("Cluster")}>Everything</button>
            <button type="button" aria-pressed={scope === "Projects"} onClick={() => setScope("Projects")}>Some projects</button>
          </div>
          <span className="hint">
            {scope === "Cluster"
              ? "Every project, the console's database, settings and certificates, and built images: what install.sh --restore needs on a new server."
              : "The projects' apps, jobs, volumes and secrets. Restores of one project or app come from either kind."}
          </span>
        </div>
        {scope === "Projects" && (
          <fieldset className="project-picks">
            <legend>Projects</legend>
            {projects.data?.map((p) => (
              <label key={p.name} className="check"><input type="checkbox" checked={picked.includes(p.name)} onChange={() => toggle(p.name)} /><span>{p.name}</span></label>
            ))}
            {projects.data?.length === 0 && <span className="dim">No projects yet.</span>}
            {fieldError("projects") && <span className="field-error" role="alert">{fieldError("projects")}</span>}
          </fieldset>
        )}
        <div className="fields">
          <Field id="plan-schedule" label="Schedule (cron, UTC)" className="mono" value={schedule} onChange={(e) => setSchedule(e.target.value)}
            error={fieldError("schedule")} hint={desc ? `Runs ${desc} UTC.` : "Five fields: minute hour day month weekday."} />
          <Field id="plan-retention" label="Keep each backup" className="mono" value={retention} onChange={(e) => setRetention(e.target.value)}
            error={fieldError("retention")} hint="Like 14d or 72h; Velero deletes older backups." />
        </div>
        <label className="check"><input type="checkbox" checked={volumes} onChange={(e) => setVolumes(e.target.checked)} />
          <span><b>Back up volume data</b><small>File by file, encrypted with the recovery key. Off: only the objects (apps, settings, secrets).</small></span></label>
        <label className="check"><input type="checkbox" checked={paused} onChange={(e) => setPaused(e.target.checked)} />
          <span><b>Paused</b><small>No scheduled runs; “Back up now” still works.</small></span></label>
        {error && !error.field && <p className="form-error" role="alert">{error.message}</p>}
      </div>
    </Dialog>
  );
}

function RestoreDialog({ backup, onClose }: { backup: Backup; onClose: () => void }) {
  const queryClient = useQueryClient();
  const projects = useQuery({ queryKey: ["projects"], queryFn: workloads.projects });
  const [project, setProject] = useState(backup.projects[0] ?? "");
  const exists = !!projects.data?.some((p) => p.name === project);
  const apps = useQuery({ queryKey: ["apps", project], queryFn: () => workloads.apps(project), enabled: exists });
  const [what, setWhat] = useState<"project" | "apps">("project");
  const [picked, setPicked] = useState<string[]>([]);
  const [others, setOthers] = useState("");
  const [into, setInto] = useState<"same" | "new">("new");
  const names = useMemo(() => projects.data?.map((p) => p.name) ?? [], [projects.data]);
  const [target, setTarget] = useState("");
  const targetName = target.trim() || suggestedTarget(project, names);
  const [error, setError] = useState<{ field?: string; message: string }>();
  const fieldError = (f: string) => (error?.field === f ? error.message : undefined);
  const chosenApps = what === "apps" ? [...new Set([...picked, ...others.split(/[\s,]+/).map((s) => s.trim()).filter(Boolean)])].sort() : [];
  const input = { backup: backup.name, project, targetProject: into === "new" ? targetName : undefined, apps: chosenApps.length ? chosenApps : undefined };

  const restore = useMutation({
    mutationFn: () => backupsApi.restore(input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: backupKeys.restores });
      onClose();
    },
    onError: (err) => setError(err instanceof ApiError ? { field: err.field, message: err.message } : { message: errorText(err) }),
  });
  const toggle = (a: string) => setPicked((cur) => (cur.includes(a) ? cur.filter((x) => x !== a) : [...cur, a]));
  const invalidTarget = into === "new" && (!PROJECT_RE.test(targetName) || names.includes(targetName));

  return (
    <Dialog title="Restore from a backup" onClose={onClose} wide onSubmit={() => { setError(undefined); restore.mutate(); }}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={restore.isPending || !project || invalidTarget || (what === "apps" && chosenApps.length === 0)}>
          {restore.isPending ? "Starting…" : "Restore"}
        </button>
      </>}>
      <div className="stack">
        <p className="dim">From <code>{backup.name}</code>, made {ago(backup.startedAt)}{backup.volumes ? " with volume data" : " without volume data"}.</p>
        <div className="field">
          <label htmlFor="rs-project">Project</label>
          <select id="rs-project" className="input" value={project} onChange={(e) => { setProject(e.target.value); setPicked([]); }}>
            {backup.projects.map((p) => <option key={p} value={p}>{p}</option>)}
          </select>
          {fieldError("project") && <span className="field-error" role="alert">{fieldError("project")}</span>}
        </div>
        <div className="field">
          <span className="label">What</span>
          <div className="seg" role="group" aria-label="What to restore">
            <button type="button" aria-pressed={what === "project"} onClick={() => setWhat("project")}>The whole project</button>
            <button type="button" aria-pressed={what === "apps"} onClick={() => setWhat("apps")}>Some apps</button>
          </div>
        </div>
        {what === "apps" && (
          <fieldset className="project-picks">
            <legend>Apps, with their volumes, secrets and domains</legend>
            {apps.data?.map((a) => (
              <label key={a.name} className="check"><input type="checkbox" checked={picked.includes(a.name)} onChange={() => toggle(a.name)} /><span>{a.name}</span></label>
            ))}
            <Field id="rs-apps" label={apps.data?.length ? "Apps that no longer exist" : "App names"} className="mono" value={others} onChange={(e) => setOthers(e.target.value)}
              placeholder="web, worker" autoComplete="off" spellCheck={false} error={fieldError("apps")} />
          </fieldset>
        )}
        <div className="field">
          <span className="label">Where</span>
          <div className="seg" role="group" aria-label="Where to restore">
            <button type="button" aria-pressed={into === "new"} onClick={() => setInto("new")}>Into a new project</button>
            <button type="button" aria-pressed={into === "same"} onClick={() => setInto("same")}>Into {project || "the project"}</button>
          </div>
        </div>
        {into === "new" && (
          <Field id="rs-target" label="New project's name" className="mono" value={target} onChange={(e) => setTarget(e.target.value.toLowerCase())}
            placeholder={suggestedTarget(project, names)} maxLength={40} autoComplete="off" spellCheck={false}
            error={fieldError("targetProject") ?? (target && invalidTarget ? (names.includes(targetName) ? "That project exists; the restore creates a new one." : "Lowercase letters, digits and dashes.") : undefined)}
            hint="Created with the original's settings. Public hostnames stay with the original project; give the copy its own." />
        )}
        <p className="restore-summary">{describeRestore(input)}</p>
        {error && !error.field && <p className="form-error" role="alert">{error.message}</p>}
      </div>
    </Dialog>
  );
}
