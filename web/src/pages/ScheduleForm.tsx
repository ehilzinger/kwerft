// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { useDeferredValue, useState, type FormEvent } from "react";
import { Link, getRouteApi, useNavigate } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../api";
import { CreateProjectDialog } from "../components/CreateProjectDialog";
import { Dialog } from "../components/Dialog";
import { EnvRows, envOf, type EnvRow } from "../components/EnvRows";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { VolumeMounts, checkMounts, mountsOf, volumesOf, type Mount } from "../components/VolumeMounts";
import { ownDisks } from "../mounts";
import { describeCron, jobs, prettyDuration, timeZones, when, type Concurrency, type Schedule, type ScheduleSpec, type TaskSpec } from "../jobs";
import { NAME_RE, abilities, sizes, toYAML, workloads, type Size } from "../workloads";
import { CommandSyntaxError, formatCommand, splitCommand } from "../shellwords";
import { errorText } from "./Apps";
import { RunsTable } from "./Jobs";
import { DURATION_RE, RunNowDialog } from "./RunNowDialog";
import "../styles/workloads.css";
import "../styles/jobs.css";

const newRoute = getRouteApi("/authed/jobs/new");
const editRoute = getRouteApi("/authed/jobs/$project/schedules/$name");

type Preset = "hourly" | "daily" | "weekly" | "custom";
type Egress = "" | "none" | "https" | "all";

type Form = {
  name: string;
  project: string;
  source: "app" | "image";
  app: string;
  image: string;
  pullSecret: string;
  command: string; // one line, split like a shell would (see shellwords.ts)
  env: EnvRow[];
  preset: Preset;
  minute: string;
  time: string; // HH:MM
  weekday: string; // 0-6, Sunday = 0
  cron: string;
  timeZone: string;
  concurrency: Concurrency;
  keepSucceeded: string;
  keepFailed: string;
  timeout: string;
  retries: string;
  stopSeconds: string;
  restart: string[];
  mounts: Mount[];
  size: "" | Size;
  egress: Egress;
};

const weekdays = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];
const pad = (n: number) => String(n).padStart(2, "0");

function cronOf(f: Form): string {
  const [hh = "0", mm = "0"] = f.time.split(":");
  switch (f.preset) {
    case "hourly": return `${Number(f.minute) || 0} * * * *`;
    case "daily": return `${Number(mm)} ${Number(hh)} * * *`;
    case "weekly": return `${Number(mm)} ${Number(hh)} * * ${f.weekday}`;
    default: return f.cron.trim();
  }
}

function presetOf(expr: string): Pick<Form, "preset" | "minute" | "time" | "weekday" | "cron"> {
  const base = { minute: "0", time: "03:00", weekday: "1", cron: expr };
  let m = /^(\d{1,2}) \* \* \* \*$/.exec(expr);
  if (m) return { ...base, preset: "hourly", minute: m[1]! };
  m = /^(\d{1,2}) (\d{1,2}) \* \* \*$/.exec(expr);
  if (m) return { ...base, preset: "daily", time: `${pad(Number(m[2]))}:${pad(Number(m[1]))}` };
  m = /^(\d{1,2}) (\d{1,2}) \* \* ([0-6])$/.exec(expr);
  if (m) return { ...base, preset: "weekly", time: `${pad(Number(m[2]))}:${pad(Number(m[1]))}`, weekday: m[3]! };
  return { ...base, preset: "custom" };
}

function formOf(name: string, project: string, spec: ScheduleSpec): Form {
  const t = spec.task;
  return {
    name, project,
    source: t.source?.image ? "image" : "app",
    app: t.fromApp ?? "",
    image: t.source?.image?.ref ?? "",
    pullSecret: t.source?.image?.pullSecret ?? "",
    command: formatCommand(t.command ?? []),
    env: (t.env ?? []).filter((e) => !e.valueFrom).map((e) => ({ name: e.name, value: e.value ?? "" })),
    ...presetOf(spec.schedule),
    timeZone: spec.timeZone ?? "",
    concurrency: spec.concurrency ?? "Forbid",
    keepSucceeded: String(spec.history?.succeeded ?? 3),
    keepFailed: String(spec.history?.failed ?? 3),
    timeout: prettyDuration(t.timeout),
    retries: String(t.retries ?? 0),
    stopSeconds: t.stopSeconds ? String(t.stopSeconds) : "",
    restart: t.onSuccess?.restart ?? [],
    mounts: mountsOf(t.volumes),
    size: t.size ?? "",
    egress: t.egress ?? "",
  };
}

