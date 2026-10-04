import { useQuery } from "@tanstack/react-query";
import { LogViewer } from "../components/LogViewer";
import { podsKey } from "../components/Replicas";
import { appLogsPath, podsApi } from "../pods";
import type { App } from "../workloads";

// The Logs tab of App detail: live logs of every replica (or one, when
// opened from the Replicas table), streamed from Kubernetes as the user.
export function AppLogs({ app, pod }: { app: App; pod?: string }) {
  const project = app.metadata.namespace;
  const name = app.metadata.name;
  const replicas = useQuery({ queryKey: podsKey(project, name), queryFn: () => podsApi.appPods(project, name), refetchInterval: 10000 });
  return <LogViewer path={appLogsPath(project, name)} replicas={replicas.data?.pods} pod={pod} downloadName={`${project}-${name}`} />;
}
