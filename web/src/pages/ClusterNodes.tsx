import { useState } from "react";
import { getRouteApi } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError } from "../api";
import { CopyButton } from "../components/CopyButton";
import { Dialog } from "../components/Dialog";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import {
  catalogKey, controlPlaneCountProblem, labelsText, nodeTone, nodesApi, nodesKey, parseLabels, phaseTone, poolSummary, priceText, roleText,
  serverTypeText, type ClusterNode, type ClusterNodes as Nodes, type JoinCommand, type Pool, type PoolRole, type PoolServer,
} from "../nodes";
import { ClusterLayout } from "./Clusters";
import { errorText } from "./Apps";
import "../styles/firewall.css";
import "../styles/nodes.css";

const route = getRouteApi("/authed/clusters/$name/nodes");

// A cluster › Nodes: node pools of Hetzner Cloud servers, the cluster's
// nodes, and the join command for dedicated servers (W2, Phase 5). Every
// change is made as the signed-in user; the reconcilers create, drain and
// delete the servers.
export function ClusterNodes() {
  const { name } = route.useParams();
  const q = useQuery({
    queryKey: nodesKey(name), queryFn: () => nodesApi.get(name), retry: false,
    refetchInterval: (query) => {
      const d = query.state.data;
      const moving = d?.pools.some((p) => p.state !== "ready" || p.deleting) || d?.nodes.some((n) => n.status !== "Ready");
      return moving ? 4000 : 15000;
    },
  });
  const [dialog, setDialog] = useState<
    | { kind: "add" }
    | { kind: "scale"; pool: Pool }
    | { kind: "delete"; pool: Pool }
    | { kind: "remove"; node: ClusterNode }
    | { kind: "server"; pool: Pool; server: PoolServer }
  >();

  const actions = q.data && (
    <>
      {q.data.cloud && <button className="btn sm pri" onClick={() => setDialog({ kind: "add" })}><Icon name="plus" />Add node pool</button>}
    </>
  );
  let body;
  if (q.isPending) body = <p className="loading">Loading nodes…</p>;
  else if (q.isError) {
    body = q.error instanceof ApiError && q.error.status === 403
      ? <div className="empty"><h2>Owners and admins manage nodes</h2><p>Servers, node pools and joining new servers to the cluster.</p></div>
      : <div className="banner bad" role="alert"><Icon name="alert" /><span>{errorText(q.error)}</span></div>;
  } else {
    const d = q.data;
    body = (
      <>
        {!d.reachable && <div className="banner warn" role="status"><Icon name="alert" /><span>{d.problem ?? "The cluster cannot be reached right now."} Node pools keep working; nodes show again once it answers.</span></div>}
        <Pools data={d} onScale={(pool) => setDialog({ kind: "scale", pool })} onDelete={(pool) => setDialog({ kind: "delete", pool })}
          onRemoveServer={(pool, server) => setDialog({ kind: "server", pool, server })} onAdd={() => setDialog({ kind: "add" })} />
        <NodesTable data={d} onRemove={(node) => setDialog({ kind: "remove", node })} />
        <JoinCard data={d} />
        {dialog?.kind === "add" && <PoolDialog data={d} onClose={() => setDialog(undefined)} />}
        {dialog?.kind === "scale" && <PoolDialog data={d} edit={dialog.pool} onClose={() => setDialog(undefined)} />}
        {dialog?.kind === "delete" && <DeletePoolDialog cluster={name} pool={dialog.pool} onClose={() => setDialog(undefined)} />}
        {dialog?.kind === "remove" && <RemoveNodeDialog cluster={name} node={dialog.node} onClose={() => setDialog(undefined)} />}
        {dialog?.kind === "server" && <RemoveServerDialog cluster={name} pool={dialog.pool} server={dialog.server} onClose={() => setDialog(undefined)} />}
      </>
    );
  }
  return <ClusterLayout cluster={name} current="nodes" actions={actions}>{body}</ClusterLayout>;
}

// ---- pools ---------------------------------------------------------------------------

