import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError } from "../api";
import { roleLabel, type Role } from "../access";
import { CopyButton } from "../components/CopyButton";
import { Dialog } from "../components/Dialog";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import {
  daysProblem, describeProjects, downloadText, identityApi, parseNames, tokenRoles,
  type APIToken, type IssuedKubeconfig, type IssuedToken, type TokenInput,
} from "../identity";
import { workloads } from "../workloads";
import "../styles/workloads.css";
import "../styles/builds.css";

const unreachable = "The console could not be reached. Check your connection and try again.";
const errText = (e: unknown) => (e instanceof ApiError ? e.message : unreachable);
const fmt = (iso: string) => new Date(iso).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
const fmtDate = (iso: string) => new Date(iso).toLocaleDateString(undefined, { dateStyle: "medium" });
const tokensKey = ["account", "tokens"];

type Kind = APIToken["kind"];

// API tokens and kubeconfigs on the account page: both are tokens that act
// as the user, with their role or less, until they expire or are revoked.
export function TokensCard({ role }: { role: Role }) {
  const list = useQuery({ queryKey: tokensKey, queryFn: identityApi.tokens });
  const [dialog, setDialog] = useState<Kind>();
  const [revoking, setRevoking] = useState<APIToken>();
  return (
    <section className="card">
      <h2>API tokens</h2>
      <div className="bd stack">
        <p className="dim note">Tokens let scripts and kubectl act as you, with your role or less. They work until they expire or you revoke them.</p>
        {list.isPending && <p className="dim">Loading…</p>}
        {list.isError && <p className="form-error" role="alert">{errText(list.error)}</p>}
        {list.data && (list.data.tokens.length === 0
          ? <p className="dim note">No tokens yet.</p>
          : <ul className="rows">{list.data.tokens.map((t) => <TokenRow key={t.id} token={t} onRevoke={() => setRevoking(t)} />)}</ul>)}
        <div className="actions">
          <button className="btn" onClick={() => setDialog("kubeconfig")} disabled={!list.data}>Download kubeconfig</button>
          <button className="btn" onClick={() => setDialog("api")} disabled={!list.data}><Icon name="plus" />Create token</button>
        </div>
      </div>
      {dialog && list.data && (
        <TokenDialog kind={dialog} myRole={role} maxDays={list.data.maxDays} defaultDays={list.data.defaultDays} onClose={() => setDialog(undefined)} />
      )}
      {revoking && <RevokeDialog token={revoking} onClose={() => setRevoking(undefined)} />}
    </section>
  );
}

function TokenRow({ token: t, onRevoke }: { token: APIToken; onRevoke: () => void }) {
  return (
    <li>
      <div className="row">
        <Icon name="key" />
        <div className="grow">
          <b>{t.name}</b> {t.kind === "kubeconfig" && <span className="pill info nodot">kubeconfig</span>} {t.expired && <span className="pill bad">Expired</span>}
          <small>
            <code>{t.hint}</code> · {roleLabel[t.role] ?? t.role} · {describeProjects(t.projects)} · {t.expired ? "expired" : "expires"} {fmtDate(t.expiresAt)}
          </small>
          <small>{t.lastUsedAt ? `Last used ${fmt(t.lastUsedAt)}${t.lastUsedIp ? ` from ${t.lastUsedIp}` : ""}` : "Never used"}</small>
        </div>
        <button className="btn sm danger" onClick={onRevoke}>Revoke</button>
      </div>
    </li>
  );
}

function RevokeDialog({ token, onClose }: { token: APIToken; onClose: () => void }) {
  const queryClient = useQueryClient();
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function revoke() {
    setBusy(true);
    setError(undefined);
    try {
      await identityApi.revokeToken(token.id);
      await queryClient.invalidateQueries({ queryKey: tokensKey });
      onClose();
    } catch (err) {
      setError(errText(err));
      setBusy(false);
    }
  }

  return (
    <Dialog title={`Revoke ${token.name}?`} onClose={onClose} onSubmit={revoke}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri danger" disabled={busy}>{busy ? "Revoking…" : "Revoke token"}</button>
      </>}>
      <p className="note">
        {token.kind === "kubeconfig" ? "kubectl with this kubeconfig" : "Anything that uses this token"} stops working at once. This cannot be undone.
      </p>
      {error && <p className="form-error" role="alert">{error}</p>}
    </Dialog>
  );
}

type Issued = ({ kind: "api" } & IssuedToken) | ({ kind: "kubeconfig" } & IssuedKubeconfig);

