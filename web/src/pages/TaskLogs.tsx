import type { Task } from "../jobs";

// Placeholder for the logs of one run in Task detail. The live LogViewer
// (built alongside the App logs) replaces this component: a Task's pods carry
// the label kwerft.dev/task=<name> in namespace <project>, and
// task.status.pod names the most recent one (a retry starts a new pod).
// Finished runs keep their pod, and so their logs, until the Task expires or
// its Schedule prunes it.
export function TaskLogs({ task }: { task: Task }) {
  const ns = task.metadata.namespace;
  const pod = task.status?.pod;
  return (
    <div className="logs-placeholder">
      <p>Live logs for this run arrive with log streaming. Until then:</p>
      <code>{pod ? `kubectl logs -n ${ns} ${pod}${task.status?.phase === "Running" ? " -f" : ""}` : `kubectl logs -n ${ns} -l kwerft.dev/task=${task.metadata.name}`}</code>
      {!pod && <p className="dim">No pod has started yet.</p>}
    </div>
  );
}
