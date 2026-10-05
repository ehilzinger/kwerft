import { useState, type FormEvent } from "react";
import { Link, getRouteApi, useNavigate } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../api";
import { Dialog } from "../components/Dialog";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { RevealSecret } from "../components/RevealSecret";
import { abilities, ago, words, workloads } from "../workloads";
import {
  derivedOf, derivedText, describeKey, keyList, keyProblem, lastUpdate, missingKeys, parseUser, secretKeys, secretsApi, setNameProblem,
  type SecretSet,
} from "../secrets";
import { errorText } from "./Apps";
import "../styles/workloads.css";
import "../styles/jobs.css";
import "../styles/secrets.css";

const route = getRouteApi("/authed/secrets");
const POLL = 5000;
/** A change this recent gets the banner on top. */
const RECENT_MS = 10 * 60 * 1000;

export function Secrets() {
  const search = route.useSearch();
  const navigate = useNavigate();
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const projects = useQuery({ queryKey: ["projects"], queryFn: workloads.projects, refetchInterval: POLL });
  const project = search.project ?? projects.data?.[0]?.name;
  const proj = projects.data?.find((p) => p.name === project);
  const can = abilities(session.data, proj);
  const platform = session.data?.role === "owner" || session.data?.role === "admin";
  const sets = useQuery({ queryKey: secretKeys.sets(project ?? ""), queryFn: () => secretsApi.sets(project!), enabled: !!project, refetchInterval: POLL });
  const [dialog, setDialog] = useState<"create" | "edit" | "copy" | "delete">();
  const go = (s: { project?: string; set?: string }) => void navigate({ to: "/secrets", search: s, replace: true });

  const list = sets.data ?? [];
  const selected = list.find((s) => s.name === search.set) ?? list[0];
  const denied = can.deploy ? undefined : "Your role can see key names here but not change secrets.";
  const recent = recentChange(list);

  return (
    <section className="view">
      <div className="ph">
        <div>
          <h1>Secrets</h1>
          <p className="sub">{project ? <>Project {project} · </> : null}values are encrypted at rest and write-only · apps reference them by set and key</p>
        </div>
        <div className="acts">
          {(projects.data?.length ?? 0) > 1 && (
            <div className="seg" role="group" aria-label="Project">
              {projects.data!.map((p) => <button key={p.name} aria-pressed={project === p.name} onClick={() => go({ project: p.name })}>{p.name}</button>)}
            </div>
          )}
          <button className="btn pri" disabled={!project || !can.deploy} title={denied} onClick={() => setDialog("create")}><Icon name="plus" />New secret set</button>
        </div>
      </div>

      {sets.isError && <div className="banner bad" role="alert"><Icon name="alert" /><span>{errorText(sets.error)}</span></div>}
      {recent && (
        <div className="banner info" role="status">
          <Icon name="key" />
          <span><b>{recent.key} was changed {ago(recent.at)}.</b> {rollNote(recent.set)}</span>
        </div>
      )}

      {projects.isSuccess && projects.data.length === 0 ? (
        <div className="empty"><h2>No projects yet</h2><p>Secret sets belong to a project. Create one on the Apps page first.</p></div>
      ) : sets.isPending ? (
        <p className="loading">Loading secret sets…</p>
      ) : list.length === 0 ? (
        <div className="empty">
          <h2>No secret sets in {project}</h2>
          <p>A secret set holds values such as API keys and passwords. Apps, jobs and schedules of the project reference them by set and key; nobody reads them back.</p>
          {can.deploy && <button className="btn pri" onClick={() => setDialog("create")}><Icon name="plus" />New secret set</button>}
        </div>
      ) : (
        <div className="g2">
          <div className="card" style={{ overflow: "hidden" }}>
            <div className="ch-h"><h3>Secret sets</h3><span className="dim small">Shared by every app and job in the project</span></div>
            <div className="scroll-x">
              <table className="t">
                <thead><tr><th>Set</th><th>Keys</th><th>Used by</th><th>Updated</th></tr></thead>
                <tbody>
                  {list.map((s) => {
                    const last = lastUpdate(s);
                    const missing = missingKeys(s);
                    return (
                      <tr key={s.name} className="click" aria-selected={selected?.name === s.name} onClick={() => go({ project, set: s.name })}>
                        <td>
                          <span className="nm"><Link to="/secrets" search={{ project, set: s.name }} onClick={(e) => e.stopPropagation()}>{s.name}</Link></span>
                          <span className="sub"><SetSub set={s} /></span>
                        </td>
                        <td className="mono">
                          {s.keys.length === 0 && missing.length === 0 && <span className="dim">none yet</span>}
                          {s.keys.map((k) => <span key={k.name} className="key-name">{k.name}</span>)}
                          {missing.filter((k) => !s.keys.some((x) => x.name === k)).map((k) => <span key={k} className="key-name dim">{k} — referenced, not set</span>)}
                        </td>
                        <td><UsedBy project={s.project} users={s.usedBy} /></td>
                        <td>{last ? <><span className="dim">{ago(last.updatedAt)}</span><span className="sub">{last.updatedBy || (last.source === "Generated" || last.source === "Derived" ? "Kwerft" : "")}</span></> : <span className="dim">—</span>}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </div>
          {selected && (
            <SetDetail key={`${selected.project}/${selected.name}`} set={selected} canWrite={can.deploy} platform={platform}
              onEdit={() => setDialog("edit")} onCopy={() => setDialog("copy")} onDelete={() => setDialog("delete")} />
          )}
        </div>
      )}

      {dialog === "create" && project && <CreateSetDialog project={project} onClose={() => setDialog(undefined)} onCreated={(s) => go({ project, set: s.name })} />}
      {dialog === "edit" && selected && <EditSetDialog set={selected} onClose={() => setDialog(undefined)} />}
      {dialog === "copy" && selected && <CopySetDialog set={selected} projects={(projects.data ?? []).map((p) => p.name)} onClose={() => setDialog(undefined)}
        onCopied={(s) => go({ project: s.project, set: s.name })} />}
      {dialog === "delete" && selected && <DeleteSetDialog set={selected} onClose={() => setDialog(undefined)} onDeleted={() => go({ project })} />}
    </section>
  );
}

/** The newest key change of the last minutes, for the banner. */
function recentChange(sets: SecretSet[], now = Date.now()) {
  let best: { key: string; at: string; set: SecretSet } | undefined;
  for (const s of sets) {
    const k = lastUpdate(s);
    if (k?.updatedAt && now - new Date(k.updatedAt).getTime() < RECENT_MS && (!best || k.updatedAt > best.at)) best = { key: k.name, at: k.updatedAt, set: s };
  }
  return best;
}

function rollNote(set: SecretSet) {
  const apps = set.usedBy.map(parseUser).filter((u) => u.kind === "App").map((u) => u.name);
  const jobs = set.usedBy.map(parseUser).filter((u) => u.kind !== "App").map((u) => u.name);
  const parts: string[] = [];
  if (apps.length) parts.push(`${joinNames(apps)} ${apps.length === 1 ? "rolls" : "roll"} out with the new value`);
  if (jobs.length) parts.push(`${joinNames(jobs)} ${jobs.length === 1 ? "picks" : "pick"} it up at the next run`);
  return parts.length ? parts.join("; ") + "." : "Nothing uses this set yet.";
}

const joinNames = (n: string[]) => (n.length <= 1 ? n.join("") : `${n.slice(0, -1).join(", ")} and ${n[n.length - 1]}`);

function SetSub({ set }: { set: SecretSet }) {
  if (set.phase === "failed") return <span className="pill bad" title={set.message}>{words(set.reason) || "Failed"}</span>;
  if (set.phase === "missing") return <span className="pill warn" title={set.message}>{set.reason === "DerivedPending" ? "Waiting for input" : "Missing key"}</span>;
  if (set.app) return <>{set.app}'s own · deleted with the app</>;
  return <>{set.description || (set.generate.length || set.derived.length ? "generated by Kwerft" : "")}</>;
}

function UsedBy({ project, users }: { project: string; users: string[] }) {
  if (users.length === 0) return <span className="dim">nothing</span>;
  return (
    <span className="tags">
      {users.map((u) => {
        const { kind, name } = parseUser(u);
        if (kind === "App") return <Link key={u} to="/apps/$project/$name" params={{ project, name }} className="tag">app {name}</Link>;
        if (kind === "Schedule") return <Link key={u} to="/jobs/$project/schedules/$name" params={{ project, name }} className="tag">schedule {name}</Link>;
        if (kind === "Task") return <Link key={u} to="/jobs/$project/tasks/$name" params={{ project, name }} className="tag">task {name}</Link>;
        return <span key={u} className="tag">{u}</span>;
      })}
    </span>
  );
}

// ---- one set ---------------------------------------------------------------------

function SetDetail({ set, canWrite, platform, onEdit, onCopy, onDelete }: {
  set: SecretSet; canWrite: boolean; platform: boolean; onEdit: () => void; onCopy: () => void; onDelete: () => void;
}) {
  const queryClient = useQueryClient();
  const refresh = () => void queryClient.invalidateQueries({ queryKey: secretKeys.sets(set.project) });
  const [editing, setEditing] = useState<string>(); // the key whose value is being replaced
  const [error, setError] = useState<{ key?: string; message: string }>();
  const [notice, setNotice] = useState<string>();
  const missing = missingKeys(set).filter((k) => !set.keys.some((x) => x.name === k));
  const locked = (key: string) => set.derived.some((d) => d.key === key);
  const conflict = set.phase === "failed";
  const ro = !canWrite || conflict;

  const done = (msg: string) => { setError(undefined); setEditing(undefined); setNotice(msg); refresh(); };
  const fail = (key: string | undefined) => (e: unknown) => setError({ key, message: errorText(e) });
  const write = useMutation({
    mutationFn: ({ key, value }: { key: string; value?: string }) =>
      value === undefined ? secretsApi.generateKey(set.project, set.name, key) : secretsApi.setKey(set.project, set.name, key, value),
    onSuccess: (k) => done(`${k.name} saved. ${set.usedBy.length ? rollNote(set) : ""}`),
    onError: (e, v) => fail(v.key)(e),
  });
  const remove = useMutation({
    mutationFn: (key: string) => secretsApi.removeKey(set.project, set.name, key),
    onSuccess: (_, key) => done(`${key} removed.`),
    onError: (e, key) => fail(key)(e),
  });

  return (
    <div className="card secret-set">
      <div className="ch-h">
        <h3>{set.name}</h3>
        <span className="acts">
          {canWrite && <button type="button" className="btn sm" onClick={onEdit}>Edit</button>}
          {platform && <button type="button" className="btn sm" onClick={onCopy} disabled={conflict}>Copy to…</button>}
          {canWrite && <button type="button" className="btn sm danger" onClick={onDelete}>Delete</button>}
        </span>
      </div>
      <div className="bd fields">
        {(set.description || set.app) && <p className="full dim note">{set.app ? `${set.app}'s own set: deleted with the app.` : set.description}</p>}
        {(set.phase === "failed" || set.phase === "missing") && set.message && (
          <div className={`banner full ${set.phase === "failed" ? "bad" : "warn"}`}><Icon name="alert" /><span>{set.message}</span></div>
        )}
        {notice && <div className="banner info full" role="status"><Icon name="key" /><span>{notice}</span><button type="button" className="btn sm" onClick={() => setNotice(undefined)}>Dismiss</button></div>}

        {set.keys.map((k) => (
          <div key={k.name} className="field full secret-key">
            <label htmlFor={`key-${k.name}`}>{k.name}</label>
            {editing === k.name ? (
              <ValueForm id={`key-${k.name}`} pending={write.isPending} onCancel={() => setEditing(undefined)}
                onSave={(value) => write.mutate({ key: k.name, value })} />
            ) : (
              <div className="key-row">
                <input id={`key-${k.name}`} className="input mono" readOnly value="••••••••••••••••••••" aria-label={`${k.name} value, hidden`} tabIndex={-1} />
                {!ro && !locked(k.name) && <button type="button" className="btn sm" onClick={() => { setEditing(k.name); setError(undefined); }}>Replace</button>}
                {!ro && !locked(k.name) && (
                  <button type="button" className="btn sm" disabled={write.isPending} title="32 random bytes, base64url"
                    onClick={() => write.mutate({ key: k.name })}>Generate</button>
                )}
                {!ro && !locked(k.name) && !set.generate.includes(k.name) && (
                  <button type="button" className="btn ghost sm danger" disabled={remove.isPending} onClick={() => remove.mutate(k.name)}>Remove</button>
                )}
                {platform && !conflict && <RevealSecret project={set.project} set={set.name} secretKey={k.name} />}
              </div>
            )}
            {error?.key === k.name ? <span className="field-error" role="alert">{error.message}</span> : (
              <span className="hint">{describeKey(k, set)}{k.updatedAt ? ` · ${ago(k.updatedAt)}` : ""}</span>
            )}
          </div>
        ))}

        {missing.map((key) => (
          <div key={key} className="field full secret-key">
            <label htmlFor={`key-${key}`}>{key} <span className="pill warn">referenced, not set</span></label>
            {ro ? <span className="hint">Used by {set.missing.filter((m) => m.endsWith(`: ${key}`)).map((m) => m.split(":")[0]).join(", ")}.</span> : (
              <ValueForm id={`key-${key}`} pending={write.isPending} onSave={(value) => write.mutate({ key, value })} onGenerate={() => write.mutate({ key })} />
            )}
            {error?.key === key && <span className="field-error" role="alert">{error.message}</span>}
          </div>
        ))}

        {!ro && <AddKey set={set} pending={write.isPending} error={error?.key === "" ? error.message : undefined}
          onSave={(key, value) => write.mutate({ key, value })} onInvalid={(message) => setError({ key: "", message })} />}

        {(set.generate.length > 0 || set.derived.length > 0) && (
          <div className="full key-rules">
            {set.generate.length > 0 && <span className="hint">Generated while missing: <code>{set.generate.join(", ")}</code></span>}
            {set.derived.map((d) => <span key={d.key} className="hint">Derived: <code>{d.key} = {d.template}</code></span>)}
          </div>
        )}
        {error && error.key === undefined && <p className="form-error full" role="alert">{error.message}</p>}
        <span className="hint full">
          Nobody sees a value after saving{platform ? " — owners and admins reveal one at a time, after their password" : ""}. Recorded in the audit log by set and key, never the value.
          Apps using a changed value roll out one replica at a time; jobs pick it up at their next run.
        </span>
      </div>
    </div>
  );
}

function ValueForm({ id, pending, onSave, onCancel, onGenerate }: {
  id: string; pending: boolean; onSave: (value: string) => void; onCancel?: () => void; onGenerate?: () => void;
}) {
  const [value, setValue] = useState("");
  return (
    <div className="key-row">
      <input id={id} className="input mono" type="password" autoComplete="new-password" spellCheck={false} placeholder="New value" value={value} autoFocus={!!onCancel}
        onChange={(e) => setValue(e.target.value)} onKeyDown={(e) => { if (e.key === "Enter" && value) { e.preventDefault(); onSave(value); } }} />
      <button type="button" className="btn pri sm" disabled={!value || pending} onClick={() => onSave(value)}>Save</button>
      {onGenerate && <button type="button" className="btn sm" disabled={pending} onClick={onGenerate} title="32 random bytes, base64url">Generate</button>}
      {onCancel && <button type="button" className="btn ghost sm" onClick={onCancel}>Cancel</button>}
    </div>
  );
}

function AddKey({ set, pending, error, onSave, onInvalid }: {
  set: SecretSet; pending: boolean; error?: string; onSave: (key: string, value?: string) => void; onInvalid: (msg: string) => void;
}) {
  const [key, setKey] = useState("");
  const [value, setValue] = useState("");
  function submit(generate: boolean) {
    const k = key.trim();
    const problem = keyProblem(k) ?? (set.keys.some((x) => x.name === k) ? `${k} exists. Use Replace on it.` : undefined) ??
      (set.derived.some((d) => d.key === k) ? `${k} is derived. Edit the set's derived keys instead.` : undefined) ??
      (!generate && !value ? "Type a value or let Kwerft generate one." : undefined);
    if (problem) return onInvalid(problem);
    onSave(k, generate ? undefined : value);
    setKey("");
    setValue("");
  }
  return (
    <div className="field full secret-key">
      <label htmlFor="sec-new">Add key</label>
      <div className="key-row add">
        <input id="sec-new" className="input mono" placeholder="NAME" value={key} spellCheck={false} autoComplete="off" onChange={(e) => setKey(e.target.value)} aria-invalid={!!error} />
        <input className="input mono" type="password" aria-label="Value" placeholder="Value" autoComplete="new-password" value={value} onChange={(e) => setValue(e.target.value)}
          onKeyDown={(e) => { if (e.key === "Enter") { e.preventDefault(); submit(false); } }} />
        <button type="button" className="btn pri sm" disabled={pending || !key.trim() || !value} onClick={() => submit(false)}>Save</button>
        <button type="button" className="btn sm" disabled={pending || !key.trim()} onClick={() => submit(true)} title="32 random bytes, base64url">Generate</button>
      </div>
      {error ? <span className="field-error" role="alert">{error}</span> : <span className="hint">Type a value or let Kwerft generate one — nobody sees it after saving.</span>}
    </div>
  );
}

// ---- dialogs ---------------------------------------------------------------------

type FieldError = { field?: string; message: string };
const fieldError = (e: unknown): FieldError => (e instanceof ApiError ? { field: e.field, message: e.message } : { message: errorText(e) });

function RulesFields({ generate, setGenerate, derived, setDerived, error }: {
  generate: string; setGenerate: (v: string) => void; derived: string; setDerived: (v: string) => void; error?: FieldError;
}) {
  return (
    <>
      <Field id="ss-generate" label="Generate (optional)" className="mono" value={generate} onChange={(e) => setGenerate(e.target.value)} placeholder="PASSWORD"
        hint="Keys Kwerft fills with 32 random bytes while they are missing, comma-separated." error={error?.field?.startsWith("generate") ? error.message : undefined} />
      <div className="field">
        <label htmlFor="ss-derived">Derived keys (optional)</label>
        <textarea id="ss-derived" className="input mono" rows={3} value={derived} onChange={(e) => setDerived(e.target.value)} spellCheck={false}
          placeholder="DATABASE_URL=postgres://app:${PASSWORD}@postgres-main:5432/app" aria-invalid={error?.field?.startsWith("derived")} />
        {error?.field?.startsWith("derived") ? <span className="field-error" role="alert">{error.message}</span> :
          <span className="hint">One per line, KEY=template; <code>{"${KEY}"}</code> is another key of the set. Kept up to date when an input changes.</span>}
      </div>
    </>
  );
}

function rulesOf(generate: string, derived: string): { generate: string[]; derived: { key: string; template: string }[] } | FieldError {
  const g = keyList(generate);
  if (g.bad) return { field: "generate", message: `"${g.bad}" is not a valid key. Letters, digits, -, _ and . only.` };
  const d = derivedOf(derived);
  if (d.bad !== undefined) return { field: "derived", message: `Line ${d.bad + 1}: write KEY=template, with at least one \${KEY} in the template.` };
  return { generate: g.keys, derived: d.derived };
}

function CreateSetDialog({ project, onClose, onCreated }: { project: string; onClose: () => void; onCreated: (s: SecretSet) => void }) {
  const queryClient = useQueryClient();
  const [name, setName] = useState("");
  const [description, setDescription] = useState("");
  const [generate, setGenerate] = useState("");
  const [derived, setDerived] = useState("");
  const [error, setError] = useState<FieldError>();
  const create = useMutation({
    mutationFn: (body: Parameters<typeof secretsApi.createSet>[1]) => secretsApi.createSet(project, body),
    onSuccess: (s) => { void queryClient.invalidateQueries({ queryKey: secretKeys.sets(project) }); onCreated(s); onClose(); },
    onError: (e) => setError(fieldError(e)),
  });
  function submit(e?: FormEvent) {
    e?.preventDefault();
    const problem = setNameProblem(name);
    if (problem) return setError({ field: "name", message: problem });
    const rules = rulesOf(generate, derived);
    if ("message" in rules) return setError(rules);
    setError(undefined);
    create.mutate({ name, description: description.trim() || undefined, ...rules });
  }
  return (
    <Dialog title={`New secret set in ${project}`} onClose={onClose} onSubmit={submit} wide
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={create.isPending}>{create.isPending ? "Creating…" : "Create set"}</button>
      </>}>
      <Field id="ss-name" label="Name" className="mono" value={name} onChange={(e) => setName(e.target.value.toLowerCase())} placeholder="payments" autoFocus autoComplete="off"
        spellCheck={false} error={error?.field === "name" ? error.message : undefined} hint="Apps reference it by this name, like payments/STRIPE_KEY." />
      <Field id="ss-desc" label="Description (optional)" value={description} onChange={(e) => setDescription(e.target.value)} placeholder="Stripe live account"
        error={error?.field === "description" ? error.message : undefined} />
      <RulesFields generate={generate} setGenerate={setGenerate} derived={derived} setDerived={setDerived} error={error} />
      <p className="hint" style={{ margin: 0 }}>You add values after creating it; they are write-only.</p>
      {error && !["name", "description"].includes(error.field ?? "") && !error.field?.match(/^(generate|derived)/) && <p className="form-error" role="alert">{error.message}</p>}
    </Dialog>
  );
}

function EditSetDialog({ set, onClose }: { set: SecretSet; onClose: () => void }) {
  const queryClient = useQueryClient();
  const [description, setDescription] = useState(set.description ?? "");
  const [generate, setGenerate] = useState(set.generate.join(", "));
  const [derived, setDerived] = useState(derivedText(set.derived));
  const [error, setError] = useState<FieldError>();
  const save = useMutation({
    mutationFn: (rules: { generate: string[]; derived: { key: string; template: string }[] }) =>
      secretsApi.updateSet(set.project, set.name, { description: description.trim(), ...rules, resourceVersion: set.resourceVersion }),
    onSuccess: () => { void queryClient.invalidateQueries({ queryKey: secretKeys.sets(set.project) }); onClose(); },
    onError: (e) => setError(fieldError(e)),
  });
  function submit() {
    const rules = rulesOf(generate, derived);
    if ("message" in rules) return setError(rules);
    setError(undefined);
    save.mutate(rules);
  }
  return (
    <Dialog title={`Edit ${set.name}`} onClose={onClose} onSubmit={submit} wide
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={save.isPending}>{save.isPending ? "Saving…" : "Save"}</button>
      </>}>
      <Field id="ss-desc" label="Description" value={description} onChange={(e) => setDescription(e.target.value)} autoFocus
        error={error?.field === "description" ? error.message : undefined} />
      <RulesFields generate={generate} setGenerate={setGenerate} derived={derived} setDerived={setDerived} error={error} />
      <p className="hint" style={{ margin: 0 }}>Taking a key off these lists keeps its value; remove it from the set afterwards if it should go.</p>
      {error && error.field !== "description" && !error.field?.match(/^(generate|derived)/) && <p className="form-error" role="alert">{error.message}</p>}
    </Dialog>
  );
}

function CopySetDialog({ set, projects, onClose, onCopied }: { set: SecretSet; projects: string[]; onClose: () => void; onCopied: (s: SecretSet) => void }) {
  const queryClient = useQueryClient();
  const others = projects.filter((p) => p !== set.project);
  const [to, setTo] = useState(others[0] ?? "");
  const [name, setName] = useState(set.name);
  const [error, setError] = useState<FieldError>();
  const copy = useMutation({
    mutationFn: () => secretsApi.copySet(set.project, set.name, to, name === set.name ? undefined : name),
    onSuccess: (s) => { void queryClient.invalidateQueries({ queryKey: secretKeys.sets(to) }); onCopied(s); onClose(); },
    onError: (e) => setError(fieldError(e)),
  });
  return (
    <Dialog title={`Copy ${set.name} to another project`} onClose={onClose} onSubmit={() => to && copy.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={!to || copy.isPending}>{copy.isPending ? "Copying…" : "Copy set"}</button>
      </>}>
      <p style={{ margin: 0 }}>The copy gets the same keys and values, then lives on its own: changing one set does not change the other. Recorded in the audit log.</p>
      <div className="field">
        <label htmlFor="cp-project">Project</label>
        <select id="cp-project" className="input" value={to} onChange={(e) => setTo(e.target.value)}>
          {others.length === 0 && <option value="">No other project</option>}
          {others.map((p) => <option key={p} value={p}>{p}</option>)}
        </select>
        {error?.field === "project" && <span className="field-error" role="alert">{error.message}</span>}
      </div>
      <Field id="cp-name" label="Name there" className="mono" value={name} onChange={(e) => setName(e.target.value.toLowerCase())}
        error={error?.field === "name" ? error.message : undefined} />
      {error && !["project", "name"].includes(error.field ?? "") && <p className="form-error" role="alert">{error.message}</p>}
    </Dialog>
  );
}

function DeleteSetDialog({ set, onClose, onDeleted }: { set: SecretSet; onClose: () => void; onDeleted: () => void }) {
  const queryClient = useQueryClient();
  const [confirm, setConfirm] = useState("");
  const del = useMutation({
    mutationFn: () => secretsApi.deleteSet(set.project, set.name),
    onSuccess: () => { void queryClient.invalidateQueries({ queryKey: secretKeys.sets(set.project) }); onDeleted(); onClose(); },
  });
  return (
    <Dialog title={`Delete ${set.name}?`} onClose={onClose} onSubmit={() => confirm === set.name && del.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri danger" disabled={confirm !== set.name || del.isPending}>{del.isPending ? "Deleting…" : "Delete set"}</button>
      </>}>
      <p style={{ margin: 0 }}>Every value in it is deleted. It cannot be undone.</p>
      {set.usedBy.length > 0 && (
        <div className="banner warn"><Icon name="alert" /><span>Used by {set.usedBy.join(", ")}. Apps keep their running replicas but cannot roll out until the keys exist again.</span></div>
      )}
      <Field id="sd-confirm" label={`Type ${set.name} to confirm`} className="mono" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="off"
        error={del.isError ? errorText(del.error) : undefined} />
    </Dialog>
  );
}
