// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useState, type FormEvent } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../api";
import { Dialog } from "../components/Dialog";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { HOST_RE } from "../workloads";
import { shortDate } from "../jobs";
import { LOCAL, useClusters } from "../clusters";
import { ClusterPicker } from "../components/ClusterUI";
import { DNSRecordsTable } from "../components/DNSRecords";
import { isTemporaryHost, settingsApi, type CertificateState, type DNSCheck, type PasskeyHolder, type Settings as SettingsData } from "../settings";
import "../styles/workloads.css";
import "../styles/settings.css";
import { GitConnectionsCard } from "./GitConnections";
import { DataKeyCard, SSOCard } from "./SettingsIdentity";
import { HCloudCard } from "./SettingsHCloud";
import { BackupsCard } from "./SettingsBackups";
import { SettingsTabs } from "./SettingsUpdates";

const unreachable = "The console could not be reached. Check your connection and try again.";
const errText = (e: unknown) => (e instanceof ApiError ? e.message : unreachable);

// Settings: where the console lives, the base domain for apps, how their
// certificates are issued, Git connections (GitConnections.tsx), single
// sign-on and the data key (SettingsIdentity.tsx). Owners and admins change
// them; everyone else sees them read-only. Single sign-on is for owners and
// admins only, the data key for owners.
export function Settings() {
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const canEdit = session.data?.role === "owner" || session.data?.role === "admin";
  const settings = useQuery({
    queryKey: ["settings"],
    queryFn: () => settingsApi.get(),
    // Poll quickly while something is being issued: the move waits for it.
    refetchInterval: (q) => (q.state.data?.pendingConsoleDomain || (q.state.data?.ready && !q.state.data.ready.status) ? 3000 : 20000),
  });
  // The apps domain, its DNS records and certificates are per cluster
  // (docs/phase5.md); everything else is the console's.
  const { multi } = useClusters();
  const [cluster, setCluster] = useState(LOCAL);
  const remote = useQuery({
    queryKey: ["settings", cluster],
    queryFn: () => settingsApi.get(cluster),
    enabled: cluster !== LOCAL,
    refetchInterval: 20000,
  });
  const apps = cluster === LOCAL ? settings.data : remote.data;

  return (
    <section className="view settings">
      <div className="ph">
        <div>
          <h1>Settings</h1>
          <p>Where the console and your apps live, how their certificates are issued, access to Git hosts, sign-in and backups</p>
        </div>
      </div>
      <SettingsTabs current="general" />
      {!canEdit && session.data && (
        <div className="banner info"><Icon name="shield" /><span>Only owners and admins change settings. You can see them here.</span></div>
      )}
      {settings.isPending && <p className="loading">Loading settings…</p>}
      {settings.isError && <div className="banner bad" role="alert"><Icon name="alert" /><span>{errText(settings.error)}</span></div>}
      {settings.data && (
        <>
          <ConsoleCard s={settings.data} canEdit={canEdit} />
          {multi && (
            <div className="toolbar">
              <ClusterPicker value={cluster} onChange={setCluster} />
              <span className="dim">Apps domain, DNS records and certificates of this cluster</span>
            </div>
          )}
          {remote.isError && cluster !== LOCAL && <div className="banner bad" role="alert"><Icon name="alert" /><span>{errText(remote.error)}</span></div>}
          {!apps ? (
            cluster !== LOCAL && remote.isPending && <p className="loading">Loading the settings of cluster {cluster}…</p>
          ) : (
            <>
              <AppsCard key={apps.cluster} s={apps} canEdit={canEdit} />
              {apps.cluster === LOCAL || !apps.cluster
                ? apps.manageRecords && <DNSRecordsCard s={apps} />
                : apps.consoleAppsDomain && <ClusterRecordsCard s={apps} />}
              <CertificatesCard s={apps} />
            </>
          )}
          <GitConnectionsCard canEdit={canEdit} />
          {canEdit && <SSOCard />}
          {session.data?.role === "owner" && <DataKeyCard />}
          <HCloudCard s={settings.data} canEdit={canEdit} />
          {canEdit && <BackupsCard />}
        </>
      )}
    </section>
  );
}