function Pools({ data, onScale, onDelete, onRemoveServer, onAdd }: {
  data: Nodes; onScale: (p: Pool) => void; onDelete: (p: Pool) => void; onRemoveServer: (p: Pool, s: PoolServer) => void; onAdd: () => void;
}) {
  if (data.pools.length === 0) {
    if (!data.cloud) {
      return (
        <div className="empty nodes-empty">
          <h2>No node pools</h2>
          <p>Node pools are Hetzner Cloud servers Kwerft creates, joins and replaces. Add a Hetzner Cloud API token under Settings › Hetzner Cloud to use them, or join your own servers below.</p>
        </div>
      );
    }
    return (
      <div className="empty nodes-empty">
        <h2>No node pools yet</h2>
        <p>A pool is a group of Hetzner Cloud servers of one type that Kwerft keeps joined to this cluster: workers for apps, three control-plane servers for a highly available control plane, or build servers that start when a build runs.</p>
        <div className="acts"><button className="btn pri" onClick={onAdd}><Icon name="plus" />Add node pool</button></div>
      </div>
    );
  }
  return (
    <section className="nodes-pools" aria-label="Node pools">
      {data.pools.map((p) => (
        <div className="card" key={p.name}>
          <div className="pool-h">
            <div>
              <h2>{p.pool} <span className="pill mute nodot">{roleText[p.role] ?? p.role}</span></h2>
              <span className="sub-line">{p.count} × {p.serverType} in {p.location}{p.role === "builds" && p.scaleDownAfterMinutes ? ` · idle after ${p.scaleDownAfterMinutes} min` : ""}
                {Object.keys(p.labels).length > 0 && <> · <span className="mono">{Object.entries(p.labels).map(([k, v]) => `${k}=${v}`).join(", ")}</span></>}</span>
            </div>
            <div className="acts">
              <span className={`pill ${p.state === "ready" ? "ok" : p.state === "failed" ? "bad" : "warn"}`}>{poolSummary(p)}</span>
              {!p.deleting && <button className="btn sm" onClick={() => onScale(p)}><Icon name="scale" />Scale</button>}
              {!p.deleting && <button className="btn sm danger" onClick={() => onDelete(p)} aria-label={`Delete pool ${p.pool}`} title="Delete pool"><Icon name="trash" /></button>}
            </div>
          </div>
          {p.message && p.state !== "ready" && <p className={`note pool-msg ${p.state === "failed" ? "form-error" : "dim"}`}>{p.message}</p>}
          {p.servers.length > 0 && (
            <div className="scroll-x">
              <table className="t fw-table">
                <thead><tr><th>Server</th><th>Type</th><th>Status</th><th>Private IP</th><th>Public IP</th><th><span className="sr">Actions</span></th></tr></thead>
                <tbody>
                  {p.servers.map((s) => (
                    <tr key={s.name}>
                      <td className="nm mono">{s.name}</td>
                      <td>{s.serverType ?? "—"}{s.serverType && s.serverType !== p.serverType && <span className="sub-line">being replaced</span>}</td>
                      <td><span className={`pill ${phaseTone(s.phase)}`}>{s.phase}</span>{s.message && <span className="sub-line">{s.message}</span>}</td>
                      <td className="mono">{s.privateIp ?? "—"}</td>
                      <td className="mono">{s.publicIp ?? "—"}</td>
                      <td className="row-acts">
                        {(s.phase === "Failed" || s.phase === "Joining") && !data.nodes.some((n) => n.name === s.name) && (
                          <button className="btn sm" onClick={() => onRemoveServer(p, s)}>Replace</button>
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          )}
        </div>
      ))}
    </section>
  );
}

// ---- nodes ---------------------------------------------------------------------------

function NodesTable({ data, onRemove }: { data: Nodes; onRemove: (n: ClusterNode) => void }) {
  const qc = useQueryClient();
  const [error, setError] = useState<string>();
  const act = useMutation({
    mutationFn: ({ node, op }: { node: string; op: "drain" | "uncordon" }) => op === "drain" ? nodesApi.drain(data.cluster, node) : nodesApi.uncordon(data.cluster, node),
    onSuccess: () => { setError(undefined); void qc.invalidateQueries({ queryKey: nodesKey(data.cluster) }); },
    onError: (e) => setError(errorText(e)),
  });
  if (!data.reachable && data.nodes.length === 0) return null;
  return (
    <section aria-label="Nodes">
      <h2 className="nodes-h">Nodes <span className="dim">{data.nodes.filter((n) => n.ready).length} of {data.nodes.length} ready · {data.controlPlanes} control plane</span></h2>
      {error && <p className="form-error" role="alert">{error}</p>}
      <div className="card scroll-x">
        <table className="t fw-table">
          <thead><tr><th>Node</th><th>Role</th><th>Status</th><th>Internal IP</th><th>External IP</th><th>Size</th><th>Pool</th><th><span className="sr">Actions</span></th></tr></thead>
          <tbody>
            {data.nodes.map((n) => {
              const pool = data.pools.find((p) => p.name === n.pool);
              const busy = n.status === "Removing";
              return (
                <tr key={n.name}>
                  <td className="nm mono">{n.name}{n.kubeletVersion && <span className="sub-line">{n.kubeletVersion}{n.platform ? ` · ${n.platform}` : ""}</span>}</td>
                  <td>{n.roles.map((r) => roleText[r] ?? r).join(", ")}</td>
                  <td><span className={`pill ${nodeTone(n)}`}>{n.ready ? n.status : "NotReady"}</span>{n.message && <span className="sub-line">{n.message}</span>}</td>
                  <td className="mono">{n.internalIp ?? "—"}</td>
                  <td className="mono">{n.externalIp ?? "—"}</td>
                  <td>{n.cpu ? `${n.cpu} CPU` : ""}{n.memory ? ` · ${n.memory}` : ""}</td>
                  <td>{pool ? pool.pool : n.pool ?? <span className="dim">—</span>}</td>
                  <td className="row-acts">
                    {!busy && (n.unschedulable || n.status === "Draining" || n.status === "Drained"
                      ? <button className="btn sm" disabled={act.isPending} onClick={() => act.mutate({ node: n.name, op: "uncordon" })}>Uncordon</button>
                      : <button className="btn sm" disabled={act.isPending} onClick={() => act.mutate({ node: n.name, op: "drain" })}>Drain</button>)}
                    {!busy && <button className="btn sm danger" onClick={() => onRemove(n)}>Remove</button>}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </section>
  );
}

// ---- join ----------------------------------------------------------------------------

function JoinCard({ data }: { data: Nodes }) {
  const [role, setRole] = useState<"worker" | "control-plane">("worker");
  const [ttl, setTTL] = useState(60);
  const [cmd, setCmd] = useState<JoinCommand>();
  const create = useMutation({ mutationFn: () => nodesApi.joinCommand(data.cluster, role, ttl), onSuccess: setCmd });
  return (
    <section className="card join-card" aria-labelledby="join-h">
      <h2 id="join-h">Join a server</h2>
      <div className="bd">
        <p className="dim note">For dedicated servers (and any Ubuntu 22.04, 24.04 or 26.04 server) on the cluster's private network: a Hetzner vSwitch coupled to the Cloud Network, or the same Cloud Network. Run the command on the server as root; it installs k3s with Kwerft's settings and joins.</p>
        {!data.joinable ? (
          <p className="note warn-text">This cluster has no join material yet: its first server publishes it once it is running.</p>
        ) : (
          <>
            <div className="join-form">
              <div className="seg" role="group" aria-label="Role">
                <button type="button" aria-pressed={role === "worker"} onClick={() => { setRole("worker"); setCmd(undefined); }}>Worker</button>
                <button type="button" aria-pressed={role === "control-plane"} onClick={() => { setRole("control-plane"); setCmd(undefined); }}>Control plane</button>
              </div>
              <label className="join-ttl">Valid for{" "}
                <select className="input" value={ttl} onChange={(e) => { setTTL(Number(e.target.value)); setCmd(undefined); }}>
                  <option value={30}>30 minutes</option>
                  <option value={60}>1 hour</option>
                  <option value={240}>4 hours</option>
                  <option value={1440}>24 hours</option>
                </select>
              </label>
              <button className="btn pri sm" disabled={create.isPending} onClick={() => create.mutate()}>{create.isPending ? "Creating…" : "Create join command"}</button>
            </div>
            {role === "control-plane" && (
              <p className="note warn-text">
                Control-plane servers run etcd: keep an odd number ({data.controlPlanes} now{controlPlaneCountProblem(data.controlPlanes, 1) ? `, ${data.controlPlanes + 1} after this one — join one more` : ""}). Only owners can create this command; the server receives the cluster's k3s server token.
              </p>
            )}
            {create.isError && <p className="form-error" role="alert">{errorText(create.error)}</p>}
            {cmd && (
              <div className="join-cmd">
                <code className="mono">{cmd.command}</code>
                <div className="acts">
                  <CopyButton text={cmd.command} />
                  <span className="dim note">Works for {cmd.role === "worker" ? "any number of servers" : "control-plane servers"} until {new Date(cmd.expiresAt).toLocaleString()}. It holds no Kubernetes credential; the console hands one out to the server when it joins.</span>
                </div>
              </div>
            )}
          </>
        )}
      </div>
    </section>
  );
}

// ---- dialogs -------------------------------------------------------------------------

type PoolForm = { name: string; role: PoolRole; location: string; serverType: string; count: string; labels: string; idle: string };

function PoolDialog({ data, edit, onClose }: { data: Nodes; edit?: Pool; onClose: () => void }) {
  const qc = useQueryClient();
  const cat = useQuery({ queryKey: catalogKey, queryFn: nodesApi.catalog, staleTime: 10 * 60_000, retry: false });
  const [f, setF] = useState<PoolForm>(() => ({
    name: edit?.pool ?? "", role: edit?.role ?? "worker", location: edit?.location ?? "", serverType: edit?.serverType ?? "",
    count: String(edit?.count ?? 1), labels: labelsText(edit?.labels ?? {}), idle: String(edit?.scaleDownAfterMinutes ?? 15),
  }));
  const [error, setError] = useState<{ field?: string; message: string }>();
  const set = <K extends keyof PoolForm>(k: K, v: PoolForm[K]) => {
    setF((x) => ({ ...x, [k]: v }));
    setError((e) => (e?.field === k ? undefined : e));
  };
  const locations = cat.data?.locations ?? [];
  const location = f.location || (locations.find((l) => l.name === "fsn1") ?? locations[0])?.name || "";
  const types = (cat.data?.serverTypes ?? []).filter((t) => t.locations.includes(location));
  const type = types.find((t) => t.name === f.serverType);
  // Control-plane nodes outside this pool (its own ones are counted by the server).
  const own = edit ? data.nodes.filter((n) => n.pool === edit.name && n.roles.includes("control-plane")).length : 0;
  const save = useMutation({
    mutationFn: () => {
      const parsed = parseLabels(f.labels);
      const count = Number(f.count);
      const idle = f.role === "builds" ? Number(f.idle) : undefined;
      return edit
        ? nodesApi.updatePool(data.cluster, edit.name, { count, serverType: f.serverType !== edit.serverType ? f.serverType : undefined, labels: parsed.labels,
          scaleDownAfterMinutes: f.role === "builds" ? idle : undefined })
        : nodesApi.createPool(data.cluster, { name: f.name.trim(), role: f.role, location, serverType: f.serverType, count, labels: parsed.labels, scaleDownAfterMinutes: idle });
    },
    onSuccess: () => { void qc.invalidateQueries({ queryKey: nodesKey(data.cluster) }); onClose(); },
    onError: (e) => setError(e instanceof ApiError ? { field: e.field, message: e.message } : { message: errorText(e) }),
  });
  const err = (field: string) => (error?.field === field ? error.message : undefined);
  const submit = () => {
    const count = Number(f.count);
    if (!Number.isInteger(count) || count < 0 || count > 50) return setError({ field: "count", message: "Between 0 and 50 servers." });
    if (!f.serverType) return setError({ field: "serverType", message: "Choose a server type." });
    const labels = parseLabels(f.labels);
    if (labels.error) return setError({ field: "labels", message: labels.error });
    if (f.role === "control-plane") {
      const p = controlPlaneCountProblem(data.controlPlanes - own, count);
      if (p) return setError({ field: "count", message: p });
    }
    setError(undefined);
    save.mutate();
  };
  return (
    <Dialog wide title={edit ? `Scale ${edit.pool}` : "Add node pool"} onClose={onClose} onSubmit={submit}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={save.isPending}>{save.isPending ? "Saving…" : edit ? "Save" : "Create pool"}</button>
      </>}>
      {!edit && (
        <>
          <Field id="pool-name" label="Name" className="mono" value={f.name} onChange={(e) => set("name", e.target.value.toLowerCase())} placeholder="workers"
            autoComplete="off" spellCheck={false} error={err("name")} hint="Lowercase letters, digits and dashes. Servers are named after the cluster and pool." />
          <div className="field">
            <label id="pool-role">Role</label>
            <div className="seg" role="group" aria-labelledby="pool-role">
              {(["worker", "control-plane", "builds"] as const).map((r) => (
                <button type="button" key={r} aria-pressed={f.role === r} onClick={() => set("role", r)}>{r === "worker" ? "Workers" : r === "control-plane" ? "Control plane" : "Builds"}</button>
              ))}
            </div>
            <span className="hint">
              {f.role === "worker" && "Apps run here. Nodes that stay NotReady for 15 minutes are replaced."}
              {f.role === "control-plane" && `k3s servers with embedded etcd. The cluster keeps an odd number of them (it has ${data.controlPlanes}); for high availability, three in all.`}
              {f.role === "builds" && "Only Git builds run here (the nodes are tainted). The pool starts servers when a build is queued and stops them when builds are idle; the count is the most it starts."}
            </span>
          </div>
        </>
      )}
      {cat.isError && <p className="form-error" role="alert">{errorText(cat.error)}</p>}
      <div className="fields nodes-fields">
        {!edit ? (
          <div className="field">
            <label htmlFor="pool-loc">Location</label>
            <select id="pool-loc" className="input" value={location} onChange={(e) => set("location", e.target.value)} aria-invalid={!!err("location")}>
              {locations.map((l) => <option key={l.name} value={l.name}>{l.name} — {l.city} ({l.networkZone})</option>)}
            </select>
            <span className="hint">All servers of a cluster share one network zone.</span>
          </div>
        ) : <div className="field"><label>Location</label><span className="mono">{edit.location}</span></div>}
        <div className="field">
          <label htmlFor="pool-type">Server type</label>
          <select id="pool-type" className="input" value={f.serverType} onChange={(e) => set("serverType", e.target.value)} aria-invalid={!!err("serverType")}
            aria-describedby="pool-type-note">
            <option value="">{cat.isPending ? "Loading…" : "Choose…"}</option>
            {types.map((t) => <option key={t.name} value={t.name}>{serverTypeText(t)}</option>)}
            {edit && !types.some((t) => t.name === edit.serverType) && <option value={edit.serverType}>{edit.serverType}</option>}
          </select>
          {err("serverType") ? <span id="pool-type-note" className="field-error" role="alert">{err("serverType")}</span>
            : <span id="pool-type-note" className="hint">{type ? priceText(type, location) + " per server." : "At least 4 GB; 8 GB is comfortable."}{edit && " Changing it replaces the servers one at a time."}</span>}
        </div>
      </div>
      <div className="fields nodes-fields">
        <Field id="pool-count" label={f.role === "builds" ? "At most" : "Servers"} inputMode="numeric" value={f.count} onChange={(e) => set("count", e.target.value.trim())}
          error={err("count")} hint={type && Number(f.count) > 0 ? `${priceText(type, location).replace("/month", "")} × ${f.count} per month at most.` : undefined} />
        {f.role === "builds" && (
          <Field id="pool-idle" label="Stop after idle (minutes)" inputMode="numeric" value={f.idle} onChange={(e) => set("idle", e.target.value.trim())}
            error={err("scaleDownAfterMinutes")} />
        )}
      </div>
      <div className="field">
        <label htmlFor="pool-labels">Node labels</label>
        <textarea id="pool-labels" className="input mono" rows={2} value={f.labels} onChange={(e) => set("labels", e.target.value)} spellCheck={false}
          placeholder="disk=nvme" aria-invalid={!!err("labels")} aria-describedby="pool-labels-note" />
        {err("labels") ? <span id="pool-labels-note" className="field-error" role="alert">{err("labels")}</span>
          : <span id="pool-labels-note" className="hint">key=value, one per line (optional).</span>}
      </div>
      {edit && Number(f.count) < edit.count && (
        <p className="note warn-text">{edit.count - Number(f.count)} server(s) will be drained (their pods move elsewhere, respecting disruption budgets for up to 15 minutes), removed from the cluster and deleted.</p>
      )}
      {error && !["name", "count", "serverType", "labels", "location", "scaleDownAfterMinutes"].includes(error.field ?? "") && <p className="form-error" role="alert">{error.message}</p>}
    </Dialog>
  );
}

function DeletePoolDialog({ cluster, pool, onClose }: { cluster: string; pool: Pool; onClose: () => void }) {
  const qc = useQueryClient();
  const del = useMutation({
    mutationFn: () => nodesApi.deletePool(cluster, pool.name),
    onSuccess: () => { void qc.invalidateQueries({ queryKey: nodesKey(cluster) }); onClose(); },
  });
  return (
    <Dialog title={`Delete pool ${pool.pool}?`} onClose={onClose} onSubmit={() => del.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri danger" disabled={del.isPending}>{del.isPending ? "Deleting…" : "Delete pool"}</button>
      </>}>
      <p>Its {pool.servers.length} server(s) are drained one by one, removed from the cluster and deleted at Hetzner. Pods move to other nodes; data on local volumes of these servers is lost.</p>
      {del.isError && <p className="form-error" role="alert">{errorText(del.error)}</p>}
    </Dialog>
  );
}

function RemoveNodeDialog({ cluster, node, onClose }: { cluster: string; node: ClusterNode; onClose: () => void }) {
  const qc = useQueryClient();
  const [force, setForce] = useState(false);
  const rm = useMutation({
    mutationFn: () => nodesApi.remove(cluster, node.name, force),
    onSuccess: () => { void qc.invalidateQueries({ queryKey: nodesKey(cluster) }); onClose(); },
  });
  const cp = node.roles.includes("control-plane");
  return (
    <Dialog title={`Remove ${node.name}?`} onClose={onClose} onSubmit={() => rm.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri danger" disabled={rm.isPending}>{rm.isPending ? "Removing…" : "Drain and remove"}</button>
      </>}>
      <p>Kwerft cordons the node and moves its pods elsewhere, respecting disruption budgets for up to 15 minutes{cp ? ", takes it out of etcd" : ""}, then removes it from the cluster.</p>
      {node.pool
        ? <p className="dim note">It belongs to a node pool: its Cloud server is deleted and the pool creates a new one.</p>
        : <p className="dim note">Then stop k3s on the server (<code>install.sh --uninstall</code>), or it registers again.</p>}
      {cp && <p className="note warn-text">A control-plane node is only removed while every other one is Ready, and never the last one.</p>}
      <label className="check"><input type="checkbox" checked={force} onChange={(e) => setForce(e.target.checked)} />Remove even if it holds local volumes (their data is lost)</label>
      {rm.isError && <p className="form-error" role="alert">{errorText(rm.error)}</p>}
    </Dialog>
  );
}

function RemoveServerDialog({ cluster, pool, server, onClose }: { cluster: string; pool: Pool; server: PoolServer; onClose: () => void }) {
  const qc = useQueryClient();
  const rm = useMutation({
    mutationFn: () => nodesApi.removeServer(cluster, pool.name, server.name),
    onSuccess: () => { void qc.invalidateQueries({ queryKey: nodesKey(cluster) }); onClose(); },
  });
  return (
    <Dialog title={`Replace ${server.name}?`} onClose={onClose} onSubmit={() => rm.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri danger" disabled={rm.isPending}>{rm.isPending ? "Replacing…" : "Delete and replace"}</button>
      </>}>
      <p>The server never became a node. Kwerft deletes it at Hetzner and creates a new one. Its log is in <code>/var/log/kwerft-join.log</code> on the server if you want to look first.</p>
      {rm.isError && <p className="form-error" role="alert">{errorText(rm.error)}</p>}
    </Dialog>
  );
}
