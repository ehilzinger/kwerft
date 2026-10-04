import { useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useRouter } from "@tanstack/react-router";
import { ApiError, api } from "../api";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { loginRoute } from "../router";

export function Login() {
  const { next } = loginRoute.useSearch();
  const router = useRouter();
  const queryClient = useQueryClient();
  const version = useQuery({ queryKey: ["version"], queryFn: api.version, staleTime: Infinity });
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      const user = await api.login(email, password);
      queryClient.setQueryData(["session"], user);
      router.history.push(next);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "The console could not be reached. Check your connection and try again.");
      setBusy(false);
    }
  }

  return (
    <div className="auth-page">
      <form className="auth-card" onSubmit={submit}>
        <div className="brand"><Icon name="logo" />Kwerft</div>
        <h1>Sign in</h1>
        <Field id="login-email" label="Email" type="email" value={email} onChange={(e) => setEmail(e.target.value)} autoComplete="username" autoFocus required />
        <Field id="login-password" label="Password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" required />
        {error && <p className="form-error" role="alert">{error}</p>}
        <button className="btn pri wide" disabled={busy}>{busy ? "Signing in…" : "Sign in"}</button>
        <p className="auth-foot">{version.data ? `Kwerft ${version.data.version}` : " "}</p>
      </form>
    </div>
  );
}
