import { useState } from "react";
import { Link, getRouteApi, useNavigate } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import { Icon } from "../components/Icon";
import { ClusterBadge, ClusterFilter } from "../components/ClusterUI";
import { inCluster, useClusterFilter } from "../clusters";
import { TaskStatus } from "../components/TaskStatus";
import { abilities, ago, words, workloads } from "../workloads";
import { describeCron, duration, jobs, startedBy, when, type ScheduleSummary, type TaskSummary } from "../jobs";
import { errorText } from "./Apps";
import { RunNowDialog, type RunSource } from "./RunNowDialog";
import "../styles/workloads.css";
import "../styles/jobs.css";

const POLL = 5000;
const route = getRouteApi("/authed/jobs");

// Jobs: Schedules (cron, next and last run, suspend toggle, run now) and
// recent runs across projects, as in the blueprint's Jobs view.
export function Jobs() {
  const search = route.useSearch();
  const navigate = useNavigate();
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const can = abilities(session.data);
  const projects = useQuery({ queryKey: ["projects"], queryFn: workloads.projects, refetchInterval: POLL });
  const schedules = useQuery({ queryKey: ["schedules"], queryFn: () => jobs.schedules(), refetchInterval: POLL });
  const tasks = useQuery({ queryKey: ["tasks", { limit: 100 }], queryFn: () => jobs.tasks({ limit: 100 }), refetchInterval: POLL });
  const [running, setRunning] = useState<RunSource | "pick">();
  const [showAll, setShowAll] = useState(false);

  const project = search.project;
  const setProject = (p?: string) => void navigate({ to: "/jobs", search: p ? { project: p } : {}, replace: true });
  const [cluster, setCluster] = useClusterFilter();
  const projectList = inCluster(projects.data ?? [], cluster);
  const shownSchedules = inCluster(schedules.data ?? [], cluster).filter((s) => !project || s.project === project);
  const shownTasks = inCluster(tasks.data ?? [], cluster).filter((t) => !project || t.project === project);
  const failed = shownSchedules
    .filter((s) => s.lastRun?.phase === "failed" && s.lastRun.reason !== "Cancelled")
    .sort((a, b) => (b.lastRun!.finished ?? "").localeCompare(a.lastRun!.finished ?? ""));
  const tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
  const denied = can.deploy ? undefined : "Your role can view jobs but not run or change them.";
  const noProjects = projects.isSuccess && projects.data.length === 0;

  return (
    <section className="view">
      <div className="ph">
        <div>
          <h1>Jobs</h1>
          <p>Scheduled and one-off runs · same image, env and volumes as an app · times in {tz}</p>
        </div>
        <div className="acts">
          <Link to="/jobs/new" search={project ? { project } : {}} className="btn" disabled={!can.deploy || noProjects} aria-disabled={!can.deploy || noProjects}
            title={denied ?? (noProjects ? "Create a project first." : undefined)}><Icon name="plus" />New schedule</Link>
          <button className="btn pri" disabled={!can.deploy || noProjects} title={denied} onClick={() => setRunning("pick")}><Icon name="play" />Run task</button>
        </div>
      </div>

      {(schedules.isError || tasks.isError) && (
        <div className="banner bad" role="alert"><Icon name="alert" /><span>{errorText(schedules.error ?? tasks.error)}</span></div>
      )}

      {failed[0] && <FailedBanner s={failed[0]} more={failed.length - 1} canRun={can.deploy} onRun={() => setRunning({ kind: "schedule", project: failed[0]!.project, schedule: failed[0]!.name })} />}

      <div className="card">
        <div className="ch-h">
          <h3>Schedules</h3>
          <div className="acts">
            <ClusterFilter value={cluster} onChange={(c) => { setCluster(c); setProject(undefined); }} />
            {projectList.length > 1 && (
              <div className="seg" role="group" aria-label="Project">
                <button aria-pressed={!project} onClick={() => setProject(undefined)}>All projects</button>
                {projectList.map((p) => <button key={p.name} aria-pressed={project === p.name} onClick={() => setProject(p.name)}>{p.name}</button>)}
              </div>
            )}
          </div>
        </div>
        {schedules.isPending ? (
          <p className="loading pad">Loading schedules…</p>
        ) : shownSchedules.length === 0 ? (
          <div className="li empty-li">
            <span>No schedules{project ? ` in ${project}` : ""} yet. A schedule runs a task on a cron schedule: a nightly backup, an hourly import, a weekly report.</span>
            {can.deploy && !noProjects && <Link to="/jobs/new" search={project ? { project } : {}} className="btn sm end"><Icon name="plus" />New schedule</Link>}
          </div>
        ) : (
          <div className="scroll-x">
            <table className="t">
              <thead>
                <tr><th>Schedule</th><th>Image</th><th>Runs</th><th>Next run</th><th>Last run</th><th>On success</th><th>Active</th><th><span className="sr">Actions</span></th></tr>
              </thead>
              <tbody>
                {shownSchedules.map((s) => (
                  <ScheduleRow key={`${s.project}/${s.name}`} s={s} canEdit={can.deploy}
                    onOpen={() => navigate({ to: "/jobs/$project/schedules/$name", params: { project: s.project, name: s.name } })}
                    onRun={() => setRunning({ kind: "schedule", project: s.project, schedule: s.name })} />
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>

      <div className="card">
        <div className="ch-h"><h3>Recent runs</h3>{shownTasks.length > 0 && <span className="dim small">{shownTasks.length} kept</span>}</div>
        {tasks.isPending ? (
          <p className="loading pad">Loading runs…</p>
        ) : shownTasks.length === 0 ? (
          <div className="li empty-li">No runs{project ? ` in ${project}` : ""} yet. Run a task from a schedule or an app; finished runs are kept for their logs.</div>
        ) : (
          <>
            <RunsTable tasks={showAll ? shownTasks : shownTasks.slice(0, 15)} showProject={!project} />
            {shownTasks.length > 15 && !showAll && (
              <div className="li"><button className="btn sm" onClick={() => setShowAll(true)}>Show all {shownTasks.length} runs</button></div>
            )}
          </>
        )}
      </div>

      {running && <RunNowDialog source={running === "pick" ? undefined : running} onClose={() => setRunning(undefined)} />}
    </section>
  );
}

function FailedBanner({ s, more, canRun, onRun }: { s: ScheduleSummary; more: number; canRun: boolean; onRun: () => void }) {
  const r = s.lastRun!;
  return (
    <div className="banner warn" role="status">
      <Icon name="alert" />
      <span>
        <b>{s.name} failed {ago(r.finished ?? r.created)}.</b>{" "}
        {r.exitCode !== undefined ? `Exit code ${r.exitCode}` : r.reason ? words(r.reason) : "It failed"}
        {r.started ? ` after ${duration(r.started, r.finished)}` : ""}.{" "}
        {s.suspend ? "The schedule is suspended." : s.nextRun ? `The next run is ${when(s.nextRun)}.` : ""}
        {more > 0 ? ` ${more} more schedule${more > 1 ? "s" : ""} failed too.` : ""}{" "}
        <Link to="/jobs/$project/tasks/$name" params={{ project: r.project, name: r.name }}>View the run</Link>
      </span>
      {canRun && <button className="btn sm" onClick={onRun}>Run now</button>}
    </div>
  );
}

function ScheduleRow({ s, canEdit, onOpen, onRun }: { s: ScheduleSummary; canEdit: boolean; onOpen: () => void; onRun: () => void }) {
  const queryClient = useQueryClient();
  const toggle = useMutation({
    mutationFn: (suspend: boolean) => jobs.suspend(s.project, s.name, suspend),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["schedules"] }),
  });
  const suspended = toggle.isPending ? !!toggle.variables : s.suspend;
  const desc = describeCron(s.schedule);
  const last = s.lastRun;
  return (
    <tr className="click" onClick={onOpen}>
      <td className="nowrap">
        <span className="nm"><Link to="/jobs/$project/schedules/$name" params={{ project: s.project, name: s.name }} onClick={(e) => e.stopPropagation()}>{s.name}</Link></span>
        <span className="sub">{s.project}<ClusterBadge cluster={s.cluster} /></span>
      </td>
      <td className="mono ell" title={s.image ?? `from app ${s.fromApp}`}>{s.image ?? <>from app <b>{s.fromApp}</b></>}</td>
      <td>
        <span className="mono nowrap">{s.schedule}</span>
        <span className="sub">{desc || "custom"}{s.timeZone ? ` · ${s.timeZone}` : ""}</span>
      </td>
      <td className={suspended ? "dim nowrap" : "nowrap"} title={s.nextRun ? new Date(s.nextRun).toLocaleString() : s.message}>
        {suspended ? "suspended" : s.phase === "failed" ? <span className="pill bad" title={s.message}>{words(s.reason ?? "Failed")}</span> : when(s.nextRun)}
        {s.active.length > 0 && <span className="sub">{s.active.length} running</span>}
      </td>
      <td>
        {last ? (
          <>
            <TaskStatus phase={last.phase} reason={last.reason} message={last.message} muted={suspended && last.phase === "succeeded"} />
            <span className="sub">
              {last.phase === "failed" && last.exitCode !== undefined ? `exit ${last.exitCode}` : duration(last.started, last.finished)}
              {" · "}{ago(last.finished ?? last.started ?? last.created)}
            </span>
          </>
        ) : <span className="dim">never</span>}
      </td>
      <td>{s.restart.length ? <>restart {s.restart.map((a, i) => <span key={a}>{i > 0 ? ", " : ""}<b>{a}</b></span>)}</> : <span className="dim">—</span>}</td>
      <td onClick={(e) => e.stopPropagation()}>
        <button type="button" role="switch" aria-checked={!suspended} className={`toggle ${suspended ? "" : "on"}`} disabled={!canEdit || toggle.isPending}
          aria-label={`${s.name}: ${suspended ? "suspended" : "active"}`} title={toggle.isError ? errorText(toggle.error) : suspended ? "Resume: start runs again" : "Suspend: no new runs"}
          onClick={() => toggle.mutate(!suspended)}><i /></button>
      </td>
      <td onClick={(e) => e.stopPropagation()}>
        <button className="btn sm" disabled={!canEdit} onClick={onRun} title={canEdit ? "Run once now, with optional env overrides" : "Your role cannot run jobs."}><Icon name="play" />Run now</button>
      </td>
    </tr>
  );
}

// The runs table, shared with the schedule page.
export function RunsTable({ tasks, showProject, showSource = true }: { tasks: TaskSummary[]; showProject?: boolean; showSource?: boolean }) {
  const navigate = useNavigate();
  return (
    <div className="scroll-x">
      <table className="t">
        <thead><tr><th>Task</th><th>Started by</th><th>Started</th><th className="num">Duration</th><th className="num">Exit</th><th>Status</th></tr></thead>
        <tbody>
          {tasks.map((t) => {
            const by = startedBy(t);
            const source = t.schedule ?? (t.fromApp ? `app ${t.fromApp}` : t.image);
            const sub = [showProject ? t.project : "", showSource ? source : "", t.restart.length && t.phase === "succeeded" ? `restarted ${t.restart.join(", ")}` : ""].filter(Boolean);
            return (
              <tr key={`${t.project}/${t.name}`} className="click" onClick={() => navigate({ to: "/jobs/$project/tasks/$name", params: { project: t.project, name: t.name } })}>
                <td>
                  <span className="nm"><Link to="/jobs/$project/tasks/$name" params={{ project: t.project, name: t.name }} onClick={(e) => e.stopPropagation()}>{t.name}</Link></span>
                  <span className="sub">
                    {sub.join(" · ")}
                    {t.overrides.length > 0 && <>{sub.length ? " · " : ""}{t.overrides.map((o) => <span key={o} className="tag">{o}</span>)}</>}
                    {showProject && <ClusterBadge cluster={t.cluster} />}
                  </span>
                </td>
                <td className={by.who === "schedule" ? "dim" : undefined}>{by.who}{by.how ? <span className="sub">{by.how}</span> : null}</td>
                <td className="dim nowrap" title={t.started ? new Date(t.started).toLocaleString() : undefined}>{ago(t.started ?? t.created)}</td>
                <td className="num">{t.started ? duration(t.started, t.finished) : "—"}</td>
                <td className="num">{t.exitCode ?? "—"}</td>
                <td><TaskStatus phase={t.phase} reason={t.reason} message={t.message} /></td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}
