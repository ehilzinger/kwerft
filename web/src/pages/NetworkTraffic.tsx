import { useEffect, useState } from "react";
import { Link } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../api";
import { Dialog } from "../components/Dialog";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { abilities, ago, workloads, NAME_RE } from "../workloads";
import {
  count, fromRow, parsePorts, peerLabel, portsLabel, portsText, sendsOut, sideLabel, toRow, traffic,
  type Drop, type HubbleStatus, type PeerKind, type PeerRow, type RuleInput, type Traffic, type TrafficRule,
} from "../traffic";
import { errorText } from "./Apps";
import "../styles/pods.css";
import "../styles/traffic.css";

const POLL = 10000;

// Network › Traffic rules: per project, the rules on top of each app's own
// "who may connect", with what Hubble saw in the last hour; dropped
// connections with a ready-made rule to allow them; the project's isolation.
export function TrafficRules() {
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const can = abilities(session.data);
  const projects = useQuery({ queryKey: ["projects"], queryFn: workloads.projects, refetchInterval: 30000 });
  const [picked, setPicked] = useState<string>();
  const project = picked ?? projects.data?.[0]?.name;
  const q = useQuery({
    queryKey: ["traffic", project],
    queryFn: () => traffic.get(project!),
    enabled: !!project,
    refetchInterval: POLL,
  });
  const [editing, setEditing] = useState<{ rule?: TrafficRule; draft?: RuleInput }>();
  const [deleting, setDeleting] = useState<TrafficRule>();

  if (projects.isPending) return <p className="loading">Loading projects…</p>;
  if (projects.isError) return <div className="banner bad" role="alert"><Icon name="alert" /><span>{errorText(projects.error)}</span></div>;
  if (!project) {
    return (
      <div className="empty">
        <h2>No projects yet</h2>
        <p>Traffic rules belong to a project: they let its apps receive from other apps, projects or the internet, and send out of it.</p>
        <Link to="/apps" className="btn">Go to apps</Link>
      </div>
    );
  }
  const data = q.data;
  const denied = can.deploy ? undefined : "Your role can view traffic rules but not change them.";
  const top = data?.drops.find((d) => d.suggestion?.project === project);

  return (
    <>
      {projects.data.length > 1 && (
        <div className="toolbar">
          <div className="seg" role="group" aria-label="Project">
            {projects.data.map((p) => <button key={p.name} aria-pressed={project === p.name} onClick={() => setPicked(p.name)}>{p.name}</button>)}
          </div>
        </div>
      )}
      {q.isError && <div className="banner bad" role="alert"><Icon name="alert" /><span>{errorText(q.error)}</span></div>}
      {data && <HubbleBanner h={data.hubble} />}
      {top && (
        <div className="banner warn" role="status">
          <Icon name="alert" />
          <span>
            <b>{top.count} connection{top.count === 1 ? "" : "s"} dropped in the last hour:</b>{" "}
            <code>{sideLabel(top.from)}</code> → <code>{sideLabel(top.to)}</code>{top.port ? <> on {top.protocol} {top.port}</> : null}. No rule allows it.
          </span>
          <button className="btn pri sm" disabled={!can.deploy} title={denied} onClick={() => setEditing({ draft: { name: top.suggestion!.name, ...top.suggestion!.spec } })}>
            Create allow rule
          </button>
        </div>
      )}

      <div className="toolbar">
        {data && <Isolation project={project} isolated={data.isolated} canChange={can.manageProjects} />}
        <button className="btn sm" style={{ marginLeft: "auto" }} disabled={!can.deploy} title={denied} onClick={() => setEditing({})}>
          <Icon name="plus" />New rule
        </button>
      </div>

      {!data ? (
        <p className="loading">Loading traffic rules…</p>
      ) : data.rules.length === 0 ? (
        <div className="empty">
          <h2>No traffic rules in {project}</h2>
          <p>
            Each app already accepts the ingress for its public ports, the platform, and the apps in its “who may connect” setting. Add a rule to let
            more in — another app or project, the internet, an address range — or to let this project's apps reach out.
          </p>
          {can.deploy && <button className="btn pri" onClick={() => setEditing({})}><Icon name="plus" />New rule</button>}
        </div>
      ) : (
        <div className="card scroll-x">
          <table className="t tr-table">
            <thead>
              <tr><th>Rule</th><th>From</th><th>To</th><th>Ports</th><th className="num">Allowed, 1 h</th><th className="num">Dropped</th><th><span className="sr">Actions</span></th></tr>
            </thead>
            <tbody>
              {data.rules.map((r) => (
                <tr key={r.name} className={r.disabled ? "off" : undefined}>
                  <td>
                    <span className="nm">{r.name}</span> {r.phase !== "ready" && <RuleStatus r={r} />}
                    {r.description && <span className="sub">{r.description}</span>}
                    {r.phase === "waiting" || r.phase === "failed" ? <span className="sub">{r.message}</span> : null}
                  </td>
                  <td><Peers peers={r.from} project={project} /></td>
                  <td><Peers peers={r.to} project={project} /></td>
                  <td className="mono">{portsLabel(r.ports)}</td>
                  <td className="num">{r.disabled ? "—" : count(r.counts?.allowed)}</td>
                  <td className={`num${(r.counts?.dropped ?? 0) > 0 ? " warn-text" : ""}`}>{r.disabled ? "—" : count(r.counts?.dropped)}</td>
                  <td className="nowrap">
                    <button className="btn sm" disabled={!can.deploy} title={denied} onClick={() => setEditing({ rule: r })}>Edit</button>{" "}
                    <button className="btn sm danger icon" aria-label={`Delete ${r.name}`} disabled={!can.deploy} title={denied ?? `Delete ${r.name}`} onClick={() => setDeleting(r)}><Icon name="trash" /></button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {data && data.drops.length > 0 && <Drops drops={data.drops} project={project} canCreate={can.deploy}
        onCreate={(d) => setEditing({ draft: { name: d.suggestion!.name, ...d.suggestion!.spec } })} />}

      <p className="dim note">
        Allowed counts connections a rule's policy let through; Dropped counts connections between the rule's sources and destinations that no
        rule allowed (for example on a port the rule does not list). A rule reaching another project's app takes effect once that project allows it too.
      </p>

      {editing && <RuleEditor project={project} rule={editing.rule} draft={editing.draft} onClose={() => setEditing(undefined)} />}
      {deleting && <DeleteRule project={project} rule={deleting} onClose={() => setDeleting(undefined)} />}
    </>
  );
}

function HubbleBanner({ h }: { h: HubbleStatus }) {
  if (h.state === "ok") return null;
  if (h.state === "off") return <div className="banner info" role="status"><Icon name="net" /><span>{h.message}</span></div>;
  if (h.state === "connecting") return <div className="banner info" role="status"><Icon name="net" /><span>Connecting to Hubble for counts and dropped connections…</span></div>;
  return <div className="banner warn" role="status"><Icon name="alert" /><span>Counts and dropped connections are unavailable: {h.message}</span></div>;
}

function Peers({ peers, project }: { peers: TrafficRule["from"]; project: string }) {
  return <span className="tags">{peers.map((p, i) => <span key={i} className="tag">{p.internet ? "internet via ingress" : peerLabel(p, project)}</span>)}</span>;
}

function RuleStatus({ r }: { r: TrafficRule }) {
  switch (r.phase) {
    case "ready": return <span className="pill ok" title={r.message}>Active</span>;
    case "disabled": return <span className="pill mute" title={r.message}>Disabled</span>;
    case "waiting": return <span className="pill warn" title={r.message}>{r.reason === "AwaitingPeer" ? "Waiting for the other project" : "No such app yet"}</span>;
    case "failed": return <span className="pill bad" title={r.message}>{r.reason === "InvalidRule" ? "Invalid" : "Failed"}</span>;
    default: return <span className="pill info" title={r.message}>Applying</span>;
  }
}

function Isolation({ project, isolated, canChange }: { project: string; isolated: boolean; canChange: boolean }) {
  const qc = useQueryClient();
  const set = useMutation({
    mutationFn: (v: boolean) => traffic.setIsolated(project, v),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["traffic", project] }),
  });
  const on = set.isPending ? !!set.variables : isolated;
  return (
    <>
      <button type="button" role="switch" aria-checked={on} className={`toggle ${on ? "on" : ""}`} disabled={!canChange || set.isPending}
        title={canChange ? undefined : "Owners and admins change a project's isolation."}
        onClick={() => set.mutate(!on)}>
        <i />Isolate {project} from other projects
      </button>
      {set.isError && <span className="form-error" role="alert">{errorText(set.error)}</span>}
    </>
  );
}

function Drops({ drops, project, canCreate, onCreate }: { drops: Drop[]; project: string; canCreate: boolean; onCreate: (d: Drop) => void }) {
  return (
    <div className="card scroll-x">
      <table className="t">
        <caption className="cap-left">Dropped in the last hour</caption>
        <thead><tr><th>From</th><th>To</th><th>Port</th><th className="num">Connections</th><th>Last</th><th><span className="sr">Actions</span></th></tr></thead>
        <tbody>
          {drops.map((d, i) => (
            <tr key={i}>
              <td className="mono">{sideLabel(d.from)}</td>
              <td className="mono">{sideLabel(d.to)}</td>
              <td className="mono">{d.port ? `${d.protocol} ${d.port}` : d.protocol ?? "—"}</td>
              <td className="num">{count(d.count)}</td>
              <td className="dim" title={new Date(d.last).toLocaleString()}>{ago(d.last)}</td>
              <td className="nowrap">
                {d.suggestion && d.suggestion.project === project ? (
                  <button className="btn sm" disabled={!canCreate} onClick={() => onCreate(d)}>Create allow rule</button>
                ) : d.suggestion ? (
                  <span className="dim">{d.suggestion.project} has to allow it</span>
                ) : null}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

// ---- editor ----------------------------------------------------------------

const kinds: { id: PeerKind; label: string }[] = [
  { id: "app", label: "App" },
  { id: "project", label: "Project" },
  { id: "internet", label: "Internet" },
  { id: "cidr", label: "Address range" },
];

function RuleEditor({ project, rule, draft, onClose }: { project: string; rule?: TrafficRule; draft?: RuleInput; onClose: () => void }) {
  const qc = useQueryClient();
  const start = rule ?? draft;
  const [name, setName] = useState(start?.name ?? "");
  const [description, setDescription] = useState(start?.description ?? "");
  const [disabled, setDisabled] = useState(rule?.disabled ?? false);
  const [from, setFrom] = useState<PeerRow[]>(start?.from.length ? start.from.map(toRow) : [{ kind: "app", value: "" }]);
  const [to, setTo] = useState<PeerRow[]>(start?.to.length ? start.to.map(toRow) : [{ kind: "app", value: "" }]);
  const [ports, setPorts] = useState(portsText(start?.ports ?? []));
  const [protocol, setProtocol] = useState<"TCP" | "UDP">(start?.ports[0]?.protocol ?? "TCP");
  const [error, setError] = useState<{ field?: string; message: string }>();
  const apps = useQuery({ queryKey: ["apps"], queryFn: () => workloads.apps() });
  const projects = useQuery({ queryKey: ["projects"], queryFn: workloads.projects });
  useEffect(() => setError(undefined), [name, from, to, ports, protocol]);

  const save = useMutation({
    mutationFn: (input: RuleInput) => (rule ? traffic.update(project, input, rule.generation) : traffic.create(project, input)),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["traffic", project] });
      onClose();
    },
    onError: (e) => setError({ field: e instanceof ApiError ? e.field : undefined, message: errorText(e) }),
  });

  const submit = () => {
    if (!rule && !NAME_RE.test(name)) return setError({ field: "name", message: "Use lowercase letters, digits and dashes, starting with a letter." });
    const parsed = parsePorts(ports, protocol);
    if (typeof parsed === "string") return setError({ field: "ports", message: parsed });
    save.mutate({ name, description, disabled, from: from.map(fromRow), to: to.map(fromRow), ports: parsed });
  };

  const appOptions = (apps.data ?? []).map((a) => (a.project === project ? a.name : `${a.project}/${a.name}`));
  const outward = sendsOut(to, project);

  return (
    <Dialog title={rule ? `Edit ${rule.name}` : "New traffic rule"} onClose={onClose} onSubmit={submit} wide
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={save.isPending}>{save.isPending ? "Saving…" : rule ? "Save rule" : "Create rule"}</button>
      </>}>
      <datalist id="tr-apps">{appOptions.map((a) => <option key={a} value={a} />)}</datalist>
      <datalist id="tr-projects">{(projects.data ?? []).map((p) => <option key={p.name} value={p.name} />)}</datalist>
      {!rule && <Field id="tr-name" label="Name" className="mono" value={name} onChange={(e) => setName(e.target.value)} maxLength={50}
        error={error?.field === "name" ? error.message : undefined} hint="e.g. web-to-api" />}
      <PeerRows legend="From" rows={from} onChange={setFrom} error={error?.field?.startsWith("from") ? error.message : undefined}
        hint={outward ? `This rule sends traffic out of ${project}, so its sources must be ${project}'s apps.` : "Apps, projects, the internet (through the ingress) or an address range."} />
      <PeerRows legend="To" rows={to} onChange={setTo} error={error?.field?.startsWith("to") ? error.message : undefined}
        hint={`Apps of ${project} receive. Another project's app, the internet or an address range: ${project}'s apps send there (another project has to allow it too).`} />
      <div className="tr-ports">
        <Field id="tr-ports" label="Ports" className="mono" value={ports} onChange={(e) => setPorts(e.target.value)} placeholder="any port"
          error={error?.field?.startsWith("ports") ? error.message : undefined} hint="Comma-separated; ranges like 9000-9010. Empty: every port." />
        <div className="field">
          <label htmlFor="tr-proto">Protocol</label>
          <select id="tr-proto" className="input" value={protocol} onChange={(e) => setProtocol(e.target.value as "TCP" | "UDP")}>
            <option>TCP</option><option>UDP</option>
          </select>
        </div>
      </div>
      <Field id="tr-desc" label="Description (optional)" value={description} onChange={(e) => setDescription(e.target.value)} maxLength={256} />
      {rule && (
        <label className="inline-check"><input type="checkbox" checked={disabled} onChange={(e) => setDisabled(e.target.checked)} /> Disabled: keep the rule, allow nothing</label>
      )}
      {error && !["name", "ports"].includes(error.field ?? "") && !error.field?.startsWith("from") && !error.field?.startsWith("to") && !error.field?.startsWith("ports") && (
        <p className="form-error" role="alert">{error.message}</p>
      )}
    </Dialog>
  );
}

function PeerRows({ legend, rows, onChange, hint, error }: { legend: string; rows: PeerRow[]; onChange: (r: PeerRow[]) => void; hint: string; error?: string }) {
  const set = (i: number, r: PeerRow) => onChange(rows.map((x, j) => (j === i ? r : x)));
  return (
    <fieldset className="field tr-peers">
      <legend>{legend}</legend>
      {rows.map((r, i) => (
        <div key={i} className="tr-peer">
          <select className="input" aria-label={`${legend} ${i + 1}: kind`} value={r.kind} onChange={(e) => set(i, { kind: e.target.value as PeerKind, value: "" })}>
            {kinds.map((k) => <option key={k.id} value={k.id}>{k.label}</option>)}
          </select>
          {r.kind === "internet" ? (
            <span className="dim">{legend === "From" ? "through the ingress" : "public addresses"}</span>
          ) : (
            <input className="input mono" aria-label={`${legend} ${i + 1}`} value={r.value} onChange={(e) => set(i, { ...r, value: e.target.value })}
              list={r.kind === "app" ? "tr-apps" : r.kind === "project" ? "tr-projects" : undefined}
              placeholder={r.kind === "app" ? "api or project/api" : r.kind === "project" ? "project" : "10.0.0.0/16"} />
          )}
          <button type="button" className="btn sm" aria-label={`Remove ${legend.toLowerCase()} ${i + 1}`} disabled={rows.length === 1}
            onClick={() => onChange(rows.filter((_, j) => j !== i))}><Icon name="trash" /></button>
        </div>
      ))}
      <button type="button" className="btn sm add" onClick={() => onChange([...rows, { kind: "app", value: "" }])}><Icon name="plus" />Add</button>
      {error ? <span className="field-error" role="alert">{error}</span> : <span className="hint">{hint}</span>}
    </fieldset>
  );
}

function DeleteRule({ project, rule, onClose }: { project: string; rule: TrafficRule; onClose: () => void }) {
  const qc = useQueryClient();
  const del = useMutation({
    mutationFn: () => traffic.remove(project, rule.name),
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: ["traffic", project] });
      onClose();
    },
  });
  return (
    <Dialog title={`Delete ${rule.name}?`} onClose={onClose} onSubmit={() => del.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri danger" disabled={del.isPending}>{del.isPending ? "Deleting…" : "Delete rule"}</button>
      </>}>
      <p>Traffic it allows ({peerList(rule.from, project)} → {peerList(rule.to, project)}, {portsLabel(rule.ports)}) is dropped from then on, unless another rule or an app's own setting allows it.</p>
      {del.isError && <p className="form-error" role="alert">{errorText(del.error)}</p>}
    </Dialog>
  );
}

function peerList(peers: Traffic["rules"][number]["from"], project: string) {
  return peers.map((p) => peerLabel(p, project)).join(", ");
}
