// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { useState, type FormEvent } from "react";
import { Link, useNavigate } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, type User } from "../api";
import { accessApi, roleLabel, roleNote } from "../access";
import { when } from "../jobs";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { inviteRoute } from "../router";
import "../styles/access.css";

const unreachable = "The console could not be reached. Check your connection and try again.";
const errText = (e: unknown) => (e instanceof ApiError ? e.message : unreachable);
const article = (role: string) => (role === "admin" || role === "owner" ? "an" : "a");

// /invite/<token>: someone was invited; they choose a name and password and
// are signed in. Public, like the sign-in page.
export function InviteAccept() {
  const { token } = inviteRoute.useParams();
  const [joined, setJoined] = useState<User>();
  const info = useQuery({
    queryKey: ["invite", token],
    queryFn: () => accessApi.lookupInvite(token),
    retry: false,
    staleTime: Infinity,
    enabled: !joined,
  });

  return (
    <div className="auth-page">
      {joined ? <Welcome user={joined} /> : info.isPending ? (
        <div className="auth-card"><Brand /><p className="dim">Checking your invite…</p></div>
      ) : info.isError ? (
        <div className="auth-card">
          <Brand />
          <h1>This invite can't be used</h1>
          <p className="form-error" role="alert">{errText(info.error)}</p>
          <Link to="/login" search={{ next: "/" }} className="btn wide">Go to sign in</Link>
        </div>
      ) : (
        <AcceptForm token={token} info={info.data} onJoined={setJoined} />
      )}
    </div>
  );
}

function Brand() {
  return <div className="brand"><Icon name="logo" />Kwerft</div>;
}

function AcceptForm({ token, info, onJoined }: {
  token: string;
  info: { email: string; role: User["role"]; invitedBy: string; expiresAt: string };
  onJoined: (u: User) => void;
}) {
  const queryClient = useQueryClient();
  const [form, setForm] = useState({ name: "", password: "", confirm: "" });
  const [error, setError] = useState<{ field?: string; message: string }>();
  const [busy, setBusy] = useState(false);
  const set = (k: keyof typeof form) => (e: { target: { value: string } }) => setForm({ ...form, [k]: e.target.value });
  const fieldError = (f: string) => (error?.field === f ? error.message : undefined);

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (form.password !== form.confirm) {
      setError({ field: "confirm", message: "The passwords don't match." });
      return;
    }
    setBusy(true);
    setError(undefined);
    try {
      const user = await accessApi.acceptInvite(token, form.name, form.password);
      queryClient.setQueryData(["session"], user);
      onJoined(user);
    } catch (err) {
      setError(err instanceof ApiError ? { field: err.field, message: err.message } : { message: unreachable });
      setBusy(false);
    }
  }

  return (
    <form className="auth-card" onSubmit={submit}>
      <Brand />
      <div>
        <h1>Join the team</h1>
        <p className="lead">
          <b>{info.invitedBy}</b> invited you to this console as {article(info.role)} <b>{roleLabel[info.role].toLowerCase()}</b>: {roleNote[info.role].toLowerCase()}.
        </p>
      </div>
      <Field id="invite-email" label="Email" value={info.email} readOnly className="readonly" autoComplete="username"
        hint={`Your sign-in. The invite expires ${when(info.expiresAt)}.`} />
      <Field id="invite-name" label="Your name" value={form.name} onChange={set("name")} autoComplete="name" autoFocus required error={fieldError("name")} />
      <Field id="invite-password" label="Password" type="password" value={form.password} onChange={set("password")} autoComplete="new-password"
        required minLength={12} error={fieldError("password")} hint="At least 12 characters. A few random words work well." />
      <Field id="invite-confirm" label="Repeat password" type="password" value={form.confirm} onChange={set("confirm")} autoComplete="new-password"
        required error={fieldError("confirm")} />
      {error && !error.field && <p className="form-error" role="alert">{error.message}</p>}
      <button className="btn pri wide" disabled={busy}>{busy ? "Creating your account…" : "Create account and sign in"}</button>
    </form>
  );
}

function Welcome({ user }: { user: User }) {
  const navigate = useNavigate();
  return (
    <div className="auth-card welcome">
      <Brand />
      <div>
        <h1>Welcome, {user.name.split(/\s+/)[0]}</h1>
        <p className="lead">
          You're signed in as {article(user.role)} {roleLabel[user.role].toLowerCase()}. Add a second factor now — a passkey or an authenticator
          app — so a leaked password alone can't open your account.
        </p>
      </div>
      <button className="btn pri wide" onClick={() => navigate({ to: "/account" })}><Icon name="key" />Set up two-factor sign-in</button>
      <button className="btn wide" onClick={() => navigate({ to: "/" })}>Later, open the console</button>
    </div>
  );
}
