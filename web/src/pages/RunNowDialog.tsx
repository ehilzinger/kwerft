import { useState } from "react";
import { Link, useNavigate } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError } from "../api";
import { Dialog } from "../components/Dialog";
import { EnvRows, envOf, type EnvRow } from "../components/EnvRows";
import { Field } from "../components/Field";
import { jobs, type Task, type TaskSpec } from "../jobs";
import { workloads } from "../workloads";
import { errorText } from "./Apps";
import "../styles/jobs.css";

export type RunSource = { kind: "app"; project: string; app: string } | { kind: "schedule"; project: string; schedule: string };

export const DURATION_RE = /^(\d+(\.\d+)?(ms|s|m|h))+$/;

const key = (s: RunSource) => `${s.kind}:${s.project}/${s.kind === "app" ? s.app : s.schedule}`;
function parseKey(k: string): RunSource | undefined {
  const m = /^(app|schedule):([^/]+)\/(.+)$/.exec(k);
  if (!m) return undefined;
  return m[1] === "app" ? { kind: "app", project: m[2]!, app: m[3]! } : { kind: "schedule", project: m[2]!, schedule: m[3]! };
}

// "Run now": a one-off Task from an App (fromApp) or from a Schedule's
// template, with env overrides such as FORCE=1. It runs as the signed-in
// user and is recorded in the audit log. Without a source the dialog lets
// you pick one. On success it opens the new run.
export function RunNowDialog({ source, initialOverrides, onClose }: { source?: RunSource; initialOverrides?: EnvRow[]; onClose: () => void }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const picking = !source;
  const apps = useQuery({ queryKey: ["apps"], queryFn: () => workloads.apps(), enabled: picking });
  const schedules = useQuery({ queryKey: ["schedules"], queryFn: () => jobs.schedules(), enabled: picking });

  const [picked, setPicked] = useState(source ? key(source) : "");
  const src = source ?? parseKey(picked);
  const [rows, setRows] = useState<EnvRow[]>(initialOverrides?.length ? initialOverrides : [{ name: "", value: "" }]);
  const [timeout, setTimeout_] = useState("");
  const [retries, setRetries] = useState("0");
  const [restart, setRestart] = useState(false);
  const [error, setError] = useState<{ field?: string; message: string }>();

  const run = useMutation({
    mutationFn: (): Promise<Task> => {
      const envOverrides = envOf(rows).vars;
      if (src!.kind === "schedule") return jobs.runSchedule(src!.project, src!.schedule, envOverrides);
      const spec: TaskSpec = { fromApp: src!.app, envOverrides };
      if (timeout.trim()) spec.timeout = timeout.trim();
      if (Number(retries) > 0) spec.retries = Number(retries);
      if (restart) spec.onSuccess = { restart: [src!.app] };
      return jobs.createTask(src!.project, spec);
    },
    onSuccess: async (task) => {
      queryClient.setQueryData(["task", task.metadata.namespace, task.metadata.name], task);
      void queryClient.invalidateQueries({ queryKey: ["tasks"] });
      void queryClient.invalidateQueries({ queryKey: ["schedules"] });
      onClose();
      await navigate({ to: "/jobs/$project/tasks/$name", params: { project: task.metadata.namespace, name: task.metadata.name } });
    },
    onError: (e) => setError(e instanceof ApiError ? { field: e.field, message: e.message } : { message: errorText(e) }),
  });

  function submit() {
    if (!src) return setError({ field: "from", message: "Choose what to run." });
    const env = envOf(rows);
    if (env.bad !== undefined) return setError({ field: `envOverrides[${env.bad}].name`, message: "Use letters, digits, _, - and ., not starting with a digit." });
    if (src.kind === "app") {
      if (timeout.trim() && !DURATION_RE.test(timeout.trim())) return setError({ field: "spec.timeout", message: "A duration such as 30m, 1h or 1h30m." });
      const n = Number(retries);
      if (!Number.isInteger(n) || n < 0 || n > 10) return setError({ field: "spec.retries", message: "A whole number from 0 to 10." });
    }
    setError(undefined);
    run.mutate();
  }

  const rowError = /^(spec\.)?envOverrides\[(\d+)\]/.exec(error?.field ?? "");
  const known = rowError || ["from", "spec.timeout", "spec.retries"].includes(error?.field ?? "");
  const title = !src ? "Run a task" : src.kind === "app" ? `Run ${src.app} as a job` : `Run ${src.schedule} now`;

  return (
    <Dialog title={title} onClose={onClose} onSubmit={submit}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={run.isPending}>{run.isPending ? "Starting…" : "Run now"}</button>
      </>}>
      {picking && (
        <div className="field">
          <label htmlFor="run-from">From</label>
          <select id="run-from" className="input" value={picked} aria-invalid={error?.field === "from"} onChange={(e) => setPicked(e.target.value)}>
            <option value="">Choose a schedule or an app…</option>
            {(schedules.data?.length ?? 0) > 0 && (
              <optgroup label="Schedules">
                {schedules.data!.map((s) => <option key={`s-${s.project}/${s.name}`} value={`schedule:${s.project}/${s.name}`}>{s.project} / {s.name}</option>)}
              </optgroup>
            )}
            {(apps.data?.length ?? 0) > 0 && (
              <optgroup label="Apps">
                {apps.data!.map((a) => <option key={`a-${a.project}/${a.name}`} value={`app:${a.project}/${a.name}`}>{a.project} / {a.name}</option>)}
              </optgroup>
            )}
          </select>
          {error?.field === "from" ? <span className="field-error" role="alert">{error.message}</span> : null}
        </div>
      )}
      {src && (
        <p className="dim note">
          {src.kind === "app"
            ? <>Same image, command, env, size and volumes as <b>{src.app}</b>, in project {src.project}. Runs once to completion in the batch priority class.</>
            : <>Same image, command, env and volumes as the schedule <b>{src.schedule}</b>. Counts as one of its runs for concurrency and history.</>}
        </p>
      )}
      <div className="field">
        <label>Override env</label>
        <EnvRows rows={rows} onChange={setRows} label="Override" placeholder="FORCE"
          errorAt={rowError ? { index: Number(rowError[2]), message: error!.message } : undefined} />
        <span className="hint">Applied last, over the {src?.kind === "schedule" ? "schedule's" : "app's"} env. Shown with the run.</span>
      </div>
      {src?.kind === "app" && (
        <>
          <div className="fields">
            <Field id="run-timeout" label="Timeout" className="mono" value={timeout} onChange={(e) => setTimeout_(e.target.value)} placeholder="none"
              error={error?.field === "spec.timeout" ? error.message : undefined} hint="e.g. 30m or 2h" />
            <Field id="run-retries" label="Retries" type="number" min={0} max={10} value={retries} onChange={(e) => setRetries(e.target.value)}
              error={error?.field === "spec.retries" ? error.message : undefined} />
          </div>
          <label className="check"><input type="checkbox" checked={restart} onChange={(e) => setRestart(e.target.checked)} />Restart {src.app} once the task succeeds</label>
          <span className="hint">Needed regularly? <Link to="/jobs/new" search={{ project: src.project, fromApp: src.app }} onClick={onClose}>Run it on a schedule</Link>.</span>
        </>
      )}
      {error && !known && <p className="form-error" role="alert">{error.field ? <><code>{error.field}</code>: </> : null}{error.message}</p>}
      <span className="hint">Runs as you; recorded in the audit log.</span>
    </Dialog>
  );
}
