import { useState } from "react";
import { Link, useNavigate } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../api";
import { CopyButton } from "../components/CopyButton";
import { Dialog } from "../components/Dialog";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import {
  cloudLocations, clusterNameProblem, clusterState, clustersApi, clustersKey, nodesText, providerLabel,
  type AgentInstall, type Cluster,
} from "../clusterAdmin";
import { ago } from "../workloads";
import "../styles/workloads.css";
import "../styles/jobs.css";
import "../styles/builds.css";

const POLL = 5000;
const unreachable = "The console could not be reached. Check your connection and try again.";
const errText = (e: unknown) => (e instanceof ApiError ? e.message : unreachable);

// Clusters & nodes › list, create (Hetzner Cloud) and adopt (W3, Phase 5;
// see docs/phase5.md). Owners and admins only.
export function ClustersList() {
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const manage = session.data?.role === "owner" || session.data?.role === "admin";
  const list = useQuery({ queryKey: clustersKey, queryFn: clustersApi.list, refetchInterval: POLL, enabled: manage });
  const [dialog, setDialog] = useState<"create" | "adopt">();

  return (
    <section className="view">
      <div className="ph">
        <div>
          <h1>Clusters &amp; nodes</h1>
          <p>Every cluster runs Kwerft. Remote clusters connect to this console through an agent; their API servers stay private.</p>
        </div>
        {manage && (
          <div className="acts">
            <button className="btn" onClick={() => setDialog("adopt")}>Adopt a cluster</button>
            <button className="btn pri" onClick={() => setDialog("create")}><Icon name="plus" />Create on Hetzner Cloud</button>
          </div>
        )}
      </div>

      {session.data && !manage ? (
        <div className="empty"><h2>Owners and admins manage clusters</h2><p>Your role does not include clusters. Projects show which cluster they run in.</p></div>
      ) : list.isError ? (
        <div className="banner bad" role="alert"><Icon name="alert" /><span>{errText(list.error)}</span></div>
      ) : list.isPending ? (
        <p className="loading">Loading clusters…</p>
      ) : (
        <div className="card scroll-x">
          <table className="t">
            <thead>
              <tr><th>Cluster</th><th>Provider</th><th>State</th><th className="num">Nodes</th><th>Kubernetes</th><th>Kwerft</th><th>Last seen</th></tr>
            </thead>
            <tbody>
              {list.data.map((c) => <ClusterRow key={c.name} c={c} />)}
            </tbody>
          </table>
        </div>
      )}
      {list.data && list.data.length === 1 && (
        <p className="dim note">Only the cluster this console runs in so far. Create one on Hetzner Cloud, or adopt a cluster you installed elsewhere.</p>
      )}

      {dialog === "create" && <CreateDialog onClose={() => setDialog(undefined)} />}
      {dialog === "adopt" && <AdoptDialog onClose={() => setDialog(undefined)} />}
    </section>
  );
}

function ClusterRow({ c }: { c: Cluster }) {
  const state = clusterState(c);
  return (
    <tr>
      <td>
        <Link to="/clusters/$name" params={{ name: c.name }} className="nm">{c.name}</Link>
        {c.displayName && <span className="sub">{c.displayName}</span>}
      </td>
      <td>
        {providerLabel[c.provider] ?? c.provider}
        {c.hetznerCloud && <span className="sub">{c.hetznerCloud.location} · {c.hetznerCloud.serverType}{c.hetznerCloud.controlPlanes === 3 ? " · HA" : ""}</span>}
      </td>
      <td><span className={`pill ${state.tone}`} title={c.message}>{state.label}</span></td>
      <td className="num">{nodesText(c)}</td>
      <td>{c.kubernetesVersion ? <code>{c.kubernetesVersion}</code> : <span className="dim">—</span>}</td>
      <td>{c.agentVersion ? <code>{c.agentVersion}</code> : <span className="dim">—</span>}</td>
      <td className="dim" title={c.lastSeen ? new Date(c.lastSeen).toLocaleString() : undefined}>
        {c.connected && c.provider !== "local" ? "now" : c.provider === "local" ? "—" : ago(c.lastSeen)}
      </td>
    </tr>
  );
}

