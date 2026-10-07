// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { LogSearch } from "../components/LogSearch";
import { LogViewer } from "../components/LogViewer";
import { podsKey } from "../components/Replicas";
import { appLogsPath, podsApi } from "../pods";
import type { App } from "../workloads";
import "../styles/logsearch.css";

// The Logs tab of App detail: live logs of every replica (or one, when
// opened from the Replicas table), streamed from Kubernetes as the user; or
// the history in VictoriaLogs, which also has replicas that are gone
// (crashed, replaced by a rollout) and searches across all of them.
export function AppLogs({ app, pod }: { app: App; pod?: string }) {
  const project = app.metadata.namespace;
  const name = app.metadata.name;
  const [view, setView] = useState<"live" | "history">("live");
  useEffect(() => { if (pod) setView("live"); }, [pod]); // a replica's Logs button
  const replicas = useQuery({ queryKey: podsKey(project, name), queryFn: () => podsApi.appPods(project, name), refetchInterval: 10000, enabled: view === "live" });
  return (
    <>
      <div className="logsource">
        <div className="seg" role="group" aria-label="Log source">
          <button type="button" aria-pressed={view === "live"} onClick={() => setView("live")}>Live replicas</button>
          <button type="button" aria-pressed={view === "history"} onClick={() => setView("history")}>History</button>
        </div>
        <span className="dim">{view === "live" ? "Streamed from the running pods." : "Every replica's lines, also of pods that are gone · kept 14 days."}</span>
      </div>
      {view === "live"
        ? <LogViewer path={appLogsPath(project, name)} replicas={replicas.data?.pods} pod={pod} downloadName={`${project}-${name}`} />
        : <LogSearch fixed={{ project, app: name }} />}
    </>
  );
}
