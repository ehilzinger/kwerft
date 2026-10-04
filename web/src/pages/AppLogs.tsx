import type { App } from "../workloads";

// Placeholder for the Logs tab of App detail. Live logs (VictoriaLogs, per
// replica, follow/search/download as in the blueprint) replace this
// component; it receives the App so it can select pods by the
// kwerft.dev/app=<name> label in namespace <project>.
export function AppLogs({ app }: { app: App }) {
  return (
    <div className="empty">
      <h2>Logs are coming soon</h2>
      <p>
        Live logs for every replica of <b>{app.metadata.name}</b>, with search and follow, arrive with log streaming. Until then:{" "}
        <code>kubectl logs -n {app.metadata.namespace} -l kwerft.dev/app={app.metadata.name} -f</code>
      </p>
    </div>
  );
}