// ---- console hostname ------------------------------------------------------------

function ConsoleCard({ s, canEdit }: { s: SettingsData; canEdit: boolean }) {
  const here = window.location.hostname;
  const [host, setHost] = useState("");
  const [check, setCheck] = useState<DNSCheck>();
  const [checking, setChecking] = useState(false);
  const [error, setError] = useState<string>();
  const [confirming, setConfirming] = useState(false);
  const cert = s.certificates.find((c) => c.purpose === "console");
  const next = s.certificates.find((c) => c.purpose === "console-next");
  const target = host.trim().toLowerCase().replace(/\.$/, "");

  // Moved while this page was open on the old name: go to the new one.
  const moved = s.previousConsoleDomain === here && s.consoleDomain !== here;
  useEffect(() => {
    if (!moved) return;
    const t = window.setTimeout(() => window.location.assign(`https://${s.consoleDomain}/login`), 5000);
    return () => window.clearTimeout(t);
  }, [moved, s.consoleDomain]);

  const queryClient = useQueryClient();
  const [undoing, setUndoing] = useState(false);
  const [undoError, setUndoError] = useState<string>();
  async function undoMove() {
    setUndoing(true);
    setUndoError(undefined);
    try {
      queryClient.setQueryData(["settings"], await settingsApi.moveConsole(s.consoleDomain, s.consoleDomain));
    } catch (err) {
      setUndoError(errText(err));
    } finally {
      setUndoing(false);
    }
  }

  async function runCheck(e?: FormEvent) {
    e?.preventDefault();
    setError(undefined);
    setCheck(undefined);
    if (!HOST_RE.test(target)) {
      setError("Enter a hostname, like ops.example.com.");
      return;
    }
    setChecking(true);
    try {
      setCheck(await settingsApi.dnsCheck(target));
    } catch (err) {
      setError(errText(err));
    } finally {
      setChecking(false);
    }
  }

  return (
    <section className="card">
      <h2>Console domain</h2>
      <div className="bd stack">
        {moved && (
          <div className="banner info" role="status"><Icon name="rocket" />
            <span>The console has moved to <b>{s.consoleDomain}</b>. Sign in again there; taking you over in a few seconds.</span>
            <a className="btn pri" href={`https://${s.consoleDomain}/login`}>Go to {s.consoleDomain}</a>
          </div>
        )}
        <div className="current">
          <div>
            <span className="k">Console</span>
            <a className="host" href={`https://${s.consoleDomain}`}>https://{s.consoleDomain}</a>
          </div>
          {cert && <CertPill c={cert} />}
          {isTemporaryHost(s.consoleDomain) && <span className="pill warn" title="sslip.io resolves <ip>.sslip.io to <ip>: fine for trying Kwerft, not for production">Temporary name</span>}
        </div>
        {s.pendingConsoleDomain && (
          <div className="banner info" role="status"><Icon name="clock" />
            <span>
              Moving to <b>{s.pendingConsoleDomain}</b>: {next?.state === "failed" ? <>the certificate failed: {next.message}</> : <>waiting for its certificate{next?.message ? ` (${next.message})` : ""}.</>}{" "}
              The console stays at {s.consoleDomain} until then.
            </span>
            {canEdit && <button className="btn sm" disabled={undoing} onClick={undoMove}>{undoing ? "Cancelling…" : "Cancel the move"}</button>}
          </div>
        )}
        {undoError && <p className="form-error" role="alert">{undoError}</p>}
        {canEdit && !moved && (
          <form className="move" onSubmit={runCheck}>
            <Field id="console-host" label="New console hostname" className="mono" value={host} placeholder="ops.example.com"
              onChange={(e) => { setHost(e.target.value); setCheck(undefined); setError(undefined); }}
              autoComplete="off" spellCheck={false} error={error}
              hint={s.manageRecords
                ? "Kwerft creates its DNS record when the name is in one of your Hetzner zones; otherwise point an A record at this server."
                : s.publicAddresses.length ? `Create an A record for it pointing to ${s.publicAddresses.join(", ")} (this server).` : "Create an A record for it pointing to this server."} />
            <div className="move-acts">
              <button className="btn" disabled={checking || !target}>{checking ? "Checking…" : "Check DNS"}</button>
              <button type="button" className="btn pri" onClick={() => setConfirming(true)}
                disabled={!check?.ok || check.hostname !== target || target === s.consoleDomain || target === s.pendingConsoleDomain}>Move the console…</button>
            </div>
            {check && <p className={check.ok ? "ok-text" : "form-error"} role="status">{check.ok ? "✓ " : ""}{check.message}</p>}
          </form>
        )}
        <p className="dim note">
          The console moves once the new name has its certificate; the old name then redirects to it for a day. Passkeys stay bound to the old name, and everyone signs in again on the new one.
        </p>
      </div>
      {confirming && <MoveDialog from={s.consoleDomain} to={target} onClose={() => setConfirming(false)}
        onMoved={() => { setConfirming(false); setHost(""); setCheck(undefined); }} />}
    </section>
  );
}

