import { useState, type ReactNode } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError } from "../api";
import {
  authLabels, authNote, authOptions, connectionProblem, gitApi, notOffered, providers, webhookHint,
  type Connection, type ConnectionInput, type GitAuth, type GitProvider,
} from "../builds";
import { CopyButton } from "../components/CopyButton";
import { Dialog } from "../components/Dialog";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { ago, workloads } from "../workloads";
import "../styles/builds.css";

const unreachable = "The console could not be reached. Check your connection and try again.";
const errText = (e: unknown) => (e instanceof ApiError ? e.message : unreachable);
const KEY = ["git-connections"];

// Settings → Git connections: access to Git hosts for builds. Owners and
// admins add, change and remove them; everyone sees them. Credentials are
// write-only: the console never shows them again, and the webhook secret
// only once, when it is made.
export function GitConnectionsCard({ canEdit }: { canEdit: boolean }) {
  const conns = useQuery({ queryKey: KEY, queryFn: gitApi.connections, retry: false, refetchInterval: (q) => (q.state.data?.some((c) => !c.ready) ? 5000 : 30000) });
  const [dialog, setDialog] = useState<{ edit?: Connection } | { remove: Connection } | { rotate: Connection }>();
  const [secret, setSecret] = useState<{ connection: Connection; secret: string; created: boolean }>();
  const unavailable = conns.isError && notOffered(conns.error);
  const list = conns.data ?? [];

  return (
    <section className="card">
      <div className="ch-h">
        <h2>Git connections</h2>
        {canEdit && !unavailable && <button className="btn sm" onClick={() => setDialog({})}><Icon name="plus" />Add connection</button>}
      </div>
      {conns.isPending && <div className="bd"><p className="loading">Loading Git connections…</p></div>}
      {unavailable && <div className="bd"><p className="dim note">This console cannot manage Git connections yet. Public repositories build without one.</p></div>}
      {conns.isError && !unavailable && <div className="bd"><div className="banner bad" role="alert"><Icon name="alert" /><span>{errText(conns.error)}</span></div></div>}
      {conns.isSuccess && list.length === 0 && (
        <div className="bd">
          <p className="dim note">
            Connect GitHub, GitLab, Gitea or any Git host so Kwerft can clone private repositories, build on every push and report build status on commits. Public repositories need no connection.
          </p>
        </div>
      )}
      {list.length > 0 && (
        <div className="conns">
          {list.map((c) => (
            <ConnectionRow key={c.name} c={c} canEdit={canEdit}
              onEdit={() => setDialog({ edit: c })} onRemove={() => setDialog({ remove: c })} onRotate={() => setDialog({ rotate: c })} />
          ))}
        </div>
      )}

      {dialog && "remove" in dialog && <DeleteConnectionDialog c={dialog.remove} onClose={() => setDialog(undefined)} />}
      {dialog && "rotate" in dialog && (
        <RotateDialog c={dialog.rotate} onClose={() => setDialog(undefined)}
          onRotated={(s) => { setDialog(undefined); setSecret({ connection: dialog.rotate, secret: s, created: false }); }} />
      )}
      {dialog && !("remove" in dialog) && !("rotate" in dialog) && (
        <ConnectionDialog edit={dialog.edit} onClose={() => setDialog(undefined)}
          onCreated={(c, s) => { setDialog(undefined); setSecret({ connection: c, secret: s, created: true }); }} />
      )}
      {secret && <SecretDialog {...secret} onClose={() => setSecret(undefined)} />}
    </section>
  );
}

