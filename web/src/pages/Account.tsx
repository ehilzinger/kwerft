// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { createContext, useContext, useState, type FormEvent, type ReactNode } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, accountApi, api, type Account as AccountData, type AccountSession, type Passkey, type SSOIdentity, type TOTPSetup, type User } from "../api";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { identityApi, issuerHost, ssoPasswordHint } from "../identity";
import { createPasskey, passkeyErrorMessage, passkeysSupported } from "../webauthn";
import { TokensCard } from "./AccountTokens";

const unreachable = "The console could not be reached. Check your connection and try again.";
const errText = (e: unknown) => (e instanceof ApiError ? e.message : unreachable);
const fmt = (iso: string) => new Date(iso).toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });

// False for an account without a password: it confirms changes with an
// empty password soon after signing in with single sign-on.
const HasPassword = createContext(true);

// The account page: profile, password, single sign-on, two-factor sign-in,
// API tokens and sessions.
export function Account() {
  const account = useQuery({ queryKey: ["account"], queryFn: accountApi.get });
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  if (account.isPending) return <div className="view"><p className="dim">Loading…</p></div>;
  if (account.isError) return <div className="view"><p className="form-error" role="alert">{errText(account.error)}</p></div>;
  const a = account.data;
  return (
    <div className="view account">
      <div className="ph">
        <div>
          <h1>Your account</h1>
          <p>{a.user.email} · <span className="cap">{a.user.role}</span></p>
        </div>
      </div>
      {session.data?.mustEnrol && (
        <div className="banner warn" role="alert">
          <Icon name="alert" />
          <span>This console requires two-factor sign-in. Add a passkey or an authenticator app below to continue; the rest of the console opens once you have one.</span>
        </div>
      )}
      <HasPassword.Provider value={a.hasPassword}>
        <ProfileCard user={a.user} />
        <PasswordCard hasPassword={a.hasPassword} />
        {a.identities.length > 0 && <SSOCard identities={a.identities} hasPassword={a.hasPassword} />}
        <TwoFactorCard account={a} />
        <TokensCard role={a.user.role} />
        <SessionsCard sessions={a.sessions} />
      </HasPassword.Provider>
    </div>
  );
}

function useRefresh() {
  const queryClient = useQueryClient();
  // The session too: a new second factor ends "set one up first" (mustEnrol).
  return () => Promise.all([
    queryClient.invalidateQueries({ queryKey: ["account"] }),
    queryClient.invalidateQueries({ queryKey: ["session"] }),
  ]);
}

function Card({ title, children, note }: { title: string; note?: ReactNode; children: ReactNode }) {
  return (
    <section className="card">
      <h2>{title}</h2>
      <div className="bd stack">
        {note && <p className="dim note">{note}</p>}
        {children}
      </div>
    </section>
  );
}

// ---- profile and password ----------------------------------------------------

