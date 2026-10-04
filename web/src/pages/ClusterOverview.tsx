import { useState } from "react";
import { Link, getRouteApi, useNavigate } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../api";
import { Dialog } from "../components/Dialog";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { clusterState, clustersApi, clustersKey, nodesText, providerLabel, type AgentInstall, type Cluster } from "../clusters";
import { ago } from "../workloads";
import { ClusterLayout } from "./Clusters";
import { InstallCommand } from "./ClustersList";

const route = getRouteApi("/authed/clusters/$name");
const POLL = 5000;
const unreachable = "The console could not be reached. Check your connection and try again.";
const errText = (e: unknown) => (e instanceof ApiError ? e.message : unreachable);
const when = (iso?: string) => (iso ? new Date(iso).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" }) : "—");

// A cluster › Overview: health, agent, versions, node pools; rotate the
// agent token, delete (W3, Phase 5).
export function ClusterOverview() {
  const { name } = route.useParams();
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const manage = session.data?.role === "owner" || session.data?.role === "admin";
  const q = useQuery({ queryKey: [...clustersKey, name], queryFn: () => clustersApi.get(name), refetchInterval: POLL, enabled: manage });
  const [dialog, setDialog] = useState<"delete" | "rotate">();
  const c = q.data;
  const remote = c && c.provider !== "local";

  const actions = c && remote && manage && !c.deleting ? (
    <>
      <button className="btn" onClick={() => setDialog("rotate")}><Icon name="key" />Rotate agent token</button>
      <button className="btn danger" onClick={() => setDialog("delete")}><Icon name="trash" />Delete</button>
    </>
  ) : undefined;

  return (
    <ClusterLayout cluster={name} current="overview" actions={actions}>
      {session.data && !manage ? (
        <div className="empty"><h2>Owners and admins manage clusters</h2></div>
      ) : q.isError ? (
        <div className="banner bad" role="alert"><Icon name="alert" /><span>{errText(q.error)}</span></div>
      ) : !c ? (
        <p className="loading">Loading cluster…</p>
      ) : (
        <Overview c={c} />
      )}
      {c && dialog === "delete" && <DeleteDialog cluster={c} onClose={() => setDialog(undefined)} />}
      {c && dialog === "rotate" && <RotateDialog cluster={c} onClose={() => setDialog(undefined)} />}
    </ClusterLayout>
  );
}

function Overview({ c }: { c: Cluster }) {
  const state = clusterState(c);
  const local = c.provider === "local";
  return (
    <>
      {!c.connected && c.message && (
        <div className={`banner ${state.tone === "bad" ? "bad" : "info"}`} role="status">
          <Icon name={state.tone === "bad" ? "alert" : "clock"} /><span>{c.message}</span>
        </div>
      )}
      {c.connected && c.agent?.error && (
        <div className="banner warn" role="status"><Icon name="alert" /><span>The agent reports: {c.agent.error}</span></div>
      )}
      <div className="g2">
        <div className="card">
          <h3>Health</h3>
          <div className="stats">
            <div><span className="k">State</span><span className="v" style={{ fontSize: 16 }}><span className={`pill ${state.tone}`}>{state.label}</span></span>
              <span className="s">{local ? "the console runs here" : c.connected ? `since ${ago(c.agent?.since)}` : c.lastSeen ? `last seen ${ago(c.lastSeen)}` : "never connected"}</span></div>
            <div><span className="k">Nodes</span><span className="v">{c.lastSeen || c.connected ? c.readyNodes : "—"}<small>{c.lastSeen || c.connected ? ` / ${c.nodes}` : ""}</small></span><span className="s">ready</span></div>
            <div><span className="k">Kubernetes</span><span className="v" style={{ fontSize: 16 }}>{c.kubernetesVersion || "—"}</span><span className="s">version</span></div>
          </div>
          <div className="bd sep">
            <dl className="kv">
              <dt>Provider</dt><dd>{providerLabel[c.provider] ?? c.provider}</dd>
              {c.hetznerCloud && (
                <>
                  <dt>Control plane</dt>
                  <dd>{c.hetznerCloud.controlPlanes === 3 ? "3 servers (highly available)" : "1 server"} · <code>{c.hetznerCloud.serverType}</code> in {c.hetznerCloud.location}</dd>
                </>
              )}
              <dt>Kwerft</dt><dd>{c.agentVersion ? <code>{c.agentVersion}</code> : <span className="dim">—</span>}{!local && " (agent)"}</dd>
              {!local && (
                <>
                  <dt>Agent</dt>
                  <dd>{c.agent ? <>connected from <code>{c.agent.remote}</code> since {when(c.agent.since)}</> : <span className="dim">not connected</span>}</dd>
                  <dt>Last seen</dt><dd>{c.connected ? "now" : when(c.lastSeen)}</dd>
                  <dt>Agent token</dt><dd>{c.hasToken ? "set (never shown again; rotate it for a new install command)" : <span className="dim">none: rotate it to get an install command</span>}</dd>
                </>
              )}
              <dt>Added</dt><dd>{when(c.createdAt)}</dd>
            </dl>
          </div>
        </div>

        <div className="card">
          <h3>Nodes</h3>
          <div className="bd">
            <p className="note">{nodesText(c) === "—" ? "No report yet." : `${c.nodes} node${c.nodes === 1 ? "" : "s"}, ${c.readyNodes} ready.`}</p>
            {c.pools && c.pools.length > 0 ? (
              <ul className="rows">
                {c.pools.map((p) => (
                  <li key={p.name}>
                    <div className="row">
                      <Icon name="server" />
                      <div className="grow">
                        <b>{p.name}</b> <span className="pill info nodot">{p.role}</span>
                        <small>{p.count} × <code>{p.serverType}</code> in {p.location} · {p.readyNodes} ready</small>
                      </div>
                    </div>
                  </li>
                ))}
              </ul>
            ) : (
              <p className="dim note">{c.provider === "hetzner-cloud" ? "The control-plane pool appears in a moment." : "No Cloud node pools."}</p>
            )}
            <p className="note"><Link to="/clusters/$name/nodes" params={{ name: c.name }}>Nodes and node pools</Link></p>
          </div>
        </div>
      </div>
    </>
  );
}

function DeleteDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }) {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [typed, setTyped] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function remove() {
    if (typed !== c.name) return setError(`Type ${c.name} to confirm.`);
    setBusy(true);
    setError(undefined);
    try {
      await clustersApi.remove(c.name);
      await queryClient.invalidateQueries({ queryKey: clustersKey });
      onClose();
      void navigate({ to: "/clusters" });
    } catch (e) {
      setError(errText(e));
      setBusy(false);
    }
  }

  return (
    <Dialog title={`Delete ${c.name}?`} onClose={onClose} onSubmit={remove}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri danger" disabled={busy || typed !== c.name}>{busy ? "Deleting…" : "Delete cluster"}</button>
      </>}>
      {c.provider === "hetzner-cloud" ? (
        <p className="note">
          Kwerft deletes every Cloud server of this cluster, and with them all projects, apps and data in it. Its agent is disconnected and its
          token revoked. This cannot be undone.
        </p>
      ) : (
        <p className="note">
          The console forgets this cluster: its agent is disconnected and its token revoked. Kwerft deletes the Cloud servers it created for
          the cluster's node pools; other servers and everything running on them stay as they are, without a console.
        </p>
      )}
      <Field id="del-confirm" label={`Type ${c.name} to confirm`} className="mono" value={typed} onChange={(e) => setTyped(e.target.value)}
        autoComplete="off" spellCheck={false} autoFocus />
      {error && <p className="form-error" role="alert">{error}</p>}
    </Dialog>
  );
}

function RotateDialog({ cluster: c, onClose }: { cluster: Cluster; onClose: () => void }) {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [issued, setIssued] = useState<AgentInstall>();

  async function rotate() {
    if (issued) return onClose();
    setBusy(true);
    setError(undefined);
    try {
      setIssued(await clustersApi.rotateToken(c.name));
      void queryClient.invalidateQueries({ queryKey: clustersKey });
    } catch (e) {
      setError(errText(e));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog wide title={issued ? `New agent token for ${c.name}` : `Rotate the agent token of ${c.name}?`} onClose={onClose} onSubmit={rotate}
      actions={issued ? <button className="btn pri">Done</button> : <>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri danger" disabled={busy}>{busy ? "Rotating…" : "Rotate token"}</button>
      </>}>
      {issued ? (
        <>
          <p className="note">The old token no longer works. Run this on one of the cluster's servers so its agent connects with the new one.</p>
          <InstallCommand install={issued} />
        </>
      ) : (
        <p className="note">
          The current token stops working at once and the agent disconnects; projects in this cluster are unreachable from the console until
          the agent runs with the new token. Rotate when the token may have leaked.
        </p>
      )}
      {error && <p className="form-error" role="alert">{error}</p>}
    </Dialog>
  );
}