function MoveDialog({ from, to, onClose, onMoved }: { from: string; to: string; onClose: () => void; onMoved: () => void }) {
  const queryClient = useQueryClient();
  const holders = useQuery({ queryKey: ["settings", "passkeys"], queryFn: settingsApi.passkeyHolders });
  const [confirm, setConfirm] = useState("");
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  const stranded = (holders.data ?? []).filter((h) => h.stranded);

  async function submit() {
    setBusy(true);
    setError(undefined);
    try {
      queryClient.setQueryData(["settings"], await settingsApi.moveConsole(to, confirm));
      onMoved();
    } catch (err) {
      setError(errText(err));
      setBusy(false);
    }
  }

  return (
    <Dialog title={`Move the console to ${to}?`} onClose={onClose} onSubmit={submit}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={busy || confirm.trim().toLowerCase() !== to || stranded.length > 0}>{busy ? "Moving…" : "Move the console"}</button>
      </>}>
      <ul className="consequences">
        <li><b>Everyone signs in again</b> on {to}. Sessions belong to {from} and do not move.</li>
        <li><b>Passkeys stop working.</b> A passkey only works on the hostname it was created on. Members sign in with their password and authenticator app or a recovery code, then add a passkey again on {to}.</li>
        <li><b>{from} redirects</b> to {to} for a day, then stops answering.</li>
      </ul>
      {holders.isPending && <p className="dim note">Checking who uses passkeys…</p>}
      {holders.data && holders.data.length > 0 && <Holders list={holders.data} />}
      {stranded.length > 0 && (
        <p className="form-error" role="alert">
          {stranded.map((h) => h.email).join(", ")} could not sign in after the move: no authenticator app and no recovery codes left. Ask them to add one on their account page first.
        </p>
      )}
      <Field id="move-confirm" label={`Type ${to} to confirm`} className="mono" value={confirm} onChange={(e) => setConfirm(e.target.value)}
        autoComplete="off" spellCheck={false} autoFocus error={error} />
    </Dialog>
  );
}

function Holders({ list }: { list: PasskeyHolder[] }) {
  return (
    <div className="holders">
      <p className="note">Members with passkeys, and how they sign in after the move:</p>
      <ul className="rows">
        {list.map((h) => (
          <li key={h.email} className="row">
            <Icon name="key" />
            <div className="grow"><b>{h.name}</b><small>{h.email}</small></div>
            {h.totp ? <span className="pill ok">Authenticator app</span>
              : h.recoveryCodes > 0 ? <span className="pill warn">{h.recoveryCodes} recovery {h.recoveryCodes === 1 ? "code" : "codes"}</span>
              : <span className="pill bad">No other way in</span>}
          </li>
        ))}
      </ul>
    </div>
  );
}