function ProfileCard({ user }: { user: User }) {
  const queryClient = useQueryClient();
  const refresh = useRefresh();
  const [name, setName] = useState(user.name);
  const [error, setError] = useState<string>();
  const [saved, setSaved] = useState(false);
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    setSaved(false);
    try {
      queryClient.setQueryData(["session"], await accountApi.rename(name));
      await refresh();
      setSaved(true);
    } catch (err) {
      setError(errText(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card title="Profile">
      <form className="inline-form" onSubmit={submit}>
        <Field id="account-name" label="Name" value={name} onChange={(e) => setName(e.target.value)} autoComplete="name" required error={error} />
        <button className="btn" disabled={busy || name.trim() === user.name}>{busy ? "Saving…" : "Save"}</button>
      </form>
      {saved && <p className="ok-text" role="status">Name saved.</p>}
    </Card>
  );
}

function PasswordCard({ hasPassword }: { hasPassword: boolean }) {
  if (!hasPassword) return <SetPasswordCard />;
  return <ChangePasswordCard />;
}

function ChangePasswordCard() {
  const refresh = useRefresh();
  const [form, setForm] = useState({ current: "", next: "", confirm: "" });
  const [error, setError] = useState<{ field?: string; message: string }>();
  const [done, setDone] = useState<string>();
  const [busy, setBusy] = useState(false);
  const set = (k: keyof typeof form) => (e: { target: { value: string } }) => setForm({ ...form, [k]: e.target.value });
  const fieldError = (f: string) => (error?.field === f ? error.message : undefined);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setDone(undefined);
    if (form.next !== form.confirm) {
      setError({ field: "confirm", message: "The new passwords don't match." });
      return;
    }
    setBusy(true);
    setError(undefined);
    try {
      const { signedOut } = await accountApi.changePassword(form.current, form.next);
      setForm({ current: "", next: "", confirm: "" });
      setDone(signedOut > 0 ? `Password changed. ${signedOut} other ${signedOut === 1 ? "session was" : "sessions were"} signed out.` : "Password changed.");
      await refresh();
    } catch (err) {
      setError(err instanceof ApiError ? { field: err.field, message: err.message } : { message: unreachable });
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card title="Password" note="Changing your password signs you out everywhere else.">
      <form className="fields3" onSubmit={submit}>
        <Field id="pw-current" label="Current password" type="password" value={form.current} onChange={set("current")}
          autoComplete="current-password" required error={fieldError("current")} />
        <Field id="pw-new" label="New password" type="password" value={form.next} onChange={set("next")}
          autoComplete="new-password" required minLength={12} error={fieldError("new")} hint="At least 12 characters." />
        <Field id="pw-confirm" label="Repeat new password" type="password" value={form.confirm} onChange={set("confirm")}
          autoComplete="new-password" required error={fieldError("confirm")} />
        <div className="actions">
          <button className="btn pri" disabled={busy}>{busy ? "Changing…" : "Change password"}</button>
        </div>
      </form>
      {error && !error.field && <p className="form-error" role="alert">{error.message}</p>}
      {done && <p className="ok-text" role="status">{done}</p>}
    </Card>
  );
}

// An account made by single sign-on has no password. It may set one soon
// after signing in; the server then needs no current password.
function SetPasswordCard() {
  const refresh = useRefresh();
  const [next, setNext] = useState("");
  const [error, setError] = useState<{ field?: string; message: string }>();
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await accountApi.changePassword("", next);
      setNext("");
      await refresh();
    } catch (err) {
      setError(err instanceof ApiError ? { field: err.field, message: err.message } : { message: unreachable });
    } finally {
      setBusy(false);
    }
  }

  return (
    <Card title="Password" note="Your account signs in with single sign-on. Set a password to also sign in without it.">
      <form className="inline-form" onSubmit={submit}>
        <Field id="pw-set" label="New password" type="password" value={next} onChange={(e) => setNext(e.target.value)}
          autoComplete="new-password" required minLength={12} error={error?.field === "new" ? error.message : undefined}
          hint="At least 12 characters. Works within 10 minutes of signing in with single sign-on." />
        <button className="btn pri" disabled={busy || next === ""}>{busy ? "Setting…" : "Set a password"}</button>
      </form>
      {error && error.field !== "new" && <p className="form-error" role="alert">{error.message}</p>}
    </Card>
  );
}

// ---- single sign-on ---------------------------------------------------------------

function SSOCard({ identities, hasPassword }: { identities: SSOIdentity[]; hasPassword: boolean }) {
  return (
    <Card title="Single sign-on" note="Accounts at a sign-in provider linked to yours. Kwerft links one the first time you sign in with it.">
      <ul className="rows">{identities.map((i) => <IdentityRow key={i.issuer} identity={i} canUnlink={hasPassword} />)}</ul>
      {!hasPassword && <p className="dim note">Set a password before you unlink single sign-on, so you can still sign in.</p>}
    </Card>
  );
}

function IdentityRow({ identity, canUnlink }: { identity: SSOIdentity; canUnlink: boolean }) {
  const refresh = useRefresh();
  const [unlinking, setUnlinking] = useState(false);
  const host = issuerHost(identity.issuer);
  return (
    <li>
      <div className="row">
        <Icon name="shield" />
        <div className="grow">
          <b>{host}</b>
          <small>
            {identity.email} · linked {fmt(identity.linkedAt)} · {identity.lastLoginAt ? `last sign-in ${fmt(identity.lastLoginAt)}` : "never used to sign in"}
          </small>
        </div>
        {canUnlink && !unlinking && <button className="btn sm danger" onClick={() => setUnlinking(true)}>Unlink</button>}
      </div>
      {unlinking && (
        <Confirm id={`sso-unlink-${host}`} submit="Unlink" onCancel={() => setUnlinking(false)}
          run={async (pw) => { await identityApi.unlink(identity.issuer, pw); await refresh(); }}>
          <p className="note">You then sign in with your password. Signing in with {host} again links it again.</p>
        </Confirm>
      )}
    </li>
  );
}