const emptySpec: ScheduleSpec = { schedule: "0 3 * * *", task: {} };

// specOf applies the form to base, keeping what the form does not show
// (startingDeadline, custom resources, env from secrets, ttl, ...).
function specOf(f: Form, base: ScheduleSpec): ScheduleSpec {
  const spec: ScheduleSpec = structuredClone(base);
  spec.schedule = cronOf(f);
  if (f.timeZone.trim()) spec.timeZone = f.timeZone.trim();
  else delete spec.timeZone;
  spec.concurrency = f.concurrency;
  spec.history = { succeeded: Number(f.keepSucceeded), failed: Number(f.keepFailed) };
  const t: TaskSpec = spec.task;
  if (f.source === "app") {
    t.fromApp = f.app;
    delete t.source;
  } else {
    t.source = { image: { ref: f.image.trim(), ...(f.pullSecret.trim() ? { pullSecret: f.pullSecret.trim() } : {}) } };
    delete t.fromApp;
  }
  const command = splitCommand(f.command); // check() has rejected bad quoting
  if (command.length) t.command = command;
  else delete t.command;
  t.env = [...envOf(f.env).vars, ...(base.task.env ?? []).filter((e) => e.valueFrom)];
  if (t.env.length === 0) delete t.env;
  const keptDisks = ownDisks(base.task.volumes);
  t.volumes = [...keptDisks, ...volumesOf(f.mounts)];
  if (t.volumes.length === 0) delete t.volumes;
  if (f.timeout.trim()) t.timeout = f.timeout.trim();
  else delete t.timeout;
  t.retries = Number(f.retries);
  if (f.stopSeconds.trim()) t.stopSeconds = Number(f.stopSeconds);
  else delete t.stopSeconds;
  if (f.restart.length) t.onSuccess = { ...t.onSuccess, restart: f.restart };
  else delete t.onSuccess;
  if (f.size) t.size = f.size;
  else if (t.size !== "custom") delete t.size;
  if (f.egress) t.egress = f.egress;
  else delete t.egress;
  return spec;
}

type Problem = { field: string; message: string };

function check(f: Form, creating: boolean): Problem | undefined {
  if (creating) {
    if (!NAME_RE.test(f.name) || f.name.length > 52) return { field: "name", message: "Lowercase letters, digits and dashes, starting with a letter; at most 52 characters." };
    if (!f.project) return { field: "project", message: "Choose a project." };
  }
  if (f.source === "app" && !f.app) return { field: "app", message: "Choose the app whose image and settings the job uses." };
  if (f.source === "image" && !f.image.trim()) return { field: "image", message: "Enter an image, like ghcr.io/acme/ops:1.4.0." };
  try {
    splitCommand(f.command);
  } catch (e) {
    if (e instanceof CommandSyntaxError) return { field: "command", message: e.message };
    throw e;
  }
  const env = envOf(f.env);
  if (env.bad !== undefined) return { field: `env[${env.bad}]`, message: "Use letters, digits, _, - and ., not starting with a digit." };
  if (f.preset === "hourly" && !(Number.isInteger(Number(f.minute)) && Number(f.minute) >= 0 && Number(f.minute) <= 59 && f.minute.trim() !== "")) return { field: "cron", message: "A minute from 0 to 59." };
  if ((f.preset === "daily" || f.preset === "weekly") && !/^\d{2}:\d{2}$/.test(f.time)) return { field: "cron", message: "Choose a time." };
  if (f.preset === "custom" && !f.cron.trim()) return { field: "cron", message: "Enter a cron expression, like 30 3 * * *." };
  for (const [k, v] of [["keepSucceeded", f.keepSucceeded], ["keepFailed", f.keepFailed]] as const) {
    const n = Number(v);
    if (!Number.isInteger(n) || n < 0 || n > 100 || v.trim() === "") return { field: k, message: "A whole number from 0 to 100." };
  }
  if (f.timeout.trim() && !DURATION_RE.test(f.timeout.trim())) return { field: "timeout", message: "A duration such as 30m, 1h or 1h30m." };
  const r = Number(f.retries);
  if (!Number.isInteger(r) || r < 0 || r > 10 || f.retries.trim() === "") return { field: "retries", message: "A whole number from 0 to 10." };
  if (f.stopSeconds.trim()) {
    const st = Number(f.stopSeconds);
    if (!Number.isInteger(st) || st < 1 || st > 3600) return { field: "stopSeconds", message: "A whole number of seconds from 1 to 3600." };
  }
  const m = checkMounts(f.mounts);
  if (m) return { field: `mounts[${m[0]}]`, message: m[1] };
  return undefined;
}

