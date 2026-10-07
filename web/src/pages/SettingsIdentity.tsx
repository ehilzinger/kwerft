// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, accountApi } from "../api";
import { CopyButton } from "../components/CopyButton";
import { Dialog } from "../components/Dialog";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import {
  identityApi, parseDomains, providerButton, providerHint, providerLabel, providers, ssoPasswordHint,
  type DataKeyRotated, type SSOConfig, type SSOInput, type SSOProvider, type SSOSettings,
} from "../identity";
import "../styles/builds.css";

const unreachable = "The console could not be reached. Check your connection and try again.";
const errText = (e: unknown) => (e instanceof ApiError ? e.message : unreachable);
const ssoKey = ["settings", "sso"];
const dataKeyKey = ["settings", "data-key"];

// ---- single sign-on (owners and admins) --------------------------------------------

export function SSOCard() {
  const q = useQuery({ queryKey: ssoKey, queryFn: identityApi.ssoSettings });
  return (
    <section className="card">
      <h2>Single sign-on</h2>
      {q.isPending && <div className="bd"><p className="dim note">Loading…</p></div>}
      {q.isError && <div className="bd"><div className="banner bad" role="alert"><Icon name="alert" /><span>{errText(q.error)}</span></div></div>}
      {q.data && !q.data.available && (
        <div className="bd"><p className="dim note">Single sign-on needs the console's cluster connection, where its settings are kept. It is not available while the console runs without it.</p></div>
      )}
      {q.data?.available && <SSOForm s={q.data} />}
    </section>
  );
}

type Form = {
  enabled: boolean;
  provider: SSOProvider;
  issuer: string;
  tenant: string;
  clientId: string;
  displayName: string;
  domains: string;
  autoJoin: boolean;
  defaultRole: SSOConfig["defaultRole"];
};

const formOf = (c: SSOConfig | null): Form => ({
  enabled: c?.enabled ?? false,
  provider: c?.provider ?? "google",
  issuer: c?.issuer ?? "",
  tenant: c?.tenant ?? "",
  clientId: c?.clientId ?? "",
  displayName: c?.displayName ?? "",
  domains: (c?.allowedDomains ?? []).join(", "),
  autoJoin: c?.autoJoin ?? false,
  defaultRole: c?.defaultRole ?? "viewer",
});

// What the server stores: only the provider's own fields, the rest empty.
const inputOf = (f: Form, clientSecret: string): SSOInput => ({
  enabled: f.enabled,
  provider: f.provider,
  issuer: f.provider === "keycloak" || f.provider === "oidc" ? f.issuer.trim() : "",
  tenant: f.provider === "microsoft" ? f.tenant.trim() : "",
  clientId: f.clientId.trim(),
  clientSecret: clientSecret.trim(),
  displayName: f.displayName.trim(),
  allowedDomains: parseDomains(f.domains),
  autoJoin: f.autoJoin,
  defaultRole: f.defaultRole,
});

const fieldKeys = ["issuer", "tenant", "clientId", "clientSecret", "displayName", "allowedDomains", "defaultRole", "provider"];

