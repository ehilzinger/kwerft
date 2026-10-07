// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { useState, type FormEvent } from "react";
import { useNavigate } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../api";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";

type Step = "token" | "owner";

const steps: { id: Step | "domains" | "hetzner"; title: string; note: string }[] = [
  { id: "token", title: "Setup token", note: "Proves you control the server" },
  { id: "owner", title: "Owner account", note: "The first person with full access" },
  { id: "domains", title: "Domains & TLS", note: "Later, in Settings" },
  { id: "hetzner", title: "Hetzner integration", note: "Later, in Settings" },
];

export function Setup() {
  const [step, setStep] = useState<Step>("token");
  const status = useQuery({ queryKey: ["setup"], queryFn: api.setupStatus });
  const domain = status.data?.consoleDomain;
  // The DNS hint follows the console setting: a temporary sslip.io name needs
  // a real one later, in Settings, where the apps domain is set too.
  const domainNote = !domain ? "Later, in Settings"
    : domain.endsWith(".sslip.io") ? `Temporary ${domain} · your own in Settings`
    : `Console at ${domain} · apps domain in Settings`;
  return (
    <div className="setup">
      <aside>
        <div className="brand"><Icon name="logo" />Kwerft setup</div>
        <ol className="vsteps">
          {steps.map((s, i) => {
            const state = s.id === step ? "cur" : s.id === "token" && step === "owner" ? "done" : "";
            const later = s.id === "domains" || s.id === "hetzner";
            return (
              <li key={s.id} className={`${state} ${later ? "later" : ""}`} aria-current={state === "cur" ? "step" : undefined}>
                <span className="dot">{state === "done" ? "✓" : i + 1}</span>
                <div><b>{s.title}</b><small>{s.id === "domains" ? domainNote : s.note}</small></div>
              </li>
            );
          })}
        </ol>
      </aside>
      <main>{step === "token" ? <TokenStep onDone={() => setStep("owner")} /> : <OwnerStep />}</main>
    </div>
  );
}

const tokenCommand = "sudo cat /etc/kwerft/setup-token";

function TokenStep({ onDone }: { onDone: () => void }) {
  const [token, setToken] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [copied, setCopied] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await api.setupVerify(token);
      onDone();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "The console could not be reached. Check your connection and try again.");
    } finally {
      setBusy(false);
    }
  }

  async function copy() {
    try {
      await navigator.clipboard.writeText(tokenCommand);
      setCopied(true);
    } catch {
      /* clipboard refused: the command stays selectable */
    }
  }

  return (
    <form className="setup-form" onSubmit={submit}>
      <div>
        <h1>Enter your setup token</h1>
        <p className="lead">The installer saved a one-time token on the server. It proves you control the machine. Run this on the server and paste the result:</p>
      </div>
      <div className="command">
        <code>{tokenCommand}</code>
        <button type="button" className="btn sm" onClick={copy}>{copied ? "Copied" : "Copy"}</button>
      </div>
      <Field id="setup-token" label="Setup token" className="mono" value={token} onChange={(e) => setToken(e.target.value)}
        placeholder="kwft_setup_…" autoComplete="off" spellCheck={false} autoFocus required error={error}
        hint="Valid for 24 hours, and only until the owner account exists." />
      <div className="actions">
        <button className="btn pri" disabled={busy || token.trim() === ""}>{busy ? "Checking…" : "Continue"}</button>
      </div>
    </form>
  );
}

function OwnerStep() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [form, setForm] = useState({ name: "", email: "", password: "", confirm: "" });
  const [error, setError] = useState<{ field?: string; message: string }>();
  const [busy, setBusy] = useState(false);

  const set = (k: keyof typeof form) => (e: { target: { value: string } }) => setForm({ ...form, [k]: e.target.value });

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (form.password !== form.confirm) {
      setError({ field: "confirm", message: "The passwords don't match." });
      return;
    }
    setBusy(true);
    setError(undefined);
    try {
      const user = await api.setupOwner({ name: form.name, email: form.email, password: form.password });
      queryClient.setQueryData(["setup"], { complete: true });
      queryClient.setQueryData(["session"], user);
      await navigate({ to: "/" });
    } catch (err) {
      if (err instanceof ApiError) setError({ field: err.field, message: err.message });
      else setError({ message: "The console could not be reached. Check your connection and try again." });
      setBusy(false);
    }
  }

  const fieldError = (f: string) => (error?.field === f ? error.message : undefined);

  return (
    <form className="setup-form" onSubmit={submit}>
      <div>
        <h1>Create the owner account</h1>
        <p className="lead">The owner can do everything in this console, including inviting the rest of your team afterwards.</p>
      </div>
      <div className="fields">
        <Field id="owner-name" label="Name" value={form.name} onChange={set("name")} autoComplete="name" autoFocus required error={fieldError("name")} />
        <Field id="owner-email" label="Email" type="email" value={form.email} onChange={set("email")} autoComplete="email" required error={fieldError("email")} />
        <Field id="owner-password" label="Password" type="password" value={form.password} onChange={set("password")} autoComplete="new-password"
          required minLength={12} error={fieldError("password")} hint="At least 12 characters. A few random words work well." />
        <Field id="owner-confirm" label="Repeat password" type="password" value={form.confirm} onChange={set("confirm")} autoComplete="new-password"
          required error={fieldError("confirm")} />
      </div>
      {error && !error.field && <p className="form-error" role="alert">{error.message}</p>}
      <div className="actions">
        <button className="btn pri" disabled={busy}>{busy ? "Creating account…" : "Create account and open the console"}</button>
      </div>
    </form>
  );
}