// ---- two-factor sign-in --------------------------------------------------------

// Confirm asks for the password (or a current authenticator code) before a
// change to how the account signs in. An account without a password may
// leave it empty soon after signing in with single sign-on.
function Confirm({ id, label, submit, run, onCancel, children }: {
  id: string;
  label?: string;
  submit: string;
  run: (password: string) => Promise<void>;
  onCancel: () => void;
  children?: ReactNode;
}) {
  const hasPassword = useContext(HasPassword);
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function go(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await run(password);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : passkeyErrorMessage(err));
      setBusy(false);
    }
  }

  return (
    <form className="confirm" onSubmit={go}>
      {children}
      <Field id={id} label={label ?? "Confirm with your password"} type="password" value={password}
        onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" autoFocus required={hasPassword} error={error}
        hint={hasPassword ? undefined : ssoPasswordHint} />
      <div className="actions">
        <button type="button" className="btn" onClick={onCancel} disabled={busy}>Cancel</button>
        <button className="btn pri" disabled={busy}>{busy ? "Working…" : submit}</button>
      </div>
    </form>
  );
}

function TwoFactorCard({ account }: { account: AccountData }) {
  // Recovery codes are shown once, right after they are generated.
  const [codes, setCodes] = useState<string[]>();
  const anyFactor = account.totp || account.passkeys.length > 0;
  return (
    <Card title="Two-factor sign-in"
      note="A second factor keeps your account safe even if your password leaks. Add a passkey, an authenticator app, or both.">
      {codes && <RecoveryCodes codes={codes} onClose={() => setCodes(undefined)} />}
      <PasskeysSection account={account} onCodes={setCodes} />
      <TOTPSection account={account} onCodes={setCodes} />
      {anyFactor && <RecoverySection left={account.recoveryCodesLeft} onCodes={setCodes} />}
    </Card>
  );
}

function RecoveryCodes({ codes, onClose }: { codes: string[]; onClose: () => void }) {
  const [copied, setCopied] = useState(false);
  async function copy() {
    try {
      await navigator.clipboard.writeText(codes.join("\n"));
      setCopied(true);
    } catch {
      /* clipboard refused: the codes stay selectable */
    }
  }
  return (
    <div className="recovery" role="alert">
      <b>Save your recovery codes</b>
      <p>If you lose your passkeys and phone, each of these codes signs you in once. Keep them in a password manager or on paper. They are not shown again.</p>
      <ol className="codes">{codes.map((c) => <li key={c}><code>{c}</code></li>)}</ol>
      <div className="actions">
        <button type="button" className="btn" onClick={copy}>{copied ? "Copied" : "Copy all"}</button>
        <button type="button" className="btn pri" onClick={onClose}>I've saved them</button>
      </div>
    </div>
  );
}

type OnCodes = (codes: string[]) => void;

function TOTPSection({ account, onCodes }: { account: AccountData; onCodes: OnCodes }) {
  const refresh = useRefresh();
  const [mode, setMode] = useState<"idle" | "start" | "disable">("idle");
  const [setup, setSetup] = useState<TOTPSetup>();

  const status = account.totp ? <span className="pill ok">On</span> : <span className="pill off">Off</span>;
  return (
    <div className="factor">
      <div className="factor-head">
        <div><b>Authenticator app</b> {status}<small>Codes from an app such as 1Password, Bitwarden, Aegis or Google Authenticator.</small></div>
        {mode === "idle" && !setup && (account.totp
          ? <button className="btn" onClick={() => setMode("disable")}>Turn off</button>
          : <button className="btn" onClick={() => setMode("start")} disabled={!account.available.totp}>Set up</button>)}
      </div>
      {!account.available.totp && !account.totp && (
        <p className="dim note">Not available: the console has no data key (KWERFT_DATA_KEY) to encrypt the app's secret with.</p>
      )}
      {mode === "start" && (
        <Confirm id="totp-start-pw" submit="Continue" onCancel={() => setMode("idle")}
          run={async (pw) => { setSetup(await accountApi.totpStart(pw)); setMode("idle"); }} />
      )}
      {mode === "disable" && (
        <Confirm id="totp-disable-pw" label="Password or current authenticator code" submit="Turn off" onCancel={() => setMode("idle")}
          run={async (pw) => { await accountApi.totpDisable(pw); await refresh(); setMode("idle"); }} />
      )}
      {setup && (
        <TOTPEnroll setup={setup} onCancel={() => setSetup(undefined)}
          onDone={async (codes) => { setSetup(undefined); if (codes) onCodes(codes); await refresh(); }} />
      )}
    </div>
  );
}

