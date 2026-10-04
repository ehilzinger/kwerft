import { lazy, Suspense, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import type { App } from "../workloads";
import { age, cpuText, memText, podsApi, type Replica } from "../pods";
import "../styles/pods.css";

// xterm.js is large: load it with the first shell, not with the console.
const ShellDialog = lazy(() => import("./Terminal").then((m) => ({ default: m.ShellDialog })));

/** Query key shared by the replicas table and the Logs tab. */
export const podsKey = (project: string, app: string) => ["pods", project, app];

// The Replicas card of App detail: one row per pod, polled, with Logs and
// Shell. The buttons follow what Kubernetes RBAC allows the user (the server
// asks it); the endpoints enforce it anyway.
export function Replicas({ app, onLogs }: { app: App; onLogs: (pod: string) => void }) {
  const project = app.metadata.namespace;
  const name = app.metadata.name;
  const q = useQuery({ queryKey: podsKey(project, name), queryFn: () => podsApi.appPods(project, name), refetchInterval: 5000 });
  const [shell, setShell] = useState<Replica>();
  const pods = q.data?.pods ?? [];
  const access = q.data?.access ?? { logs: false, exec: false };
  const ready = pods.filter((p) => p.ready).length;

  return (
    <div className="card replicas" style={{ overflow: "hidden" }}>
      <h3>
        Replicas
        {q.data && pods.length > 0 && <span className="dim sm-note">{ready} of {pods.length} ready</span>}
      </h3>
      {q.isPending ? (
        <div className="li empty-li">Loading replicas…</div>
      ) : q.isError ? (
        <div className="li empty-li">Could not load replicas: {q.error.message}</div>
      ) : pods.length === 0 ? (
        <div className="li empty-li">
          {app.spec.replicas === 0 ? "No replicas: this app is scaled to 0." : "No replicas yet. They appear here once Kubernetes schedules them."}
        </div>
      ) : (
        <div className="scroll-x">
          <table className="t">
            <thead>
              <tr>
                <th>Pod</th><th>Node</th><th>Status</th><th className="num">Restarts</th><th className="num">Age</th>
                <th className="num" title={q.data.metrics ? undefined : "Needs metrics-server"}>CPU</th>
                <th className="num" title={q.data.metrics ? undefined : "Needs metrics-server"}>Memory</th>
                <th aria-label="Actions" />
              </tr>
            </thead>
            <tbody>
              {pods.map((p) => {
                const running = p.containers.some((c) => c.state === "running");
                const last = p.containers.find((c) => c.lastTermination)?.lastTermination;
                return (
                  <tr key={p.name}>
                    <td className="mono">{p.name}</td>
                    <td>{p.node || <span className="dim">not scheduled</span>}</td>
                    <td><span className={`pill ${p.tone}`} title={p.message}>{p.status}</span></td>
                    <td className="num" title={last ? `Last restart: ${last.reason} (exit code ${last.exitCode})` : undefined}>{p.restarts}</td>
                    <td className="num">{age(p.created)}</td>
                    <td className="num">{cpuText(p.cpuMillis)}</td>
                    <td className="num">{memText(p.memoryBytes)}</td>
                    <td className="row-acts">
                      <button type="button" className="btn sm" disabled={!access.logs}
                        title={access.logs ? `Logs of ${p.name}` : "Your role cannot read logs."} onClick={() => onLogs(p.name)}>Logs</button>
                      <button type="button" className="btn sm" disabled={!access.exec || !running}
                        title={!access.exec ? "Your role cannot open a shell." : !running ? "The container is not running." : `Shell in ${p.name} (recorded)`}
                        onClick={() => setShell(p)}>Shell</button>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
      {shell && (
        <Suspense fallback={null}>
          <ShellDialog target={{ project, app: name, pod: shell.name, containers: shell.containers.map((c) => c.name) }} onClose={() => setShell(undefined)} />
        </Suspense>
      )}
    </div>
  );
}