// ---- apps domain and certificate method ------------------------------------------------

function AppsCard({ s, canEdit }: { s: SettingsData; canEdit: boolean }) {
  const queryClient = useQueryClient();
  // A remote cluster under the console's apps domain: a certificate per
  // hostname, records kept by the console.
  const remote = !!s.cluster && s.cluster !== LOCAL;
  const [apps, setApps] = useState(s.appsDomain ?? "");
  const [tls, setTLS] = useState<"http01" | "dns01">(s.tls);
  const [records, setRecords] = useState(s.manageRecords);
  const [token, setToken] = useState("");
  const [error, setError] = useState<{ field?: string; message: string }>();
  const [done, setDone] = useState<string>();
  const [warning, setWarning] = useState<string>();
  const [busy, setBusy] = useState(false);
  const [check, setCheck] = useState<DNSCheck>();
  const domain = apps.trim().toLowerCase().replace(/\.$/, "");
  const fieldError = (f: string) => (error?.field === f ? error.message : undefined);
  const changed = domain !== (s.appsDomain ?? "") || tls !== s.tls || records !== s.manageRecords || token.trim() !== "";
  const needsToken = tls === "dns01" || records;
  const ip = s.publicAddresses.join(", ") || (remote ? "the cluster" : "this server");
  const sharesConsoleDomain = remote && !!s.consoleAppsDomain && (domain === "" || domain === s.consoleAppsDomain);

  async function save(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    setDone(undefined);
    setWarning(undefined);
    if (domain && !HOST_RE.test(domain)) {
      setError({ field: "appsDomain", message: "Enter a domain, like apps.example.com." });
      return;
    }
    setBusy(true);
    try {
      const res = await settingsApi.saveApps({ appsDomain: domain, tls, manageRecords: records, ...(token.trim() ? { token: token.trim() } : {}) }, s.cluster);
      queryClient.setQueryData(remote ? ["settings", s.cluster] : ["settings"], res.settings);
      setToken("");
      setWarning(res.warning);
      setDone(res.zone ? `Saved. The token can manage the zone ${res.zone}.` : "Saved.");
    } catch (err) {
      setError(err instanceof ApiError ? { field: err.field, message: err.message } : { message: unreachable });
    } finally {
      setBusy(false);
    }
  }

  async function checkWildcard() {
    setCheck(undefined);
    try {
      setCheck(await settingsApi.dnsCheck(domain, true));
    } catch (err) {
      setError({ field: "appsDomain", message: errText(err) });
    }
  }

  return (
    <section className="card">
      <h2>Apps domain &amp; certificates</h2>
      <form className="bd stack" onSubmit={save}>
        {remote ? (
          <p className="dim note">
            Leave it empty to use the console&apos;s apps domain{s.consoleAppsDomain && <> <code>{s.consoleAppsDomain}</code></>}: {s.consoleRecords
              ? <>the console keeps a DNS record for each of this cluster&apos;s hostnames under it, pointing to {ip}</>
              : <>create a DNS record for each of this cluster&apos;s hostnames under it, pointing to {ip}</>}, and the cluster gets a certificate for each (HTTP-01).
            A domain of its own works as it does for the console&apos;s cluster.
          </p>
        ) : (
          <p className="dim note">
            Apps get subdomains under a base domain, so <code>invoices</code> becomes <code>invoices.{domain || "apps.example.com"}</code> without touching DNS again. The deploy wizard suggests these names.
          </p>
        )}
        <div className="fields">
          <div className="stack tight">
            <Field id="apps-domain" label="Base domain for apps" className="mono" value={apps} placeholder={remote && s.consoleAppsDomain ? s.consoleAppsDomain : "apps.example.com"}
              onChange={(e) => { setApps(e.target.value); setCheck(undefined); }} disabled={!canEdit} autoComplete="off" spellCheck={false}
              error={fieldError("appsDomain")}
              hint={sharesConsoleDomain ? `One record per hostname → ${ip}` : records ? `Kwerft keeps *.${domain || "apps.example.com"} → ${ip}` : `Create *.${domain || "apps.example.com"} → ${ip}`} />
            {canEdit && !remote && domain && HOST_RE.test(domain) && (
              <div className="inline-check">
                <button type="button" className="btn sm" onClick={checkWildcard}>Check wildcard DNS</button>
                {check && <span className={check.ok ? "ok-text" : "form-error"} role="status">{check.ok ? `✓ ${check.managed ? check.message : `*.${domain} points to this server.`}` : check.message}</span>}
              </div>
            )}
          </div>
          <div className="field full">
            <label id="tls-label">How should certificates be issued?</label>
            <div className="radio" role="group" aria-labelledby="tls-label">
              <button type="button" className="opt" aria-pressed={tls === "http01"} disabled={!canEdit} onClick={() => setTLS("http01")}>
                <span className="r" /><b>HTTP-01, one certificate per hostname</b>
                <span>No DNS credentials needed. Every app hostname needs a DNS record (a wildcard record covers them all) and a listener of its own; the Gateway has room for 59.</span>
              </button>
              <button type="button" className="opt" aria-pressed={tls === "dns01"} disabled={!canEdit || sharesConsoleDomain} onClick={() => setTLS("dns01")}>
                <span className="r" /><b>DNS-01 via Hetzner DNS, wildcard certificate</b>
                <span>One <code>*.{domain || "apps.example.com"}</code> certificate and listener for every app under the domain, without limit. Needs a Hetzner API token for the DNS zone.</span>
              </button>
            </div>
          </div>
          {!sharesConsoleDomain && <div className="field full">
            <label className="check records">
              <input type="checkbox" checked={records} disabled={!canEdit} onChange={(e) => setRecords(e.target.checked)} />
              <span>
                <b>Let Kwerft create the DNS records</b>
                <small>
                  A and AAAA records for <code>{s.pendingConsoleDomain || s.consoleDomain}</code>{domain && <> and <code>*.{domain}</code></>} pointing to {ip}, kept up to date in Hetzner DNS when the server&apos;s address changes.
                  Records you created yourself are never changed.
                </small>
              </span>
            </label>
          </div>}
          {needsToken && (
            <div className="full">
              <Field id="dns-token" label="Hetzner API token" className="mono" type="password" value={token} disabled={!canEdit}
                onChange={(e) => setToken(e.target.value)} autoComplete="off" spellCheck={false}
                placeholder={s.tokenSet ? "•••••••••••••••• stored — enter a new one to replace it" : "Read & Write token of the project with the DNS zone"}
                error={fieldError("token")}
                hint="From the Hetzner Console: the project with your DNS zones → Security → API tokens, Read & Write. Kwerft checks it can see the zone, stores it, and never shows it again." />
            </div>
          )}
        </div>
        {tls === "dns01" && s.tls !== "dns01" && (
          <p className="dim note">Apps under the domain move to the wildcard once its certificate is issued, usually within two minutes; until then they keep their own certificates.</p>
        )}
        {records && !s.manageRecords && (
          <p className="dim note">Kwerft creates the records within a minute of saving. Names that already have records it did not create are listed as conflicts, not overwritten.</p>
        )}
        {!records && s.manageRecords && (
          <p className="dim note">The records stay as they are; Kwerft just stops updating them.</p>
        )}
        {tls === "http01" && s.tls === "dns01" && (
          <p className="dim note">Apps under the domain go back to a certificate each. Their HTTPS answers with a default certificate until those are issued.</p>
        )}
        {error && !error.field && <p className="form-error" role="alert">{error.message}</p>}
        {warning && <p className="warn-text note" role="status">{warning}</p>}
        {done && <p className="ok-text" role="status">{done}</p>}
        {canEdit && (
          <div className="actions">
            <button className="btn pri" disabled={busy || !changed}>{busy ? "Saving…" : "Save"}</button>
          </div>
        )}
      </form>
    </section>
  );
}