function TOTPEnroll({ setup, onDone, onCancel }: { setup: TOTPSetup; onDone: (codes?: string[]) => void; onCancel: () => void }) {
  const [code, setCode] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      onDone((await accountApi.totpConfirm(code)).recoveryCodes);
    } catch (err) {
      setError(errText(err));
      setBusy(false);
    }
  }

  return (
    <form className="enroll" onSubmit={submit}>
      <img className="qr" src={setup.qr} width={168} height={168} alt="QR code to add Kwerft to your authenticator app" />
      <div className="stack">
        <p>Scan the code with your authenticator app, then enter the 6-digit code it shows.</p>
        <p className="dim">Can't scan? Enter this key instead: <code className="secret">{setup.secret}</code></p>
        <Field id="totp-code" label="Code from the app" className="mono" value={code} onChange={(e) => setCode(e.target.value)}
          inputMode="numeric" autoComplete="one-time-code" placeholder="123 456" autoFocus required error={error} />
        <div className="actions">
          <button type="button" className="btn" onClick={onCancel} disabled={busy}>Cancel</button>
          <button className="btn pri" disabled={busy || code.trim() === ""}>{busy ? "Checking…" : "Turn on"}</button>
        </div>
      </div>
    </form>
  );
}

function PasskeysSection({ account, onCodes }: { account: AccountData; onCodes: OnCodes }) {
  const refresh = useRefresh();
  const [adding, setAdding] = useState(false);
  const [name, setName] = useState("");
  const supported = passkeysSupported();
  const canAdd = account.available.passkeys && supported;

  async function add(password: string) {
    const options = await accountApi.passkeyBegin(password);
    const credential = await createPasskey(options);
    const res = await accountApi.passkeyFinish(name, credential);
    setAdding(false);
    setName("");
    if (res.recoveryCodes) onCodes(res.recoveryCodes);
    await refresh();
  }

  return (
    <div className="factor">
      <div className="factor-head">
        <div>
          <b>Passkeys</b> {account.passkeys.length > 0 ? <span className="pill ok">{account.passkeys.length}</span> : <span className="pill off">None</span>}
          <small>Sign in with your fingerprint, face or device PIN, or a security key. Passkeys also work without a password.</small>
        </div>
        {!adding && <button className="btn" onClick={() => setAdding(true)} disabled={!canAdd}><Icon name="plus" />Add passkey</button>}
      </div>
      {!account.available.passkeys && <p className="dim note">Not available: the console was started without its domain (--console-domain).</p>}
      {account.available.passkeys && !supported && <p className="dim note">This browser does not support passkeys.</p>}
      {adding && (
        <Confirm id="passkey-add-pw" submit="Create passkey" onCancel={() => setAdding(false)} run={add}>
          <Field id="passkey-name" label="Name" value={name} onChange={(e) => setName(e.target.value)} maxLength={60}
            placeholder="e.g. MacBook, YubiKey" hint="Helps you tell your passkeys apart." />
        </Confirm>
      )}
      {account.passkeys.length > 0 && (
        <ul className="rows">{account.passkeys.map((p) => <PasskeyRow key={p.id} passkey={p} />)}</ul>
      )}
    </div>
  );
}