// Server field paths → form fields.
function locate(field?: string): string | undefined {
  if (!field) return undefined;
  if (field === "name") return "name";
  if (field === "spec.schedule") return "cron";
  if (field === "spec.timeZone") return "timeZone";
  if (field === "spec.task.fromApp") return "app";
  if (field.startsWith("spec.task.command")) return "command";
  if (field.startsWith("spec.task.source")) return "image";
  const env = /^spec\.task\.env\[(\d+)\]/.exec(field);
  if (env) return `env[${env[1]}]`;
  const vol = /^spec\.task\.volumes\[(\d+)\]/.exec(field);
  if (vol) return `mounts[${vol[1]}]`;
  if (field.startsWith("spec.task.onSuccess")) return "restart";
  if (field === "spec.task.timeout") return "timeout";
  if (field === "spec.task.retries") return "retries";
  if (field === "spec.task.stopSeconds") return "stopSeconds";
  if (field === "spec.history.succeeded") return "keepSucceeded";
  if (field === "spec.history.failed") return "keepFailed";
  return undefined;
}

export function NewSchedule() {
  const search = newRoute.useSearch();
  return <ScheduleForm initial={formOf("", search.project ?? "", { ...emptySpec, task: { fromApp: search.fromApp } })} />;
}

export function ScheduleDetail() {
  const { project, name } = editRoute.useParams();
  const q = useQuery({ queryKey: ["schedule", project, name], queryFn: () => jobs.schedule(project, name), refetchInterval: 10000 });
  if (q.isPending) return <section className="view"><p className="loading">Loading {name}…</p></section>;
  if (q.isError) {
    return (
      <section className="view">
        <div className="ph"><div><h1>{name}</h1><p className="sub">project {project}</p></div></div>
        <div className="empty">
          <h2>{q.error instanceof ApiError && q.error.status === 404 ? "Schedule not found" : "Could not load this schedule"}</h2>
          <p>{errorText(q.error)}</p>
          <Link to="/jobs" className="btn">Back to jobs</Link>
        </div>
      </section>
    );
  }
  return <ScheduleForm key={q.data.metadata.name} schedule={q.data} initial={formOf(name, project, q.data.spec)} />;
}