function ConnectionRow({ c, canEdit, onEdit, onRemove, onRotate }: { c: Connection; canEdit: boolean; onEdit: () => void; onRemove: () => void; onRotate: () => void }) {
  const provider = providers.find((p) => p.id === c.provider)?.label ?? c.provider;
  const hosted = c.url.replace(/^https:\/\//, "") + (c.owner ? `/${c.owner}` : "");
  return (
    <div className="conn">
      <div>
        <div className="head">
          <b>{c.name}</b>
          {c.ready ? <span className="pill ok">Ready</span> : <span className="pill bad" title={c.message}>Not ready</span>}
        </div>
        <p className="meta">
          {provider} · <span className="mono">{hosted}</span> · {authLabels[c.auth] ?? c.auth}
          {c.githubApp?.slug ? ` (${c.githubApp.slug})` : ""}
          {c.account ? <> · acts as <b>{c.account}</b></> : null}
          {" · "}{c.projects.length ? `projects ${c.projects.join(", ")}` : "all projects"}
        </p>
        {!c.ready && c.message && <p className="meta form-error">{c.message}</p>}
      </div>
      {canEdit && (
        <div className="acts">
          <button className="btn sm" onClick={onEdit}>Edit</button>
          <button className="btn sm" onClick={onRotate} disabled={!c.webhookURL} title="Make a new webhook secret">Rotate secret</button>
          <button className="btn sm danger" onClick={onRemove} aria-label={`Delete ${c.name}`}><Icon name="trash" />Delete</button>
        </div>
      )}
      <div className="hook">
        <div className="url">
          <span className="dim">Webhook</span>
          {c.webhookURL ? <><code>{c.webhookURL}</code><CopyButton text={c.webhookURL} label="Copy URL" /></> : <span className="dim">—</span>}
          <span className={`pill nodot ${c.webhookAutomatic ? "info" : "mute"}`}>{c.webhookAutomatic ? "Automatic" : "Manual"}</span>
        </div>
        <span className="dim">{webhookHint(c)} {c.lastDelivery ? `Last delivery ${ago(c.lastDelivery)}.` : "No delivery yet."}</span>
      </div>
    </div>
  );
}

// ---- add and edit -------------------------------------------------------------

type Form = {
  name: string;
  provider: GitProvider;
  url: string;
  auth: GitAuth;
  owner: string;
  projects: string[];
  token: string;
  sshPrivateKey: string;
  knownHosts: string;
  appID: string;
  installationID: string;
  privateKey: string;
};

function formOf(c?: Connection): Form {
  return {
    name: c?.name ?? "", provider: c?.provider ?? "github", url: c?.url ?? "https://github.com", auth: c?.auth ?? "githubApp", owner: c?.owner ?? "",
    projects: c?.projects ?? [], token: "", sshPrivateKey: "", knownHosts: "",
    appID: c?.githubApp ? String(c.githubApp.appID) : "", installationID: c?.githubApp ? String(c.githubApp.installationID) : "", privateKey: "",
  };
}

/** The request: secrets only when typed, so an edit keeps the stored ones. */
function inputOf(f: Form): ConnectionInput {
  const t = (s: string) => s.trim() || undefined;
  const input: ConnectionInput = { name: f.name.trim(), provider: f.provider, url: f.url.trim().replace(/\/+$/, ""), auth: f.auth, owner: t(f.owner), projects: f.projects };
  if (f.auth === "token") input.token = t(f.token);
  if (f.auth === "sshKey") {
    input.sshPrivateKey = f.sshPrivateKey.trim() ? f.sshPrivateKey.trim() + "\n" : undefined;
    input.knownHosts = t(f.knownHosts);
  }
  if (f.auth === "githubApp") input.githubApp = { appID: Number(f.appID), installationID: Number(f.installationID), privateKey: t(f.privateKey) };
  return input;
}

function ConnectionDialog({ edit, onClose, onCreated }: { edit?: Connection; onClose: () => void; onCreated: (c: Connection, secret: string) => void }) {
  const queryClient = useQueryClient();
  const projects = useQuery({ queryKey: ["projects"], queryFn: workloads.projects });
  const [f, setF] = useState(() => formOf(edit));
  const [error, setError] = useState<{ field?: string; message: string }>();
  const set = <K extends keyof Form>(k: K, v: Form[K]) => {
    setF((prev) => ({ ...prev, [k]: v }));
    setError((e) => (e?.field === k ? undefined : e));
  };
  const save = useMutation({
    mutationFn: async () => {
      const input = inputOf(f);
      if (edit) return { connection: await gitApi.update(edit.name, input), webhookSecret: "" };
      return gitApi.create(input);
    },
    onSuccess: (res) => {
      void queryClient.invalidateQueries({ queryKey: KEY });
      if (edit) onClose();
      else onCreated(res.connection, res.webhookSecret);
    },
    onError: (e) => setError(e instanceof ApiError ? { field: e.field, message: e.message } : { message: unreachable }),
  });
  const err = (...fields: string[]) => (error?.field && fields.includes(error.field) ? error.message : undefined);
  const fieldKeys = ["name", "url", "owner", "token", "sshPrivateKey", "knownHosts", "githubApp.appID", "githubApp.installationID", "githubApp.privateKey", "auth"];
  const stored = edit && edit.auth === f.auth; // secrets of this kind are stored already
  const keep = stored ? "stored — leave empty to keep it" : undefined;

  function submit() {
    const problem = connectionProblem(inputOf(f), !edit || !stored);
    if (problem) return setError(problem);
    setError(undefined);
    save.mutate();
  }

  return (
    <Dialog wide title={edit ? `Edit ${edit.name}` : "Add a Git connection"} onClose={onClose} onSubmit={submit}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={save.isPending}>{save.isPending ? "Saving…" : edit ? "Save" : "Add connection"}</button>
      </>}>
      <div className="field">
        <label>Git host</label>
        <div className="seg" role="group" aria-label="Git host">
          {providers.map((p) => (
            <button type="button" key={p.id} aria-pressed={f.provider === p.id}
              onClick={() => {
                const prev = providers.find((x) => x.id === f.provider);
                setF((x) => ({
                  ...x, provider: p.id,
                  url: !x.url || x.url === prev?.url ? p.url : x.url,
                  auth: authOptions(p.id).includes(x.auth) ? x.auth : authOptions(p.id)[0]!,
                }));
              }}>{p.label}</button>
          ))}
        </div>
      </div>
      <div className="fields">
        <Field id="gc-name" label="Name" className="mono" value={f.name} onChange={(e) => set("name", e.target.value.toLowerCase())} disabled={!!edit}
          placeholder={f.provider === "github" ? "github-acme" : f.provider === "gitlab" ? "gitlab-acme" : "git-acme"} autoComplete="off" spellCheck={false}
          error={err("name")} hint={edit ? "The name cannot change: apps refer to it." : "Apps refer to it by this name."} />
        <Field id="gc-url" label="Address" className="mono" value={f.url} onChange={(e) => set("url", e.target.value)} placeholder="https://git.example.com"
          autoComplete="off" spellCheck={false} error={err("url")} />
        <Field id="gc-owner" label="Account or organisation (optional)" className="mono" value={f.owner} onChange={(e) => set("owner", e.target.value)} placeholder="acme"
          autoComplete="off" spellCheck={false} error={err("owner")} hint="Limits the connection to repositories of this owner or group." />
      </div>
      <div className="field">
        <label>Authentication</label>
        <div className="seg" role="group" aria-label="Authentication">
          {authOptions(f.provider).map((a) => <button type="button" key={a} aria-pressed={f.auth === a} onClick={() => set("auth", a)}>{authLabels[a]}</button>)}
        </div>
        {err("auth") ? <span className="field-error" role="alert">{err("auth")}</span> : <span className="hint">{authNote(f.auth)}</span>}
      </div>
      {f.auth === "token" && (
        <Field id="gc-token" label="Access token" className="mono" type="password" value={f.token} onChange={(e) => set("token", e.target.value)}
          autoComplete="off" spellCheck={false} placeholder={keep} error={err("token")}
          hint={f.provider === "github" ? "A fine-grained token with Contents (read), Commit statuses and Webhooks (read & write) on the repositories."
            : f.provider === "gitlab" ? "A project, group or personal access token with the api scope (read_repository is enough without webhooks and checks)."
            : "An access token with repository read access, and write access to webhooks and commit statuses."} />
      )}
      {f.auth === "sshKey" && (
        <>
          <TextArea id="gc-key" label="Private key" value={f.sshPrivateKey} onChange={(v) => set("sshPrivateKey", v)} error={err("sshPrivateKey")} placeholder={keep ?? "-----BEGIN OPENSSH PRIVATE KEY-----"}
            hint={<>Make one with <code>ssh-keygen -t ed25519 -N "" -f kwerft</code> and add <code>kwerft.pub</code> as a read-only deploy key to the repository.</>} />
          <TextArea id="gc-known" label="Known hosts (optional)" value={f.knownHosts} onChange={(v) => set("knownHosts", v)} error={err("knownHosts")} rows={3}
            placeholder={keep ?? "github.com ssh-ed25519 AAAAC3Nza…"} hint={<>The host's keys, from <code>ssh-keyscan {hostOf(f.url) || "git.example.com"}</code>, so Kwerft can tell it is talking to the right server.</>} />
        </>
      )}
      {f.auth === "githubApp" && (
        <>
          <div className="fields">
            <Field id="gc-appid" label="App ID" className="mono" inputMode="numeric" value={f.appID} onChange={(e) => set("appID", e.target.value.trim())} error={err("githubApp.appID")}
              hint="On the app's settings page, under About." />
            <Field id="gc-inst" label="Installation ID" className="mono" inputMode="numeric" value={f.installationID} onChange={(e) => set("installationID", e.target.value.trim())}
              error={err("githubApp.installationID")} hint="The number at the end of the installation's URL." />
          </div>
          <TextArea id="gc-pem" label="Private key" value={f.privateKey} onChange={(v) => set("privateKey", v)} error={err("githubApp.privateKey")}
            placeholder={keep ?? "-----BEGIN RSA PRIVATE KEY-----"} hint="The .pem file generated on the app's settings page. Needs Contents (read), Commit statuses (write) and Webhooks permissions." />
        </>
      )}
      <div className="field">
        <label id="gc-projects">Projects</label>
        <div className="projects-pick" role="group" aria-labelledby="gc-projects">
          <label className="check"><input type="checkbox" checked={f.projects.length === 0} onChange={() => set("projects", [])} />All projects</label>
          {projects.data?.map((p) => (
            <label key={p.name} className="check">
              <input type="checkbox" checked={f.projects.includes(p.name)}
                onChange={(e) => set("projects", e.target.checked ? [...f.projects, p.name] : f.projects.filter((x) => x !== p.name))} />
              {p.name}
            </label>
          ))}
          {f.projects.filter((x) => !projects.data?.some((p) => p.name === x)).map((x) => (
            <label key={x} className="check"><input type="checkbox" checked onChange={() => set("projects", f.projects.filter((y) => y !== x))} />{x}</label>
          ))}
        </div>
        <span className="hint">Only apps in these projects may build with it.</span>
      </div>
      {error && !fieldKeys.includes(error.field ?? "") && <p className="form-error" role="alert">{error.field ? <><code>{error.field}</code>: </> : null}{error.message}</p>}
    </Dialog>
  );
}

const hostOf = (url: string) => /^https:\/\/([^/:]+)/.exec(url)?.[1] ?? "";

function TextArea({ id, label, value, onChange, error, hint, placeholder, rows = 5 }: {
  id: string; label: string; value: string; onChange: (v: string) => void; error?: string; hint?: ReactNode; placeholder?: string; rows?: number;
}) {
  return (
    <div className="field">
      <label htmlFor={id}>{label}</label>
      <textarea id={id} className="input mono" rows={rows} value={value} onChange={(e) => onChange(e.target.value)} placeholder={placeholder}
        spellCheck={false} autoComplete="off" aria-invalid={!!error} aria-describedby={`${id}-note`} />
      {error ? <span id={`${id}-note`} className="field-error" role="alert">{error}</span> : hint ? <span id={`${id}-note`} className="hint">{hint}</span> : null}
    </div>
  );
}

// ---- secrets, rotation, deletion ------------------------------------------------------

function SecretDialog({ connection, secret, created, onClose }: { connection: Connection; secret: string; created: boolean; onClose: () => void }) {
  return (
    <Dialog wide title={created ? `${connection.name} is added` : `New webhook secret for ${connection.name}`} onClose={onClose} onSubmit={onClose}
      actions={<button className="btn pri">I have stored it</button>}>
      {secret ? (
        <>
          <div className="store-now" role="status">
            <p><b>Store the webhook secret now.</b> Kwerft shows it only this once; if it is lost, rotate it.</p>
          </div>
          <div className="secret-box"><code>{secret}</code><CopyButton text={secret} /></div>
        </>
      ) : <p className="dim note">The server did not return a webhook secret.</p>}
      {connection.webhookURL && (
        <div className="field">
          <label>Webhook URL</label>
          <div className="secret-box"><code>{connection.webhookURL}</code><CopyButton text={connection.webhookURL} /></div>
          <span className="hint">{webhookHint(connection)}</span>
        </div>
      )}
      {!created && !connection.webhookAutomatic && <p className="dim note">The old secret no longer works: enter the new one in every repository's webhook.</p>}
    </Dialog>
  );
}

function RotateDialog({ c, onClose, onRotated }: { c: Connection; onClose: () => void; onRotated: (secret: string) => void }) {
  const rotate = useMutation({ mutationFn: () => gitApi.rotateSecret(c.name), onSuccess: (r) => onRotated(r.webhookSecret) });
  return (
    <Dialog title={`Rotate the webhook secret of ${c.name}?`} onClose={onClose} onSubmit={() => rotate.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={rotate.isPending}>{rotate.isPending ? "Rotating…" : "Rotate secret"}</button>
      </>}>
      <p style={{ margin: 0 }}>
        The current secret stops working at once.{" "}
        {c.webhookAutomatic ? "Kwerft updates the webhooks it registered itself." : "Pushes are ignored until you enter the new secret in each repository's webhook."}
      </p>
      {rotate.isError && <p className="form-error" role="alert">{errText(rotate.error)}</p>}
    </Dialog>
  );
}

function DeleteConnectionDialog({ c, onClose }: { c: Connection; onClose: () => void }) {
  const queryClient = useQueryClient();
  const apps = useQuery({ queryKey: ["apps"], queryFn: () => workloads.apps() });
  const users = (apps.data ?? []).filter((a) => a.source.type === "git" && a.source.connection === c.name).map((a) => `${a.project}/${a.name}`);
  const [confirm, setConfirm] = useState("");
  const del = useMutation({
    mutationFn: () => gitApi.remove(c.name),
    onSuccess: () => { void queryClient.invalidateQueries({ queryKey: KEY }); onClose(); },
  });
  return (
    <Dialog title={`Delete ${c.name}?`} onClose={onClose} onSubmit={() => confirm === c.name && del.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri danger" disabled={confirm !== c.name || del.isPending}>{del.isPending ? "Deleting…" : "Delete connection"}</button>
      </>}>
      <p style={{ margin: 0 }}>Kwerft forgets its credentials{c.webhookAutomatic ? " and removes the webhooks it registered" : ""}. Running apps keep running.</p>
      {users.length > 0 && <p className="warn-text note" role="status">Used by {users.join(", ")}: their next builds fail until they get another connection.</p>}
      <Field id="gc-delete" label={`Type ${c.name} to confirm`} className="mono" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoFocus autoComplete="off"
        error={del.isError ? errText(del.error) : undefined} />
    </Dialog>
  );
}