function PasskeyRow({ passkey }: { passkey: Passkey }) {
  const refresh = useRefresh();
  const [mode, setMode] = useState<"idle" | "rename" | "remove">("idle");
  const [name, setName] = useState(passkey.name);
  const [error, setError] = useState<string>();

  async function rename(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    try {
      await accountApi.renamePasskey(passkey.id, name);
      await refresh();
      setMode("idle");
    } catch (err) {
      setError(errText(err));
    }
  }

  return (
    <li>
      {mode === "rename" ? (
        <form className="inline-form" onSubmit={rename}>
          <Field id={`pk-name-${passkey.id}`} label="Name" value={name} onChange={(e) => setName(e.target.value)} maxLength={60} autoFocus error={error} />
          <button type="button" className="btn" onClick={() => setMode("idle")}>Cancel</button>
          <button className="btn pri">Save</button>
        </form>
      ) : (
        <div className="row">
          <Icon name="key" />
          <div className="grow">
            <b>{passkey.name}</b>
            <small>Added {fmt(passkey.createdAt)} · {passkey.lastUsedAt ? `last used ${fmt(passkey.lastUsedAt)}` : "never used"}</small>
          </div>
          {mode === "idle" && (
            <>
              <button className="btn sm" onClick={() => setMode("rename")}>Rename</button>
              <button className="btn sm danger" onClick={() => setMode("remove")}>Remove</button>
            </>
          )}
        </div>
      )}
      {mode === "remove" && (
        <Confirm id={`pk-remove-${passkey.id}`} submit="Remove passkey" onCancel={() => setMode("idle")}
          run={async (pw) => { await accountApi.removePasskey(passkey.id, pw); await refresh(); }} />
      )}
    </li>
  );
}

function RecoverySection({ left, onCodes }: { left: number; onCodes: OnCodes }) {
  const refresh = useRefresh();
  const [regenerating, setRegenerating] = useState(false);
  return (
    <div className="factor">
      <div className="factor-head">
        <div>
          <b>Recovery codes</b> <span className={`pill ${left > 2 ? "ok" : "warn"}`}>{left} left</span>
          <small>For when your passkeys and authenticator app are out of reach. New codes replace all old ones.</small>
        </div>
        {!regenerating && <button className="btn" onClick={() => setRegenerating(true)}>Generate new codes</button>}
      </div>
      {regenerating && (
        <Confirm id="recovery-pw" label="Password or current authenticator code" submit="Generate new codes" onCancel={() => setRegenerating(false)}
          run={async (pw) => { const res = await accountApi.regenerateCodes(pw); setRegenerating(false); onCodes(res.recoveryCodes); await refresh(); }} />
      )}
    </div>
  );
}

// ---- sessions -------------------------------------------------------------------

function SessionsCard({ sessions }: { sessions: AccountSession[] }) {
  const refresh = useRefresh();
  const [error, setError] = useState<string>();
  const others = sessions.filter((s) => !s.current).length;

  async function act(f: () => Promise<unknown>) {
    setError(undefined);
    try {
      await f();
      await refresh();
    } catch (err) {
      setError(errText(err));
    }
  }

  return (
    <Card title="Signed-in sessions" note="Every browser where your account is signed in. Sign out any you don't recognize, then change your password.">
      <ul className="rows">
        {sessions.map((s) => (
          <li key={s.id}>
            <div className="row">
              <Icon name="server" />
              <div className="grow">
                <b>{describeAgent(s.userAgent)}</b> {s.current && <span className="pill ok">This browser</span>}
                <small>{s.ip} · signed in {fmt(s.createdAt)} · last active {fmt(s.lastSeenAt)}</small>
              </div>
              {!s.current && <button className="btn sm" onClick={() => act(() => accountApi.signOutSession(s.id))}>Sign out</button>}
            </div>
          </li>
        ))}
      </ul>
      {error && <p className="form-error" role="alert">{error}</p>}
      {others > 0 && (
        <div className="actions">
          <button className="btn" onClick={() => act(accountApi.signOutOthers)}>Sign out all other sessions</button>
        </div>
      )}
    </Card>
  );
}

// describeAgent turns a User-Agent header into "Firefox on macOS".
function describeAgent(ua: string) {
  const browser = /Edg\//.test(ua) ? "Edge" : /Firefox\//.test(ua) ? "Firefox" : /Chrome\//.test(ua) ? "Chrome"
    : /Safari\//.test(ua) ? "Safari" : /^curl\//.test(ua) ? "curl" : "";
  const os = /iPhone|iPad/.test(ua) ? "iOS" : /Android/.test(ua) ? "Android" : /Mac OS X/.test(ua) ? "macOS"
    : /Windows/.test(ua) ? "Windows" : /Linux/.test(ua) ? "Linux" : "";
  return [browser, os].filter(Boolean).join(" on ") || ua || "Unknown browser";
}