function SSOForm({ s }: { s: SSOSettings }) {
  const queryClient = useQueryClient();
  const [f, setF] = useState(() => formOf(s.sso));
  const [secret, setSecret] = useState("");
  const [error, setError] = useState<{ field?: string; message: string }>();
  const [done, setDone] = useState(false);
  const [busy, setBusy] = useState(false);
  const set = <K extends keyof Form>(k: K, v: Form[K]) => {
    setF((x) => ({ ...x, [k]: v }));
    setDone(false);
    setError((e) => (e?.field === k || (k === "domains" && e?.field === "allowedDomains") ? undefined : e));
  };
  const err = (field: string) => (error?.field === field ? error.message : undefined);
  const changed = JSON.stringify(inputOf(f, "")) !== JSON.stringify(inputOf(formOf(s.sso), "")) || secret.trim() !== "";
  const needsSecret = f.enabled && !s.secretSet;

  async function save(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    setDone(false);
    if (needsSecret && !secret.trim()) return setError({ field: "clientSecret", message: "Enter the client secret from your provider." });
    if (f.autoJoin && parseDomains(f.domains).length === 0) {
      return setError({ field: "allowedDomains", message: "Add the email domains whose people may join." });
    }
    setBusy(true);
    try {
      const res = await identityApi.saveSSO(inputOf(f, secret));
      queryClient.setQueryData(ssoKey, res);
      void queryClient.invalidateQueries({ queryKey: ["sso"] });
      setF(formOf(res.sso));
      setSecret("");
      setDone(true);
    } catch (err) {
      setError(err instanceof ApiError ? { field: err.field, message: err.message } : { message: unreachable });
    } finally {
      setBusy(false);
    }
  }

  return (
    <form className="bd stack" onSubmit={save}>
      <p className="dim note">
        Members sign in with their account at your identity provider; Kwerft matches it to theirs by email address.
        Members' own passkeys and authenticator apps are still asked for after single sign-on.
      </p>
      <label className="check">
        <input type="checkbox" checked={f.enabled} onChange={(e) => set("enabled", e.target.checked)} />
        <span>
          <b>Offer single sign-on on the sign-in page</b>
          <small>Passwords and passkeys keep working next to it.</small>
        </span>
      </label>
      <div className="fields">
        <div className="field">
          <label htmlFor="sso-provider">Provider</label>
          <select id="sso-provider" className="input" value={f.provider} onChange={(e) => set("provider", e.target.value as SSOProvider)}
            aria-invalid={!!err("provider")} aria-describedby={err("provider") ? "sso-provider-error" : undefined}>
            {providers.map((p) => <option key={p} value={p}>{providerLabel[p]}</option>)}
          </select>
          {err("provider") && <span id="sso-provider-error" className="field-error" role="alert">{err("provider")}</span>}
        </div>
        {f.provider === "microsoft" && (
          <Field id="sso-tenant" label="Directory (tenant) ID" className="mono" value={f.tenant} onChange={(e) => set("tenant", e.target.value)}
            autoComplete="off" spellCheck={false} placeholder="00000000-0000-0000-0000-000000000000" error={err("tenant")}
            hint="On the app registration's Overview page." />
        )}
        {(f.provider === "keycloak" || f.provider === "oidc") && (
          <Field id="sso-issuer" label="Issuer URL" className="mono" value={f.issuer} onChange={(e) => set("issuer", e.target.value)}
            autoComplete="off" spellCheck={false} error={err("issuer")}
            placeholder={f.provider === "keycloak" ? "https://sso.example.com/realms/<realm>" : "https://idp.example.com"}
            hint={f.provider === "keycloak" ? "The realm's URL." : "Where the provider serves /.well-known/openid-configuration."} />
        )}
        <Field id="sso-client-id" label="Client ID" className="mono" value={f.clientId} onChange={(e) => set("clientId", e.target.value)}
          autoComplete="off" spellCheck={false} error={err("clientId")} />
        <Field id="sso-client-secret" label="Client secret" className="mono" type="password" value={secret}
          onChange={(e) => { setSecret(e.target.value); setDone(false); setError((x) => (x?.field === "clientSecret" ? undefined : x)); }}
          autoComplete="off" spellCheck={false} required={needsSecret} error={err("clientSecret")}
          placeholder={s.secretSet ? "Saved — leave empty to keep" : undefined} hint="Stored in the cluster and never shown again." />
        <Field id="sso-label" label="Button label" value={f.displayName} onChange={(e) => set("displayName", e.target.value)} maxLength={40}
          autoComplete="off" placeholder={providerButton[f.provider]} error={err("displayName")}
          hint={`The sign-in page says “Sign in with ${f.displayName.trim() || providerButton[f.provider]}”.`} />
        <Field id="sso-domains" label="Allowed email domains" className="mono" value={f.domains} onChange={(e) => set("domains", e.target.value)}
          autoComplete="off" spellCheck={false} placeholder="example.com" error={err("allowedDomains")}
          hint="Separated by commas or spaces." />
        <div className="full">
          <label className="check">
            <input type="checkbox" checked={f.autoJoin} onChange={(e) => set("autoJoin", e.target.checked)} />
            <span>
              <b>Let new people join on their first sign-in</b>
              <small>Needs allowed domains. Without it, only members and people with an open invite can sign in.</small>
            </span>
          </label>
        </div>
        {f.autoJoin && (
          <div className="field">
            <label htmlFor="sso-role">Role for people who join</label>
            <select id="sso-role" className="input" value={f.defaultRole} onChange={(e) => set("defaultRole", e.target.value as Form["defaultRole"])}
              aria-invalid={!!err("defaultRole")} aria-describedby="sso-role-note">
              <option value="developer">Developer</option>
              <option value="viewer">Viewer</option>
            </select>
            {err("defaultRole") ? <span id="sso-role-note" className="field-error" role="alert">{err("defaultRole")}</span>
              : <span id="sso-role-note" className="hint">Owners and admins can change it later under Access.</span>}
          </div>
        )}
      </div>
      <div className="field">
        <label>Redirect URL</label>
        <div className="secret-box"><code>{s.redirectUrl}</code><CopyButton text={s.redirectUrl} /></div>
        <span className="hint">{providerHint[f.provider] ?? "Register a confidential OpenID Connect client (authorization code flow) and add the redirect URL."}</span>
      </div>
      {error && !fieldKeys.includes(error.field ?? "") && <p className="form-error" role="alert">{error.message}</p>}
      {done && <p className="ok-text" role="status">Saved.</p>}
      <div className="actions">
        <button className="btn pri" disabled={busy || !changed}>{busy ? "Saving…" : "Save"}</button>
      </div>
    </form>
  );
}