function CreateDialog({ onClose }: { onClose: () => void }) {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [name, setName] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [location, setLocation] = useState("fsn1");
  const [serverType, setServerType] = useState("cx32");
  const [controlPlanes, setControlPlanes] = useState<1 | 3>(1);
  const [error, setError] = useState<{ field?: string; message: string }>();
  const [busy, setBusy] = useState(false);
  const err = (f: string) => (error?.field === f ? error.message : undefined);

  async function submit() {
    const problem = clusterNameProblem(name);
    if (problem) return setError({ field: "name", message: problem });
    if (!serverType.trim()) return setError({ field: "serverType", message: "Enter a server type, such as cx32." });
    setError(undefined);
    setBusy(true);
    try {
      await clustersApi.create({ name, displayName: displayName.trim() || undefined, provider: "hetzner-cloud",
        hetznerCloud: { location, serverType: serverType.trim(), controlPlanes } });
      void queryClient.invalidateQueries({ queryKey: clustersKey });
      onClose();
      void navigate({ to: "/clusters/$name", params: { name } });
    } catch (e) {
      setError(e instanceof ApiError ? { field: e.field, message: e.message } : { message: unreachable });
      setBusy(false);
    }
  }

  return (
    <Dialog wide title="Create a cluster on Hetzner Cloud" onClose={onClose} onSubmit={submit}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={busy}>{busy ? "Creating…" : "Create cluster"}</button>
      </>}>
      <p className="dim note">
        Kwerft creates the control-plane servers with the Cloud API token from Settings, installs Kubernetes and Kwerft on them,
        and the cluster connects back here. Servers are billed by Hetzner from the moment they exist.
      </p>
      <div className="fields">
        <Field id="cl-name" label="Name" className="mono" value={name} onChange={(e) => setName(e.target.value.toLowerCase())} autoFocus
          autoComplete="off" spellCheck={false} placeholder="edge" maxLength={40} error={err("name")} hint="Used in server names. Cannot change later." />
        <Field id="cl-display" label="Display name (optional)" value={displayName} onChange={(e) => setDisplayName(e.target.value)} maxLength={80}
          placeholder="Edge, EU" error={err("displayName")} />
      </div>
      <div className="fields">
        <div className="field">
          <label htmlFor="cl-loc">Location</label>
          <select id="cl-loc" className="input" value={location} onChange={(e) => setLocation(e.target.value)} aria-invalid={!!err("location")}>
            {cloudLocations.map((l) => <option key={l.id} value={l.id}>{l.label}</option>)}
          </select>
          {err("location") && <span className="field-error" role="alert">{err("location")}</span>}
        </div>
        <Field id="cl-type" label="Server type" className="mono" value={serverType} onChange={(e) => setServerType(e.target.value.toLowerCase())}
          autoComplete="off" spellCheck={false} error={err("serverType")} hint="For the control-plane servers, e.g. cx32. Add worker pools on the Nodes tab later." />
      </div>
      <div className="field">
        <label>Control plane</label>
        <div className="choice two" role="group" aria-label="Control plane">
          <button type="button" className="opt" aria-pressed={controlPlanes === 1} onClick={() => setControlPlanes(1)}>
            <b>1 server</b><span>Cheapest. The cluster's API is down while that server is.</span>
          </button>
          <button type="button" className="opt" aria-pressed={controlPlanes === 3} onClick={() => setControlPlanes(3)}>
            <b>3 servers</b><span>Highly available: survives losing one of them.</span>
          </button>
        </div>
        {err("controlPlanes") && <span className="field-error" role="alert">{err("controlPlanes")}</span>}
      </div>
      {error && !["name", "displayName", "location", "serverType", "controlPlanes"].includes(error.field ?? "") && (
        <p className="form-error" role="alert">{error.message}</p>
      )}
    </Dialog>
  );
}

function AdoptDialog({ onClose }: { onClose: () => void }) {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [displayName, setDisplayName] = useState("");
  const [error, setError] = useState<{ field?: string; message: string }>();
  const [busy, setBusy] = useState(false);
  const [issued, setIssued] = useState<AgentInstall>();

  async function submit() {
    if (issued) return onClose();
    const problem = clusterNameProblem(name);
    if (problem) return setError({ field: "name", message: problem });
    setError(undefined);
    setBusy(true);
    try {
      const res = await clustersApi.create({ name, displayName: displayName.trim() || undefined, provider: "adopted" });
      if (res.token && res.installCommand) setIssued({ token: res.token, installCommand: res.installCommand });
      void queryClient.invalidateQueries({ queryKey: clustersKey });
    } catch (e) {
      setError(e instanceof ApiError ? { field: e.field, message: e.message } : { message: unreachable });
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog wide title={issued ? `Install the agent on ${name}` : "Adopt a cluster"} onClose={onClose} onSubmit={submit}
      actions={issued ? <button className="btn pri">Done</button> : <>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={busy}>{busy ? "Adding…" : "Add cluster"}</button>
      </>}>
      {issued ? <InstallCommand install={issued} /> : (
        <>
          <p className="dim note">
            For servers Kwerft did not create: a dedicated server, or another provider's. The next step shows a command that turns a fresh Ubuntu
            server into this cluster's first node and connects it here.
          </p>
          <div className="fields">
            <Field id="ad-name" label="Name" className="mono" value={name} onChange={(e) => setName(e.target.value.toLowerCase())} autoFocus
              autoComplete="off" spellCheck={false} placeholder="office" maxLength={40} error={error?.field === "name" ? error.message : undefined}
              hint="Lowercase letters, digits and dashes." />
            <Field id="ad-display" label="Display name (optional)" value={displayName} onChange={(e) => setDisplayName(e.target.value)} maxLength={80}
              error={error?.field === "displayName" ? error.message : undefined} />
          </div>
          {error && !["name", "displayName"].includes(error.field ?? "") && <p className="form-error" role="alert">{error.message}</p>}
        </>
      )}
    </Dialog>
  );
}

/** The agent's install command, shown once (adopt, rotate). */
export function InstallCommand({ install }: { install: AgentInstall }) {
  return (
    <>
      <div className="store-now" role="status"><p><b>Copy it now: the token in it is not shown again.</b></p></div>
      <div className="field">
        <label>Run as root on the cluster's first server</label>
        <div className="secret-box"><code>{install.installCommand}</code><CopyButton text={install.installCommand} /></div>
        <span className="hint">
          Ubuntu 22.04, 24.04 or 26.04 with 4 GB of RAM or more. It installs Kubernetes and Kwerft without a console of its own, and the
          cluster connects here: outbound HTTPS to this console is all it needs. On a server that already runs the agent it only updates the token.
        </span>
      </div>
    </>
  );
}
