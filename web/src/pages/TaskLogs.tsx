// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@tanstack/react-query";
import type { Task } from "../jobs";
import { podsApi, taskLogsPath } from "../pods";
import { LogViewer } from "../components/LogViewer";

// The logs of one run in Task detail. A retry starts a new pod, so the
// replica selector lists every pod of the Task; finished runs keep their pod
// (and logs) until the Task expires or its Schedule prunes it.
export function TaskLogs({ task }: { task: Task }) {
  const project = task.metadata.namespace;
  const name = task.metadata.name;
  const running = task.status?.phase === "Pending" || task.status?.phase === "Running";
  const pods = useQuery({
    queryKey: ["task-pods", project, name],
    queryFn: () => podsApi.taskPods(project, name),
    refetchInterval: running ? 5000 : false,
  });

  if (!task.status?.pod) return <p className="dim">No pod has started yet.</p>;
  return (
    <LogViewer
      path={taskLogsPath(project, name)}
      replicas={pods.data?.pods}
      pod={task.status.pod}
      follow={running}
      downloadName={name}
    />
  );
}