// ---- data key (owners) ------------------------------------------------------------

export function DataKeyCard() {
  const q = useQuery({ queryKey: dataKeyKey, queryFn: identityApi.dataKey });
  const [rotating, setRotating] = useState(false);
  const [rotated, setRotated] = useState<DataKeyRotated>();
  const k = q.data;
  return (
    <section className="card">
      <h2>Data key</h2>
      <div className="bd stack">
        <p className="dim note">
          The data key encrypts authenticator-app secrets in the console's database. Keep a copy with your database backups: a backup cannot be read without it.
        </p>
        {q.isPending && <p className="dim note">Loading…</p>}
        {q.isError && <div className="banner bad" role="alert"><Icon name="alert" /><span>{errText(q.error)}</span></div>}
        {k && !k.available && <p className="dim note">Not available: the console has no data key, or no cluster connection to manage it.</p>}
        {k?.available && (
          <>
            <div className="current">
              <div>
                <span className="k">Key ID</span>
                <span className="host">{k.keyId}</span>
              </div>
              {k.canRotate && <button className="btn" onClick={() => setRotating(true)}><Icon name="restart" />Rotate data key</button>}
            </div>
            <p className="note">
              {k.upToDate} of {k.sealed} {k.sealed === 1 ? "secret uses" : "secrets use"} it. Kept in the Secret <code>{k.secret}</code>.
            </p>
            {k.otherKeyIds.length > 0 && <p className="note warn-text">Previous key still configured: <code>{k.otherKeyIds.join(", ")}</code></p>}
            {rotated && <Rotated r={rotated} secret={k.secret} />}
          </>
        )}
      </div>
      {rotating && <RotateDialog onClose={() => setRotating(false)} onRotated={(r) => { setRotating(false); setRotated(r); }} />}
    </section>
  );
}

function Rotated({ r, secret }: { r: DataKeyRotated; secret: string }) {
  return (
    <div className="store-now" role="status">
      <p>
        <b>The data key is now {r.keyId}.</b> {r.resealed} {r.resealed === 1 ? "secret was" : "secrets were"} encrypted again
        {r.unreadable > 0 ? `; ${r.unreadable} could not be read with any configured key and stay as they were` : ""}.
        {r.previousRetired ? " The previous key is no longer configured." : ""}
      </p>
      <p>
        Back up the Secret <code>{secret}</code> again together with the database. Backups made before now need the previous key <code>{r.previousKeyId}</code>.
      </p>
    </div>
  );
}

function RotateDialog({ onClose, onRotated }: { onClose: () => void; onRotated: (r: DataKeyRotated) => void }) {
  const queryClient = useQueryClient();
  const account = useQuery({ queryKey: ["account"], queryFn: accountApi.get });
  const hasPassword = account.data?.hasPassword ?? true;
  const [password, setPassword] = useState("");
  const [error, setError] = useState<{ field?: string; message: string }>();
  const [busy, setBusy] = useState(false);

  async function rotate() {
    setBusy(true);
    setError(undefined);
    try {
      const r = await identityApi.rotateDataKey(password);
      await queryClient.invalidateQueries({ queryKey: dataKeyKey });
      onRotated(r);
    } catch (err) {
      setError(err instanceof ApiError ? { field: err.field, message: err.message } : { message: unreachable });
      setBusy(false);
    }
  }

  return (
    <Dialog title="Rotate the data key?" onClose={onClose} onSubmit={rotate}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={busy}>{busy ? "Rotating…" : "Rotate data key"}</button>
      </>}>
      <p className="note">
        Kwerft creates a new key and encrypts every authenticator-app secret with it. Sign-in keeps working throughout.
      </p>
      <Field id="dk-password" label="Confirm with your password" type="password" value={password} onChange={(e) => setPassword(e.target.value)}
        autoComplete="current-password" autoFocus required={hasPassword} error={error?.field === "password" ? error.message : undefined}
        hint={hasPassword ? undefined : ssoPasswordHint} />
      {error && error.field !== "password" && <p className="form-error" role="alert">{error.message}</p>}
    </Dialog>
  );
}
