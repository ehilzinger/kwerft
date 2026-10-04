import { useQuery } from "@tanstack/react-query";
import { api } from "../api";

export function Overview() {
  const version = useQuery({ queryKey: ["version"], queryFn: api.version, staleTime: Infinity });

  return (
    <section className="view">
      <div className="ph">
        <div>
          <h1>Overview</h1>
          <p>Cluster health, capacity and what needs attention.</p>
        </div>
      </div>
      <div className="tiles">
        <div className="tile">
          <span className="k">Console</span>
          <span className="v">{version.data?.version ?? "—"}</span>
          <span className="s">
            {version.isError ? <span className="pill bad">API unreachable</span> : <span className="pill ok">Running</span>}
          </span>
        </div>
        <div className="tile">
          <span className="k">Platform</span>
          <span className="v">{version.data ? (version.data.platform === "cloud" ? "Hetzner Cloud" : "Dedicated") : "—"}</span>
          <span className="s">Detected by the installer</span>
        </div>
      </div>
      <div className="empty">
        <h2>Capacity and alerts arrive with monitoring</h2>
        <p>CPU, memory, storage and traffic tiles, the usage chart and the “needs attention” feed are built in Phase 3, once VictoriaMetrics is wired in.</p>
      </div>
    </section>
  );
}