// ---- DNS records -----------------------------------------------------------------------

function DNSRecordsCard({ s }: { s: SettingsData }) {
  return (
    <section className="card">
      <h2>DNS records</h2>
      {s.dnsMessage && (
        <div className="bd"><div className="banner warn" role="status"><Icon name="alert" /><span>{s.dnsMessage}</span></div></div>
      )}
      {s.dnsRecords.length === 0 ? (
        <div className="bd"><p className="dim note">{s.dnsMessage ? "No records yet." : "Kwerft is creating the records…"}</p></div>
      ) : (
        <DNSRecordsTable records={s.dnsRecords} />
      )}
      <div className="bd">
        <p className="dim note">
          Kwerft labels the records it creates (<code>kwerft.dev/managed-by</code>) and only changes those{s.dnsSyncedAt ? <>; last checked {shortDate(s.dnsSyncedAt)} {new Date(s.dnsSyncedAt).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}</> : null}.
        </p>
      </div>
    </section>
  );
}

/** A remote cluster: the records the console keeps for its hostnames under the console's apps domain. */
function ClusterRecordsCard({ s }: { s: SettingsData }) {
  const d = s.clusterDNS;
  return (
    <section className="card">
      <h2>DNS records</h2>
      {d?.message && (
        <div className="bd"><div className="banner warn" role="status"><Icon name="alert" /><span>{d.message}</span></div></div>
      )}
      {!d || d.records.length === 0 ? (
        <div className="bd"><p className="dim note">No app of this cluster has a hostname under <code>{s.consoleAppsDomain}</code> yet.</p></div>
      ) : (
        <DNSRecordsTable records={d.records} />
      )}
      <div className="bd">
        <p className="dim note">
          The console keeps one record per hostname, more specific than <code>*.{s.consoleAppsDomain}</code>, which points at the console&apos;s cluster. A new hostname gets its record within a minute or two of its deploy.
        </p>
      </div>
    </section>
  );
}