function TokenDialog({ kind, myRole, maxDays, defaultDays, onClose }: {
  kind: Kind; myRole: Role; maxDays: number; defaultDays: number; onClose: () => void;
}) {
  const queryClient = useQueryClient();
  const projects = useQuery({ queryKey: ["projects"], queryFn: workloads.projects });
  const [name, setName] = useState("");
  const [role, setRole] = useState<Role>(() => (tokenRoles(myRole, false).includes("developer") ? "developer" : myRole));
  const [picked, setPicked] = useState<string[]>([]);
  // Typed by hand when the project list could not load.
  const [typed, setTyped] = useState("");
  const [days, setDays] = useState(String(defaultDays));
  const [error, setError] = useState<{ field?: string; message: string }>();
  const [busy, setBusy] = useState(false);
  const [issued, setIssued] = useState<Issued>();
  const err = (f: string) => (error?.field === f ? error.message : undefined);
  const fieldKeys = ["name", "role", "projects", "expiresInDays"];

  const chosen = projects.isError ? parseNames(typed) : picked;
  const choices = tokenRoles(myRole, chosen.length > 0);
  // Limiting to projects narrows the roles; fall back to the highest allowed.
  const roleValue = choices.includes(role) ? role : choices[0] ?? myRole;

  async function submit() {
    if (issued) return onClose();
    setError(undefined);
    if (kind === "api" && !name.trim()) return setError({ field: "name", message: "Enter a name." });
    const d = daysProblem(days, maxDays);
    if (d) return setError({ field: "expiresInDays", message: d });
    const input: TokenInput = { name: name.trim(), role: roleValue, projects: chosen, expiresInDays: Number(days.trim()) };
    setBusy(true);
    try {
      if (kind === "api") {
        setIssued({ kind, ...(await identityApi.createToken(input)) });
      } else {
        const res = await identityApi.kubeconfig(input);
        downloadText(res.kubeconfig, res.filename, "application/yaml");
        setIssued({ kind, ...res });
      }
      void queryClient.invalidateQueries({ queryKey: tokensKey });
    } catch (e) {
      setError(e instanceof ApiError ? { field: e.field, message: e.message } : { message: unreachable });
    } finally {
      setBusy(false);
    }
  }

  const title = kind === "api" ? (issued ? `${issued.apiToken.name} is created` : "Create token") : issued ? "Kubeconfig downloaded" : "Download kubeconfig";
  return (
    <Dialog wide title={title} onClose={onClose} onSubmit={submit}
      actions={issued ? <>
        {issued.kind === "kubeconfig" && (
          <button type="button" className="btn" onClick={() => downloadText(issued.kubeconfig, issued.filename, "application/yaml")}>Download again</button>
        )}
        <button className="btn pri">Done</button>
      </> : <>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={busy}>{busy ? (kind === "api" ? "Creating…" : "Preparing…") : kind === "api" ? "Create token" : "Download kubeconfig"}</button>
      </>}>
      {issued ? <IssuedView issued={issued} /> : (
        <>
          {kind === "kubeconfig" && (
            <p className="dim note">A kubeconfig for kubectl with a token of its own. It acts as you, with the role and projects you pick here.</p>
          )}
          <Field id="tok-name" label={kind === "api" ? "Name" : "Name (optional)"} value={name} onChange={(e) => setName(e.target.value)} maxLength={60}
            autoComplete="off" autoFocus placeholder={kind === "api" ? "e.g. CI deploys" : "e.g. laptop"} error={err("name")}
            hint="Helps you tell your tokens apart." />
          <div className="fields">
            <div className="field">
              <label htmlFor="tok-role">Role</label>
              <select id="tok-role" className="input" value={roleValue} onChange={(e) => setRole(e.target.value as Role)}
                aria-invalid={!!err("role")} aria-describedby="tok-role-note">
                {choices.map((r) => <option key={r} value={r}>{roleLabel[r]}</option>)}
              </select>
              {err("role") ? <span id="tok-role-note" className="field-error" role="alert">{err("role")}</span>
                : <span id="tok-role-note" className="hint">Yours or lower. A token limited to projects can be developer or viewer.</span>}
            </div>
            <Field id="tok-days" label="Expires after (days)" type="number" min={1} max={maxDays} inputMode="numeric" value={days}
              onChange={(e) => setDays(e.target.value)} required error={err("expiresInDays")} hint={`1 to ${maxDays} days.`} />
          </div>
          {projects.isError ? (
            <Field id="tok-projects" label="Projects" className="mono" value={typed} onChange={(e) => setTyped(e.target.value)} autoComplete="off" spellCheck={false}
              placeholder="shop, billing" error={err("projects")} hint="Project names, separated by commas. Empty: all projects." />
          ) : (
            <div className="field">
              <label id="tok-projects">Projects</label>
              <div className="projects-pick" role="group" aria-labelledby="tok-projects">
                <label className="check"><input type="checkbox" checked={picked.length === 0} onChange={() => setPicked([])} />All projects</label>
                {projects.data?.map((p) => (
                  <label key={p.name} className="check">
                    <input type="checkbox" checked={picked.includes(p.name)}
                      onChange={(e) => setPicked(e.target.checked ? [...picked, p.name] : picked.filter((x) => x !== p.name))} />
                    {p.name}
                  </label>
                ))}
                {projects.isPending && <span className="dim note">Loading projects…</span>}
              </div>
              {err("projects") ? <span className="field-error" role="alert">{err("projects")}</span>
                : <span className="hint">Pick projects to limit the token to them.</span>}
            </div>
          )}
          {error && !fieldKeys.includes(error.field ?? "") && <p className="form-error" role="alert">{error.message}</p>}
        </>
      )}
    </Dialog>
  );
}

function IssuedView({ issued }: { issued: Issued }) {
  if (issued.kind === "api") {
    const curl = `curl -H "Authorization: Bearer ${issued.token}" https://${window.location.host}/api/v1/session`;
    return (
      <>
        <div className="store-now" role="status"><p><b>Copy it now: it is not shown again.</b></p></div>
        <div className="secret-box"><code>{issued.token}</code><CopyButton text={issued.token} /></div>
        <div className="field">
          <label>Try it</label>
          <div className="secret-box"><code>{curl}</code><CopyButton text={curl} /></div>
          <span className="hint">Send the token as a bearer token with every request to the API.</span>
        </div>
      </>
    );
  }
  const project = issued.apiToken.projects[0] ?? "<project>";
  return (
    <>
      <p className="note">Your browser saved <code>{issued.filename}</code>, usually in Downloads. Point kubectl at it:</p>
      <pre className="codebox">{`export KUBECONFIG=~/Downloads/${issued.filename}\nkubectl get pods -n ${project}`}</pre>
      <p className="dim note">Shells (kubectl exec), port-forwarding and Secrets are not available through the kubeconfig; open shells in the console, where they are recorded.</p>
    </>
  );
}