function ScheduleForm({ schedule, initial: start }: { schedule?: Schedule; initial: Form }) {
  const creating = !schedule;
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const can = abilities(session.data);
  const projects = useQuery({ queryKey: ["projects"], queryFn: workloads.projects });
  const apps = useQuery({ queryKey: ["apps"], queryFn: () => workloads.apps() });

  // Like AppSettings: the form starts from the spec as loaded and is not
  // reset by polling; the generation sent on save refuses a stale write.
  const [base, setBase] = useState(schedule);
  const [initial, setInitial] = useState(start);
  const [f, setF] = useState(start);
  const [problem, setProblem] = useState<{ field?: string; message: string; conflict?: boolean }>();
  const [dialog, setDialog] = useState<"run" | "delete" | "project">();
  const set = <K extends keyof Form>(k: K, v: Form[K]) => {
    setF((prev) => ({ ...prev, [k]: v }));
    setProblem((p) => (p?.field === k ? undefined : p));
  };

  // Apps, volumes and restarts belong to one project; switching clears them.
  const changeProject = (p: string) => {
    setF((prev) => ({ ...prev, project: p, app: "", mounts: [], restart: [] }));
    setProblem(undefined);
  };
  const project = f.project || (creating && projects.data?.length === 1 ? projects.data[0]!.name : f.project);
  const form = { ...f, project };
  const projectApps = (apps.data ?? []).filter((a) => a.project === project);
  const baseSpec = base?.spec ?? emptySpec;
  const spec = specOf(form, baseSpec);
  const dirty = JSON.stringify(f) !== JSON.stringify(initial);
  const stale = schedule && base && schedule.metadata.generation > base.metadata.generation;
  const ro = !can.deploy;

  // The next runs, from the server's parser (the one the reconciler uses).
  const expr = useDeferredValue(spec.schedule);
  const tz = useDeferredValue(f.timeZone.trim());
  const preview = useQuery({
    queryKey: ["cron-preview", expr, tz],
    queryFn: () => jobs.preview(expr, tz),
    enabled: !!expr,
    retry: false,
    staleTime: 30000,
  });
  const previewError = preview.error instanceof ApiError ? preview.error : undefined;

  const save = useMutation({
    mutationFn: () =>
      creating ? jobs.createSchedule(project, f.name, spec) : jobs.updateSchedule(base!.metadata.namespace, base!.metadata.name, spec, base!.metadata.generation),
    onSuccess: async (saved) => {
      queryClient.setQueryData(["schedule", saved.metadata.namespace, saved.metadata.name], saved);
      void queryClient.invalidateQueries({ queryKey: ["schedules"] });
      if (creating) {
        await navigate({ to: "/jobs/$project/schedules/$name", params: { project: saved.metadata.namespace, name: saved.metadata.name } });
        return;
      }
      setBase(saved);
      const next = formOf(saved.metadata.name, saved.metadata.namespace, saved.spec);
      setInitial(next);
      setF(next);
      setProblem({ message: "Saved. The next run uses the new settings." });
    },
    onError: (e) => {
      if (!(e instanceof ApiError)) return setProblem({ message: errorText(e) });
      setProblem({ field: locate(e.field) ?? (e.field ? `server:${e.field}` : undefined), message: e.message, conflict: e.status === 409 });
    },
  });
  const toggle = useMutation({
    mutationFn: (suspend: boolean) => jobs.suspend(base!.metadata.namespace, base!.metadata.name, suspend),
    onSuccess: (s) => {
      queryClient.setQueryData(["schedule", s.metadata.namespace, s.metadata.name], s);
      void queryClient.invalidateQueries({ queryKey: ["schedules"] });
    },
  });
  const runs = useQuery({
    queryKey: ["tasks", { project: schedule?.metadata.namespace, schedule: schedule?.metadata.name }],
    queryFn: () => jobs.tasks({ project: schedule!.metadata.namespace, schedule: schedule!.metadata.name }),
    enabled: !!schedule,
    refetchInterval: 5000,
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    const p = check(form, creating);
    if (p) return setProblem(p);
    setProblem(undefined);
    save.mutate();
  }

  function reload() {
    if (!schedule) return;
    setBase(schedule);
    const next = formOf(schedule.metadata.name, schedule.metadata.namespace, schedule.spec);
    setInitial(next);
    setF(next);
    setProblem(undefined);
  }

  const err = (field: string) => (problem?.field === field ? problem.message : undefined);
  const indexed = (prefix: string): [number, string] | undefined => {
    const m = new RegExp(`^${prefix}\\[(\\d+)\\]`).exec(problem?.field ?? "");
    return m ? [Number(m[1]), problem!.message] : undefined;
  };
  const envErr = indexed("env");
  const desc = describeCron(spec.schedule);
  const suspended = toggle.isPending ? !!toggle.variables : !!schedule?.spec.suspend;
  const yaml = `apiVersion: kwerft.dev/v1alpha1\nkind: Schedule\nmetadata:\n  name: ${f.name || "my-schedule"}\n  namespace: ${project || "my-project"}\nspec:\n${toYAML(spec, 1)}`;
  const known = problem?.field && !problem.field.startsWith("server:");

  return (
    <section className="view">
      <div className="ph">
        <div>
          <h1>
            {creating ? "New schedule" : schedule!.metadata.name}
            {schedule && (suspended ? <span className="pill mute">Suspended</span> : <span className="pill ok">Active</span>)}
          </h1>
          <p className="sub">
            {creating ? <>Runs a task on a cron schedule. Each run is a Task, so scheduled and manual runs look the same.</>
              : <>project <Link to="/jobs" search={{ project: schedule!.metadata.namespace }} className="dim">{schedule!.metadata.namespace}</Link>
                {schedule!.status?.nextScheduleTime && !suspended ? ` · next run ${when(schedule!.status.nextScheduleTime)}` : ""}</>}
          </p>
        </div>
        {schedule && (
          <div className="acts">
            <button type="button" role="switch" aria-checked={!suspended} className={`toggle labelled ${suspended ? "" : "on"}`} disabled={ro || toggle.isPending}
              onClick={() => toggle.mutate(!suspended)}><i />{suspended ? "Suspended" : "Active"}</button>
            <button className="btn" disabled={ro} onClick={() => setDialog("run")}><Icon name="play" />Run now</button>
            <button className="btn danger" disabled={ro} onClick={() => setDialog("delete")}><Icon name="trash" />Delete</button>
          </div>
        )}
      </div>

      {ro && <div className="banner info"><Icon name="shield" /><span>Your role can view schedules but not change them.</span></div>}
      {toggle.isError && <div className="banner bad" role="alert"><Icon name="alert" /><span>{errorText(toggle.error)}</span></div>}
      {stale && !save.isPending && (
        <div className="banner warn">
          <Icon name="alert" /><span>This schedule changed since you opened it.</span>
          <button type="button" className="btn sm" onClick={reload}>Load the latest</button>
        </div>
      )}
      {schedule?.status?.conditions?.find((c) => c.type === "Ready" && c.status === "False") && (
        <div className="banner bad" role="alert"><Icon name="alert" /><span>{schedule.status.conditions.find((c) => c.type === "Ready")!.message}</span></div>
      )}

      <form className="tabpanel" onSubmit={submit} noValidate>
        <fieldset disabled={ro} className="plain">
          <div className="g2e">
            <div className="card">
              <h3>What runs</h3>
              <div className="bd fields">
                {creating && (
                  <>
                    <Field id="s-name" label="Name" className="mono" value={f.name} onChange={(e) => set("name", e.target.value.toLowerCase())} placeholder="nightly-backup"
                      autoFocus autoComplete="off" spellCheck={false} maxLength={52} error={err("name")} hint="Runs are named after it." />
                    <div className="field">
                      <label htmlFor="s-proj">Project</label>
                      <select id="s-proj" className="input" value={project} aria-invalid={!!err("project")}
                        onChange={(e) => (e.target.value === "+new" ? setDialog("project") : changeProject(e.target.value))}>
                        {!project && <option value="">Choose a project…</option>}
                        {projects.data?.map((p) => <option key={p.name} value={p.name}>{p.name}</option>)}
                        {can.manageProjects && <option value="+new">New project…</option>}
                      </select>
                      {err("project") && <span className="field-error" role="alert">{err("project")}</span>}
                    </div>
                  </>
                )}
                <div className="field full">
                  <label>Source</label>
                  <div className="seg" role="group" aria-label="Source">
                    <button type="button" aria-pressed={f.source === "app"} onClick={() => set("source", "app")}>From an app</button>
                    <button type="button" aria-pressed={f.source === "image"} onClick={() => set("source", "image")}>Container image</button>
                  </div>
                </div>
                {f.source === "app" ? (
                  <div className="field full">
                    <label htmlFor="s-app">App</label>
                    <select id="s-app" className="input" value={f.app} aria-invalid={!!err("app")} onChange={(e) => set("app", e.target.value)}>
                      <option value="">{project ? "Choose an app…" : "Choose a project first"}</option>
                      {projectApps.map((a) => <option key={a.name} value={a.name}>{a.name} · {a.image || a.source.repository}</option>)}
                      {f.app && !projectApps.some((a) => a.name === f.app) && <option value={f.app}>{f.app} (not found)</option>}
                    </select>
                    {err("app") ? <span className="field-error" role="alert">{err("app")}</span>
                      : <span className="hint">Runs the app's current image with its command, env, size, outbound access and shared volumes, read when each run starts.</span>}
                  </div>
                ) : (
                  <>
                    <div className="full">
                      <Field id="s-image" label="Image" className="mono" value={f.image} onChange={(e) => set("image", e.target.value)} placeholder="ghcr.io/acme/ops:1.4.0"
                        autoComplete="off" spellCheck={false} error={err("image")} />
                    </div>
                    <Field id="s-secret" label="Registry credential" className="mono" value={f.pullSecret} onChange={(e) => set("pullSecret", e.target.value)} placeholder="None (public image)"
                      autoComplete="off" spellCheck={false} />
                  </>
                )}
                <div className="full"><Field id="s-cmd" className="mono" label={`Command ${f.source === "app" ? "(empty: the app's)" : "(empty: the image's)"}`}
                  value={f.command} onChange={(e) => set("command", e.target.value)} spellCheck={false} autoComplete="off"
                  placeholder="bin/reindex --full" error={err("command")}
                  hint={`Like in a shell: echo "hi there". For pipes, && or $VARIABLES use sh -c '…'.`} /></div>
                <div className="field full">
                  <label>Env{f.source === "app" ? " (added to the app's)" : ""}</label>
                  <EnvRows rows={f.env} onChange={(v) => set("env", v)} label="Variable" errorAt={envErr ? { index: envErr[0], message: envErr[1] } : undefined} />
                </div>
                <div className="field full">
                  <label>Shared volumes and secrets{f.source === "app" ? " (besides the app's)" : ""}</label>
                  <VolumeMounts project={project} mounts={f.mounts} onChange={(v) => set("mounts", v)} errorAt={indexed("mounts")} idPrefix="s-vol" />
                </div>
              </div>
            </div>

            <div className="card">
              <h3>When</h3>
              <div className="bd fields">
                <div className="field full">
                  <label>Runs</label>
                  <div className="seg" role="group" aria-label="Schedule preset">
                    {(["hourly", "daily", "weekly", "custom"] as const).map((p) => (
                      <button type="button" key={p} aria-pressed={f.preset === p}
                        onClick={() => { set("preset", p); if (p === "custom" && !f.cron.trim()) set("cron", cronOf(f)); }}>
                        {{ hourly: "Hourly", daily: "Daily at…", weekly: "Weekly", custom: "Custom" }[p]}
                      </button>
                    ))}
                  </div>
                </div>
                {f.preset === "hourly" && (
                  <Field id="s-minute" label="At minute" type="number" min={0} max={59} value={f.minute} onChange={(e) => set("minute", e.target.value)} error={err("cron")} />
                )}
                {f.preset === "weekly" && (
                  <div className="field">
                    <label htmlFor="s-day">Day</label>
                    <select id="s-day" className="input" value={f.weekday} onChange={(e) => set("weekday", e.target.value)}>
                      {weekdays.map((d, i) => <option key={d} value={String(i)}>{d}</option>)}
                    </select>
                  </div>
                )}
                {(f.preset === "daily" || f.preset === "weekly") && (
                  <Field id="s-time" label="At" type="time" value={f.time} onChange={(e) => set("time", e.target.value)} error={err("cron")} />
                )}
                {f.preset === "custom" && (
                  <div className="full">
                    <Field id="s-cron" label="Cron expression" className="mono" value={f.cron} onChange={(e) => set("cron", e.target.value)} placeholder="30 3 * * 1-5"
                      spellCheck={false} autoComplete="off" error={err("cron") ?? (previewError?.field === "schedule" ? previewError.message : undefined)}
                      hint="minute hour day-of-month month day-of-week, or @hourly, @daily, @weekly, @every 2h" />
                  </div>
                )}
                <div className="field full">
                  <label htmlFor="s-tz">Time zone</label>
                  <input id="s-tz" className="input" list="s-tz-list" value={f.timeZone} onChange={(e) => set("timeZone", e.target.value)} placeholder="Server time (UTC)"
                    aria-invalid={!!err("timeZone") || previewError?.field === "timeZone"} autoComplete="off" spellCheck={false} />
                  <datalist id="s-tz-list">{timeZones().map((z) => <option key={z} value={z} />)}</datalist>
                  {err("timeZone") || previewError?.field === "timeZone"
                    ? <span className="field-error" role="alert">{err("timeZone") ?? previewError!.message}</span>
                    : <span className="hint">An IANA name such as Europe/Berlin; daylight saving is handled.</span>}
                </div>
                <div className="cron-preview full" aria-live="polite">
                  <div><span className="mono">{spec.schedule || "—"}</span>{desc && <b> · {desc}</b>}</div>
                  {preview.data ? (
                    <ol>{preview.data.next.map((n) => <li key={n}>{fmtRun(n)}</li>)}</ol>
                  ) : previewError && f.preset !== "custom" ? <span className="field-error">{previewError.message}</span> : preview.isFetching ? <span className="dim">…</span> : null}
                </div>
                <div className="field full">
                  <label>If a run is still going when the next is due</label>
                  <div className="seg" role="group" aria-label="Concurrency">
                    {(["Forbid", "Replace", "Allow"] as const).map((c) => (
                      <button type="button" key={c} aria-pressed={f.concurrency === c} onClick={() => set("concurrency", c)}>
                        {{ Forbid: "Wait for it", Replace: "Stop it", Allow: "Run both" }[c]}
                      </button>
                    ))}
                  </div>
                  <span className="hint">{{ Forbid: "The due run starts once the earlier one finishes, within an hour; else it is skipped.", Replace: "The earlier run is cancelled and the new one starts.", Allow: "Runs overlap; make sure the job tolerates that." }[f.concurrency]}</span>
                </div>
              </div>
            </div>
          </div>

          <div className="g2e">
            <div className="card">
              <h3>Limits &amp; history</h3>
              <div className="bd fields">
                <Field id="s-timeout" label="Timeout" className="mono" value={f.timeout} onChange={(e) => set("timeout", e.target.value)} placeholder="none" error={err("timeout")}
                  hint="Stops the run (all retries) after this long: 30m, 2h." />
                <Field id="s-retries" label="Retries" type="number" min={0} max={10} value={f.retries} onChange={(e) => set("retries", e.target.value)} error={err("retries")}
                  hint="How often a failed run starts again." />
                <Field id="s-stop" label="Time to stop" type="number" min={1} max={3600} value={f.stopSeconds} onChange={(e) => set("stopSeconds", e.target.value)} placeholder="30" error={err("stopSeconds")}
                  hint="Seconds a run gets to finish after it is stopped, before it is killed." />
                <Field id="s-keep-ok" label="Keep succeeded runs" type="number" min={0} max={100} value={f.keepSucceeded} onChange={(e) => set("keepSucceeded", e.target.value)} error={err("keepSucceeded")} />
                <Field id="s-keep-bad" label="Keep failed runs" type="number" min={0} max={100} value={f.keepFailed} onChange={(e) => set("keepFailed", e.target.value)} error={err("keepFailed")}
                  hint="With their logs; older ones are removed." />
                <div className="field">
                  <label htmlFor="s-size">Size</label>
                  <select id="s-size" className="input" value={f.size} onChange={(e) => set("size", e.target.value as Form["size"])}>
                    <option value="">{f.source === "app" ? "Same as the app" : "Small (default)"}</option>
                    {sizes.map((s) => <option key={s.id} value={s.id}>{s.label} · {s.note}</option>)}
                    {f.size === "custom" && <option value="custom">Custom requests and limits</option>}
                  </select>
                </div>
                <div className="field">
                  <label htmlFor="s-egress">Outbound access</label>
                  <select id="s-egress" className="input" value={f.egress} onChange={(e) => set("egress", e.target.value as Egress)}>
                    <option value="">{f.source === "app" ? "Same as the app" : "HTTPS to the internet (default)"}</option>
                    <option value="none">None</option>
                    <option value="https">HTTPS to the internet</option>
                    <option value="all">Unrestricted</option>
                  </select>
                </div>
              </div>
            </div>
            <div className="card">
              <h3>On success</h3>
              <div className="bd">
                <fieldset className="plain checks" aria-describedby="s-restart-hint">
                  <legend className="sr">Restart apps on success</legend>
                  {projectApps.length === 0 && <span className="dim">{project ? `No apps in ${project}.` : "Choose a project to pick apps."}</span>}
                  {projectApps.map((a) => (
                    <label key={a.name} className="check">
                      <input type="checkbox" checked={f.restart.includes(a.name)}
                        onChange={(e) => set("restart", e.target.checked ? [...f.restart, a.name] : f.restart.filter((x) => x !== a.name))} />
                      Restart <b>{a.name}</b>
                    </label>
                  ))}
                </fieldset>
                <p id="s-restart-hint" className="hint">Kwerft rolls these apps out once a run succeeds, e.g. a server that has to reopen a file the job replaced. The job itself needs no permissions for it.</p>
                {err("restart") && <p className="field-error" role="alert">{err("restart")}</p>}
              </div>
            </div>
          </div>
        </fieldset>

        {problem && (!known || problem.field === "project") && !problem.field?.startsWith("env") && (
          <div className={`banner ${problem.field || problem.conflict || save.isError ? "bad" : "info"}`} role={save.isError ? "alert" : "status"}>
            <Icon name={save.isError ? "alert" : "clock"} />
            <span>{problem.field?.startsWith("server:") ? <><code>{problem.field.slice(7)}</code>: </> : null}{problem.message}</span>
            {problem.conflict && <button type="button" className="btn sm" onClick={reload}>Load the latest</button>}
          </div>
        )}

        {can.deploy && (
          <div className="acts" style={{ justifyContent: "space-between" }}>
            <details className="yaml">
              <summary>Schedule as YAML</summary>
              <pre className="codebox">{yaml}</pre>
            </details>
            <div className="acts">
              {creating && <Link to="/jobs" className="btn">Cancel</Link>}
              <button className="btn pri" disabled={save.isPending || (!creating && !dirty)}>
                <Icon name="clock" />{save.isPending ? "Saving…" : creating ? "Create schedule" : "Save changes"}
              </button>
            </div>
          </div>
        )}
      </form>

      {schedule && (
        <div className="card">
          <div className="ch-h"><h3>Runs</h3><span className="dim small">keeps {schedule.spec.history?.succeeded ?? 3} succeeded and {schedule.spec.history?.failed ?? 3} failed</span></div>
          {runs.data && runs.data.length > 0 ? <RunsTable tasks={runs.data} showSource={false} />
            : <div className="li empty-li">{runs.isPending ? "Loading runs…" : "No runs yet."}</div>}
        </div>
      )}

      {dialog === "project" && <CreateProjectDialog onClose={() => setDialog(undefined)} onCreated={(p) => set("project", p.name)} />}
      {dialog === "run" && schedule && <RunNowDialog source={{ kind: "schedule", project: schedule.metadata.namespace, schedule: schedule.metadata.name }} onClose={() => setDialog(undefined)} />}
      {dialog === "delete" && schedule && <DeleteScheduleDialog schedule={schedule} onClose={() => setDialog(undefined)} />}
    </section>
  );
}

/** "Mon 5 Oct 03:30 +02:00" from an RFC 3339 time, in the schedule's own zone (the offset it carries). */
function fmtRun(rfc3339: string) {
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2}):\d{2}(Z|[+-]\d{2}:\d{2})$/.exec(rfc3339);
  if (!m) return rfc3339;
  const [, y, mo, d, h, mi, off] = m;
  const day = new Date(Date.UTC(Number(y), Number(mo) - 1, Number(d)));
  const wd = day.toLocaleDateString(undefined, { weekday: "short", timeZone: "UTC" });
  const mon = day.toLocaleDateString(undefined, { month: "short", timeZone: "UTC" });
  return `${wd} ${Number(d)} ${mon} ${h}:${mi} ${off === "Z" ? "UTC" : off}`;
}

function DeleteScheduleDialog({ schedule, onClose }: { schedule: Schedule; onClose: () => void }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const del = useMutation({
    mutationFn: () => jobs.deleteSchedule(schedule.metadata.namespace, schedule.metadata.name),
    onSuccess: async () => {
      void queryClient.invalidateQueries({ queryKey: ["schedules"] });
      void queryClient.invalidateQueries({ queryKey: ["tasks"] });
      await navigate({ to: "/jobs" });
    },
  });
  return (
    <Dialog title={`Delete ${schedule.metadata.name}?`} onClose={onClose} onSubmit={() => del.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri danger" disabled={del.isPending}>{del.isPending ? "Deleting…" : "Delete schedule"}</button>
      </>}>
      <p style={{ margin: 0 }}>No more runs start. Its runs, including any still going, and their logs are removed with it.</p>
      {del.isError && <p className="form-error" role="alert">{errorText(del.error)}</p>}
    </Dialog>
  );
}