// ---- certificates ----------------------------------------------------------------------

const purposes: Record<CertificateState["purpose"], string> = {
  console: "Console",
  "console-next": "New console hostname",
  "console-previous": "Previous console hostname (redirects)",
  "apps-wildcard": "Apps wildcard · DNS-01",
};

function CertificatesCard({ s }: { s: SettingsData }) {
  return (
    <section className="card">
      <h2>Certificates</h2>
      {s.ready && !s.ready.status && s.ready.reason !== "ConsoleMoving" && (
        <div className="bd"><div className="banner warn" role="status"><Icon name="alert" /><span>{s.ready.message}</span></div></div>
      )}
      {s.certificates.length === 0 ? (
        <div className="bd"><p className="dim note">No certificates yet. Each app hostname's certificate is listed under Network → Domains &amp; TLS.</p></div>
      ) : (
        <div className="scroll-x">
          <table className="t">
            <thead><tr><th>Hostname</th><th>For</th><th>State</th><th>Expires</th></tr></thead>
            <tbody>
              {s.certificates.map((c) => (
                <tr key={c.name}>
                  <td className="nm mono">{c.hostnames.join(", ")}</td>
                  <td>{purposes[c.purpose] ?? c.purpose}</td>
                  <td><CertPill c={c} /></td>
                  <td className="dim" title={c.notAfter ? new Date(c.notAfter).toLocaleString() : undefined}>{c.notAfter ? shortDate(c.notAfter) : "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

function CertPill({ c }: { c: CertificateState }) {
  switch (c.state) {
    case "valid": return <span className="pill ok" title={c.message}>Valid</span>;
    case "issuing": return <span className="pill info" title={c.message}>Issuing</span>;
    default: return <span className="pill bad" title={c.message}>Failed</span>;
  }
}
