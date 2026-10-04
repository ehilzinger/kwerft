import { useEffect, useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useRouter } from "@tanstack/react-router";
import { ApiError, api, mfaApi, type SecondFactor, type User } from "../api";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { identityApi, readLoginQuery } from "../identity";
import { loginRoute } from "../router";
import { getPasskey, passkeyErrorMessage, passkeysSupported } from "../webauthn";
import "../styles/workloads.css";

const unreachable = "The console could not be reached. Check your connection and try again.";

export function Login() {
  const { next } = loginRoute.useSearch();
  const router = useRouter();
  const queryClient = useQueryClient();
  const version = useQuery({ queryKey: ["version"], queryFn: api.version, staleTime: Infinity });
  const sso = useQuery({ queryKey: ["sso"], queryFn: identityApi.sso, staleTime: 60_000, retry: false });
  // Single sign-on comes back here with the second factors still to ask
  // for (?second-factor=…) or why it failed (?sso=<code>).
  const [fromSSO] = useState(() => readLoginQuery(window.location.search));
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string>();
  const [ssoError, setSSOError] = useState(fromSSO.ssoError);
  const [busy, setBusy] = useState(false);
  // Set once the password (or single sign-on) was right and the account has a second factor.
  const [methods, setMethods] = useState<SecondFactor[] | undefined>(fromSSO.methods);

  // Drop the parameters so a reload does not show them again.
  useEffect(() => {
    if (fromSSO.changed) window.history.replaceState(window.history.state, "", `${window.location.pathname}${fromSSO.search}${window.location.hash}`);
  }, [fromSSO]);

  function signedIn(user: User) {
    queryClient.setQueryData(["session"], user);
    router.history.push(next);
  }

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    setSSOError(undefined);
    try {
      const res = await api.login(email, password);
      if ("secondFactor" in res) {
        setMethods(res.secondFactor);
        setBusy(false);
        return;
      }
      signedIn(res);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : unreachable);
      setBusy(false);
    }
  }

  async function passwordless() {
    setBusy(true);
    setError(undefined);
    setSSOError(undefined);
    try {
      const options = await mfaApi.passwordlessBegin();
      signedIn(await mfaApi.passwordlessFinish(await getPasskey(options)));
    } catch (err) {
      setError(err instanceof ApiError ? err.message : passkeyErrorMessage(err));
      setBusy(false);
    }
  }

  function restart(message?: string) {
    setMethods(undefined);
    setPassword("");
    setError(message);
  }

  return (
    <div className="auth-page">
      {methods ? (
        <SecondFactorStep methods={methods} onDone={signedIn} onRestart={restart} />
      ) : (
        <form className="auth-card" onSubmit={submit}>
          <div className="brand"><Icon name="logo" />Kwerft</div>
          <h1>Sign in</h1>
          {ssoError && <div className="banner bad" role="alert"><Icon name="alert" /><span>{ssoError}</span></div>}
          <Field id="login-email" label="Email" type="email" value={email} onChange={(e) => setEmail(e.target.value)} autoComplete="username" autoFocus required />
          <Field id="login-password" label="Password" type="password" value={password} onChange={(e) => setPassword(e.target.value)} autoComplete="current-password" required />
          {error && <p className="form-error" role="alert">{error}</p>}
          <button className="btn pri wide" disabled={busy}>{busy ? "Signing in…" : "Sign in"}</button>
          {(passkeysSupported() || sso.data?.enabled) && <div className="or" aria-hidden="true">or</div>}
          {sso.data?.enabled && (
            // A plain link: the server redirects to the provider.
            <a className="btn wide" href={"/api/v1/sso/start" + (next && next !== "/" ? "?next=" + encodeURIComponent(next) : "")} aria-disabled={busy || undefined}>
              <Icon name="shield" />Sign in with {sso.data.displayName}
            </a>
          )}
          {passkeysSupported() && (
            <button type="button" className="btn wide" onClick={passwordless} disabled={busy}>
              <Icon name="key" />Sign in with a passkey
            </button>
          )}
          <p className="auth-foot">{version.data ? `Kwerft ${version.data.version}` : " "}</p>
        </form>
      )}
    </div>
  );
}

const switchLabel: Record<SecondFactor, string> = {
  passkey: "Use a passkey",
  totp: "Use your authenticator app",
  recovery: "Use a recovery code",
};

function SecondFactorStep({ methods, onDone, onRestart }: {
  methods: SecondFactor[];
  onDone: (u: User) => void;
  onRestart: (message?: string) => void;
}) {
  const [method, setMethod] = useState<SecondFactor>(methods[0]!);
  const [code, setCode] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  // A 401 without a field means the pending sign-in is gone: back to the password.
  function fail(err: unknown) {
    if (err instanceof ApiError && err.status === 401 && !err.field) {
      onRestart(err.message);
      return;
    }
    setError(err instanceof ApiError ? err.message : method === "passkey" ? passkeyErrorMessage(err) : unreachable);
    setBusy(false);
  }

  async function confirmWithPasskey() {
    setBusy(true);
    setError(undefined);
    try {
      const options = await mfaApi.passkeyBegin();
      onDone(await mfaApi.passkeyFinish(await getPasskey(options)));
    } catch (err) {
      fail(err);
    }
  }

  async function submitCode(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      onDone(await (method === "totp" ? mfaApi.totp(code) : mfaApi.recovery(code)));
    } catch (err) {
      setCode("");
      fail(err);
    }
  }

  function choose(m: SecondFactor) {
    setMethod(m);
    setCode("");
    setError(undefined);
  }

  return (
    <form className="auth-card" onSubmit={submitCode}>
      <div className="brand"><Icon name="logo" />Kwerft</div>
      <div>
        <h1>Confirm it's you</h1>
        <p className="lead">
          {method === "passkey" && "Use a passkey saved on this device, your phone or a security key."}
          {method === "totp" && "Enter the 6-digit code from your authenticator app."}
          {method === "recovery" && "Enter one of the recovery codes you saved when you set up two-factor sign-in. Each works once."}
        </p>
      </div>
      {method === "passkey" ? (
        <>
          {error && <p className="form-error" role="alert">{error}</p>}
          <button type="button" className="btn pri wide" onClick={confirmWithPasskey} disabled={busy} autoFocus>
            <Icon name="key" />{busy ? "Waiting for your passkey…" : "Use passkey"}
          </button>
        </>
      ) : (
        <>
          <Field key={method} id="login-code" label={method === "totp" ? "Authentication code" : "Recovery code"}
            className="mono" value={code} onChange={(e) => setCode(e.target.value)} error={error}
            autoComplete="one-time-code" inputMode={method === "totp" ? "numeric" : "text"} spellCheck={false}
            placeholder={method === "totp" ? "123 456" : "xxxx-xxxx-xxxx-xxxx"} autoFocus required />
          <button className="btn pri wide" disabled={busy || code.trim() === ""}>{busy ? "Checking…" : "Continue"}</button>
        </>
      )}
      <div className="alt-methods">
        {methods.filter((m) => m !== method).map((m) => (
          <button key={m} type="button" className="linkbtn" onClick={() => choose(m)}>{switchLabel[m]}</button>
        ))}
        <button type="button" className="linkbtn" onClick={() => onRestart()}>Start over</button>
      </div>
    </form>
  );
}
