import { lazy, Suspense, useState } from "react";
import { Link, getRouteApi, useNavigate } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../api";
import { Dialog } from "../components/Dialog";
import { Icon } from "../components/Icon";
import { TaskStatus, phaseOfTask } from "../components/TaskStatus";
import { duration, jobs, prettyDuration, type Task } from "../jobs";
import { describeMount } from "../mounts";
import { podsApi, type Replicas } from "../pods";
import { abilities, ago, sizes, words, workloads, type EnvVar } from "../workloads";
import { errorText } from "./Apps";
import { RunNowDialog, type RunSource } from "./RunNowDialog";
import { TaskLogs } from "./TaskLogs";
import "../styles/workloads.css";
import "../styles/jobs.css";
import { formatCommand } from "../shellwords";

const route = getRouteApi("/authed/jobs/$project/tasks/$name");

// xterm.js is large: load it with the first shell, not with the console.
const ShellDialog = lazy(() => import("../components/Terminal").then((m) => ({ default: m.ShellDialog })));

export function TaskDetail() {
  const { project, name } = route.useParams();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const projects = useQuery({ queryKey: ["projects"], queryFn: workloads.projects });
  const can = abilities(session.data, projects.data?.find((p) => p.name === project));
  const key = ["task", project, name];
  const q = useQuery({
    queryKey: key,
    queryFn: () => jobs.task(project, name),
    refetchInterval: (query) => (query.state.data && phaseOfTask(query.state.data.status) !== "succeeded" && phaseOfTask(query.state.data.status) !== "failed" ? 3000 : 15000),
  });
  const [dialog, setDialog] = useState<"cancel" | "delete" | "again" | "shell">();
  // A shell is possible while the run runs. The pods answer says whether
  // Kubernetes RBAC lets this user exec (the same query as the logs' pods).
  const running = q.data !== undefined && phaseOfTask(q.data.status) === "running";
  const pods = useQuery({
    queryKey: ["task-pods", project, name],
    queryFn: () => podsApi.taskPods(project, name),
    enabled: running,
    refetchInterval: running ? 5000 : false,
  });

  if (q.isPending) return <section className="view"><p className="loading">Loading {name}…</p></section>;
  if (q.isError) {
    return (
      <section className="view">
        <div className="ph"><div><h1>{name}</h1><p className="sub">project {project}</p></div></div>
        <div className="empty">
          <h2>{q.error instanceof ApiError && q.error.status === 404 ? "Run not found" : "Could not load this run"}</h2>
          <p>{errorText(q.error)}{q.error instanceof ApiError && q.error.status === 404 ? " Finished runs are removed after their retention time, or when their schedule keeps only the latest ones." : ""}</p>
          <Link to="/jobs" className="btn">Back to jobs</Link>
        </div>
      </section>
    );
  }

  const task = q.data;
  const st = task.status ?? {};
  const phase = phaseOfTask(st);
  const ready = st.conditions?.find((c) => c.type === "Ready");
  const restarted = st.conditions?.find((c) => c.type === "AppsRestarted");
  const finished = phase === "succeeded" || phase === "failed";
  const schedule = task.metadata.labels?.["kwerft.dev/schedule"];
  const by = task.metadata.annotations?.["kwerft.dev/started-by"];
  const scheduledAt = task.metadata.annotations?.["kwerft.dev/scheduled-at"];
  const spec = task.spec;
  const size = sizes.find((s) => s.id === spec.size);
  const denied = can.deploy ? undefined : "Your role can view runs but not change them.";
  const again: RunSource | undefined = schedule
    ? { kind: "schedule", project, schedule }
    : spec.fromApp ? { kind: "app", project, app: spec.fromApp } : undefined;

  return (
    <section className="view">
      <div className="ph">
        <div>
          <h1>{name} <TaskStatus phase={phase} reason={ready?.reason} message={ready?.message} /></h1>
          <p className="sub">
            {schedule ? <>Run of schedule <Link to="/jobs/$project/schedules/$name" params={{ project, name: schedule }}>{schedule}</Link></>
              : spec.fromApp ? <>From app <Link to="/apps/$project/$name" params={{ project, name: spec.fromApp }}>{spec.fromApp}</Link></>
              : <>Image <code>{spec.source?.image?.ref}</code></>}
            {" · "}project <Link to="/jobs" search={{ project }} className="dim">{project}</Link>
            {" · "}{by ? <>started by {by}</> : schedule ? "started by the schedule" : "started by kubectl"} {ago(task.metadata.creationTimestamp)}
          </p>
        </div>
        <div className="acts">
          {again && <button className="btn" disabled={!can.deploy} title={denied} onClick={() => setDialog("again")}><Icon name="play" />Run again</button>}
          {running && <ShellButton pod={st.pod} pods={pods.data} onOpen={() => setDialog("shell")} />}
          {!finished && <button className="btn danger" disabled={!can.deploy} title={denied} onClick={() => setDialog("cancel")}>Cancel run</button>}
          <button className="btn danger" disabled={!can.deploy} title={denied} onClick={() => setDialog("delete")}><Icon name="trash" />Delete</button>
        </div>
      </div>

      {phase === "failed" && ready && (
        <div className={`banner ${ready.reason === "Cancelled" ? "info" : "bad"}`} role={ready.reason === "Cancelled" ? "status" : "alert"}>
          <Icon name="alert" /><span><b>{words(ready.reason)}</b>{ready.message ? `: ${ready.message}` : ""}</span>
        </div>
      )}
      {phase === "pending" && ready && !["Pending", "Created"].includes(ready.reason) && (
        <div className="banner warn" role="status"><Icon name="alert" /><span><b>{words(ready.reason)}</b>: {ready.message}</span></div>
      )}

      <div className="g2">
        <div className="card">
          <h3>Run</h3>
          <div className="stats">
            <div><span className="k">Status</span><span className="v" style={{ fontSize: 16 }}>{ready?.reason === "Cancelled" ? "Cancelled" : words(st.phase ?? "Pending")}</span><span className="s">{ready?.message ?? "Waiting for the controller"}</span></div>
            <div><span className="k">Exit code</span><span className="v">{st.exitCode ?? "—"}</span><span className="s">{st.pod ? `pod ${st.pod}` : "no pod yet"}</span></div>
            <div><span className="k">Duration</span><span className="v" style={{ fontSize: 16 }}>{st.startTime ? duration(st.startTime, st.completionTime) : "—"}</span><span className="s">{finished ? "finished" : st.startTime ? "so far" : "not started"}</span></div>
          </div>
          <div className="bd sep">
            <dl className="kv">
              {scheduledAt && <><dt>Scheduled for</dt><dd>{new Date(scheduledAt).toLocaleString()}</dd></>}
              <dt>Created</dt><dd>{new Date(task.metadata.creationTimestamp).toLocaleString()}</dd>
              <dt>Started</dt><dd>{st.startTime ? new Date(st.startTime).toLocaleString() : <span className="dim">not yet</span>}</dd>
              <dt>Finished</dt><dd>{st.completionTime ? new Date(st.completionTime).toLocaleString() : <span className="dim">—</span>}</dd>
              <dt>Image</dt><dd><code>{st.image || spec.source?.image?.ref || (spec.fromApp ? `the image of ${spec.fromApp}` : "—")}</code></dd>
              {spec.command?.length ? <><dt>Command</dt><dd><code>{formatCommand(spec.command)}</code></dd></> : null}
              <dt>Size</dt><dd>{size ? `${size.label} · ${size.note}` : spec.size === "custom" ? "Custom" : spec.fromApp ? `Same as ${spec.fromApp}` : "Small"}</dd>
              <dt>Timeout</dt><dd>{spec.timeout ? prettyDuration(spec.timeout) : <span className="dim">none</span>}</dd>
              <dt>Retries</dt><dd>{spec.retries ?? 0}</dd>
              <dt>Time to stop</dt><dd>{spec.stopSeconds ?? 30} s</dd>
              {(spec.volumes?.length ?? 0) > 0 && <><dt>Volumes</dt><dd>{spec.volumes!.map(describeMount).join(", ")}</dd></>}
              <dt>On success</dt>
              <dd>
                {spec.onSuccess?.restart?.length ? <>restart {spec.onSuccess.restart.join(", ")}</> : <span className="dim">nothing</span>}
                {restarted && <span className="sub-line">{restarted.message}</span>}
              </dd>
              {st.job && <><dt>Job</dt><dd><code>{st.job}</code> · priority kwerft-batch</dd></>}
            </dl>
          </div>
        </div>

        <div className="card">
          <h3>Env overrides</h3>
          <EnvTable vars={spec.envOverrides ?? []} empty={`None: the run uses the ${schedule ? "schedule's" : spec.fromApp ? "app's" : "task's"} env as is.`} />
          {(spec.env?.length ?? 0) > 0 && (
            <>
              <h3 className="sep">Env</h3>
              <EnvTable vars={spec.env!} empty="" />
            </>
          )}
          {spec.fromApp && <p className="note dim pad">The app's own env applies first; then env, then the overrides.</p>}
        </div>
      </div>

      <div className="card">
        <h3>Logs</h3>
        <div className="bd"><TaskLogs task={task} /></div>
      </div>

      {dialog === "again" && again && (
        <RunNowDialog source={again} onClose={() => setDialog(undefined)}
          initialOverrides={(spec.envOverrides ?? []).filter((e) => e.value !== undefined).map((e) => ({ name: e.name, value: e.value ?? "" }))} />
      )}
      {dialog === "shell" && st.pod && (
        <Suspense fallback={null}>
          <ShellDialog onClose={() => setDialog(undefined)}
            target={{ project, task: name, pod: st.pod, containers: pods.data?.pods.find((p) => p.name === st.pod)?.containers.map((c) => c.name) ?? [] }} />
        </Suspense>
      )}
      {dialog === "cancel" && <CancelDialog task={task} onClose={() => setDialog(undefined)} onDone={(t) => { queryClient.setQueryData(key, t); void queryClient.invalidateQueries({ queryKey: ["tasks"] }); }} />}
      {dialog === "delete" && (
        <DeleteTaskDialog task={task} onClose={() => setDialog(undefined)}
          onDone={() => { void queryClient.invalidateQueries({ queryKey: ["tasks"] }); void navigate({ to: "/jobs", search: { project } }); }} />
      )}
    </section>
  );
}

function ShellButton({ pod, pods, onOpen }: { pod?: string; pods?: Replicas; onOpen: () => void }) {
  const up = pods?.pods.find((p) => p.name === pod)?.containers.some((c) => c.state === "running") ?? false;
  const why = !pods ? "Checking the run's pod…"
    : !pods.access.exec ? "Your role cannot open a shell."
    : !up ? "The run's container is not running yet."
    : `Shell in ${pod} (recorded)`;
  return <button className="btn" disabled={!pods?.access.exec || !up} title={why} onClick={onOpen}>Shell</button>;
}

function EnvTable({ vars, empty }: { vars: EnvVar[]; empty: string }) {
  if (vars.length === 0) return <div className="li empty-li">{empty}</div>;
  return (
    <div className="scroll-x">
      <table className="t">
        <tbody>
          {vars.map((e) => (
            <tr key={e.name}>
              <td className="mono">{e.name}</td>
              <td className="mono">{e.valueFrom ? <span className="tag">{e.valueFrom.secretKeyRef ? `secret · ${e.valueFrom.secretKeyRef.name}/${e.valueFrom.secretKeyRef.key}` : e.valueFrom.configMapKeyRef ? `config · ${e.valueFrom.configMapKeyRef.name}/${e.valueFrom.configMapKeyRef.key}` : `field · ${e.valueFrom.fieldRef?.fieldPath}`}</span> : e.value}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function CancelDialog({ task, onClose, onDone }: { task: Task; onClose: () => void; onDone: (t: Task) => void }) {
  const cancel = useMutation({
    mutationFn: () => jobs.cancelTask(task.metadata.namespace, task.metadata.name),
    onSuccess: (t) => { onDone(t); onClose(); },
  });
  return (
    <Dialog title={`Cancel ${task.metadata.name}?`} onClose={onClose} onSubmit={() => cancel.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Keep running</button>
        <button className="btn pri danger" disabled={cancel.isPending}>{cancel.isPending ? "Cancelling…" : "Cancel run"}</button>
      </>}>
      <p style={{ margin: 0 }}>Stops the run: its pod is terminated and the run is marked cancelled. The record stays; {task.spec.onSuccess?.restart?.length ? "no app is restarted." : "nothing is retried."}</p>
      {cancel.isError && <p className="form-error" role="alert">{errorText(cancel.error)}</p>}
    </Dialog>
  );
}

function DeleteTaskDialog({ task, onClose, onDone }: { task: Task; onClose: () => void; onDone: () => void }) {
  const del = useMutation({ mutationFn: () => jobs.deleteTask(task.metadata.namespace, task.metadata.name), onSuccess: onDone });
  const finished = ["Succeeded", "Failed"].includes(task.status?.phase ?? "");
  return (
    <Dialog title={`Delete ${task.metadata.name}?`} onClose={onClose} onSubmit={() => del.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri danger" disabled={del.isPending}>{del.isPending ? "Deleting…" : "Delete run"}</button>
      </>}>
      <p style={{ margin: 0 }}>{finished ? "Removes the run and its logs." : "Stops the run and removes it, with its logs."} It cannot be undone.</p>
      {del.isError && <p className="form-error" role="alert">{errorText(del.error)}</p>}
    </Dialog>
  );
}
