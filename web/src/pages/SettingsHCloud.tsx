import { useState, type FormEvent } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { ApiError } from "../api";
import { Field } from "../components/Field";
import { ago } from "../workloads";
import "../styles/firewall.css";
import { settingsApi, type CloudFirewallStatus, type HCloudCheck, type LoadBalancerStatus, type Settings } from "../settings";

const unreachable = "The console could not be reached. Check your connection and try again.";

// Settings › Hetzner Cloud API: the Cloud API token (write-only), the Cloud
// Firewall the server firewall is mirrored to, the optional Load Balancer in
// front of the ingress, and what the installer set up (Cloud Volumes, the
// cloud-controller-manager). Owners and admins change it; everyone sees it.
export function HCloudCard({ s, canEdit }: { s: Settings; canEdit: boolean }) {
  const h = s.hcloud;
  const queryClient = useQueryClient();
  const [token, setToken] = useState("");
  const [error, setError] = useState<{ field?: string; message: string }>();
  const [busy, setBusy] = useState(false);
  const [check, setCheck] = useState<HCloudCheck>();
  const [warning, setWarning] = useState<string>();
  const [firewall, setFirewall] = useState(h.firewall);
  const [lb, setLB] = useState(h.loadBalancer.enabled);
  const [lbType, setLBType] = useState(h.loadBalancer.type ?? "");
  const [lbLocation, setLBLocation] = useState(h.loadBalancer.location ?? "");
  const [saved, setSaved] = useState<string>();
  const fieldError = (f: string) => (error?.field === f ? error.message : undefined);
  const changed = firewall !== h.firewall || lb !== h.loadBalancer.enabled || lbType !== (h.loadBalancer.type ?? "") || lbLocation !== (h.loadBalancer.location ?? "");
  const fail = (err: unknown) => setError(err instanceof ApiError ? { field: err.field, message: err.message } : { message: unreachable });

  async function saveToken(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    setCheck(undefined);
    setWarning(undefined);
    setBusy(true);
    try {
      const res = await settingsApi.saveHCloudToken(token.trim());
      queryClient.setQueryData(["settings"], res.settings);
      setToken("");
      setCheck(res.check);
      setWarning(res.warning);
    } catch (err) {
      fail(err);
    } finally {
      setBusy(false);
    }
  }

  async function removeToken() {
    setError(undefined);
    setCheck(undefined);
    setBusy(true);
    try {
      const res = await settingsApi.removeHCloudToken();
      queryClient.setQueryData(["settings"], res.settings);
    } catch (err) {
      fail(err);
    } finally {
      setBusy(false);
    }
  }

  async function saveOptions(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    setSaved(undefined);
    setBusy(true);
    try {
      const res = await settingsApi.saveHCloud({ firewall, loadBalancer: { enabled: lb, type: lbType.trim() || undefined, location: lbLocation.trim() || undefined } });
      queryClient.setQueryData(["settings"], res);
      setSaved("Saved. Kwerft applies it within a minute.");
    } catch (err) {
      fail(err);
    } finally {
      setBusy(false);
    }
  }

  const st = h.status;
  return (
    <section className="card">
      <h2>Hetzner Cloud API</h2>
      <div className="bd stack">
        <p className="dim note">
          With a Cloud API token Kwerft mirrors the server firewall to a Hetzner Cloud Firewall in front of the cluster&apos;s Cloud servers,
          can put a Load Balancer in front of the ingress, and lets the installer set up Cloud Volumes. Dedicated servers are not affected.
        </p>
        <form className="stack tight" onSubmit={saveToken}>
          <Field id="hcloud-token" label="Cloud API token" className="mono" type="password" value={token} disabled={!canEdit}
            onChange={(e) => setToken(e.target.value)} autoComplete="off" spellCheck={false}
            placeholder={h.tokenSet ? "•••••••••••••••• stored — enter a new one to replace it" : "Read & Write token of the project the servers run in"}
            error={fieldError("token")}
            hint="Hetzner Console → the project of this cluster's servers → Security → API tokens, Read & Write. Kwerft checks it, stores it, and never shows it again." />
          {canEdit && (
            <div className="actions">
              {h.tokenSet && <button type="button" className="btn" disabled={busy} onClick={removeToken}>Remove token</button>}
              <button className="btn pri" disabled={busy || token.trim() === ""}>{busy ? "Checking…" : h.tokenSet ? "Replace token" : "Save token"}</button>
            </div>
          )}
          {check && <TokenCheck check={check} />}
          {warning && <p className="warn-text note" role="status">{warning}</p>}
        </form>

        {h.tokenSet && st?.message && <div className="banner warn" role="status"><span>{st.message}</span></div>}
        {h.tokenSet && (
          <dl className="kv">
            <dt>Servers</dt>
            <dd>
              {st?.servers?.length
                ? st.servers.map((srv) => `${srv.name} (${srv.location ?? "?"})${srv.node !== srv.name ? ` as node ${srv.node}` : ""}`).join(", ")
                : <span className="dim">None of the nodes is a Cloud server of the token&apos;s project yet.</span>}
            </dd>
            <dt>Cloud Firewall</dt><dd><FirewallState f={st?.firewall} /></dd>
            <dt>Load Balancer</dt><dd><LBState lb={st?.loadBalancer} enabled={h.loadBalancer.enabled} /></dd>
            {st?.syncedAt && <><dt>Last sync</dt><dd>{ago(st.syncedAt)}</dd></>}
          </dl>
        )}
        <dl className="kv">
          <dt>Cloud Volumes</dt>
          <dd>{h.volumes ? "Available (storage class hcloud-volumes)" : <span className="dim">Not set up. With a token stored, re-run the installer on the Cloud server to add the CSI driver.</span>}</dd>
          <dt>Cloud controller</dt>
          <dd>{h.ccm ? "hcloud cloud-controller-manager runs" : <span className="dim">Not used. It can only be chosen when a cluster is first installed (see the installer&apos;s hcloud.cloudControllerManager).</span>}</dd>
        </dl>

        {h.tokenSet && (
          <form className="stack tight" onSubmit={saveOptions}>
            <div className="field full">
              <label className="check">
                <input type="checkbox" checked={firewall === "sync"} disabled={!canEdit} onChange={(e) => setFirewall(e.target.checked ? "sync" : "off")} />
                <span>
                  <b>Mirror the server firewall to a Hetzner Cloud Firewall</b>
                  <small>
                    One Cloud Firewall for the cluster&apos;s Cloud servers with the rules of Network › Server firewall once the nodes confirmed them.
                    SSH, HTTP, HTTPS and WireGuard always stay open as the required rules say; the host firewall stays as it is.
                  </small>
                </span>
              </label>
            </div>
            <div className="field full">
              <label className="check">
                <input type="checkbox" checked={lb} disabled={!canEdit || (!h.loadBalancerReady && !h.loadBalancer.enabled)} onChange={(e) => setLB(e.target.checked)} />
                <span>
                  <b>Put a Hetzner Load Balancer in front of the ingress</b>
                  <small>
                    {h.loadBalancerReady
                      ? "TCP 80 and 443 to the Cloud servers over the private network, keeping client addresses (PROXY protocol). DNS moves to it once a server passes its health check; it costs extra in the Hetzner project."
                      : "Needs a Cloud server with a private network (Hetzner Cloud Network) and the installer run on it, which makes the ingress accept the Load Balancer."}
                  </small>
                </span>
              </label>
            </div>
            {lb && (
              <div className="fields">
                <Field id="lb-type" label="Type" className="mono" value={lbType} placeholder="lb11" disabled={!canEdit}
                  onChange={(e) => setLBType(e.target.value)} error={fieldError("loadBalancer.type")} />
                <Field id="lb-location" label="Location" className="mono" value={lbLocation} placeholder="as the first server" disabled={!canEdit}
                  onChange={(e) => setLBLocation(e.target.value)} error={fieldError("loadBalancer.location")} />
              </div>
            )}
            {fieldError("loadBalancer.enabled") && <p className="form-error" role="alert">{fieldError("loadBalancer.enabled")}</p>}
            {!lb && h.loadBalancer.enabled && <p className="dim note">DNS moves back to the servers at once; the Load Balancer is deleted ten minutes later, when resolvers no longer hand out its address.</p>}
            {firewall === "off" && h.firewall === "sync" && <p className="dim note">Kwerft removes its Cloud Firewall from the servers and deletes it. The host firewall keeps protecting them.</p>}
            {saved && <p className="ok-text" role="status">{saved}</p>}
            {canEdit && (
              <div className="actions">
                <button className="btn pri" disabled={busy || !changed}>{busy ? "Saving…" : "Save"}</button>
              </div>
            )}
          </form>
        )}
        {error && !error.field && <p className="form-error" role="alert">{error.message}</p>}
      </div>
    </section>
  );
}

function TokenCheck({ check }: { check: HCloudCheck }) {
  const where = check.locations.length ? ` in ${check.locations.join(", ")}` : "";
  return (
    <p className="ok-text note" role="status">
      ✓ Saved. The token&apos;s project has {check.servers} {check.servers === 1 ? "server" : "servers"}{where}
      {check.nodes.length > 0 && <>; this cluster&apos;s {check.nodes.map((n) => `${n.server} (${n.location})`).join(", ")}</>}.
    </p>
  );
}

export function FirewallState({ f }: { f?: CloudFirewallStatus }) {
  if (!f) return <span className="dim">Not synced yet.</span>;
  const pill = { InSync: "ok", Applying: "info", Off: "mute", Error: "bad" }[f.state] ?? "info";
  const label = { InSync: "In sync", Applying: "Waiting for confirmation", Off: "Off", Error: "Error" }[f.state] ?? f.state;
  return (
    <span>
      <span className={`pill ${pill}`}>{label}</span>{" "}
      {f.name && <code>{f.name}</code>}
      {f.state !== "Off" && f.rules !== undefined && <span className="dim"> · {f.rules} rules · {f.servers ?? 0} {f.servers === 1 ? "server" : "servers"}</span>}
      {f.message && <span className="cloud-msg">{f.message}</span>}
    </span>
  );
}

function LBState({ lb, enabled }: { lb?: LoadBalancerStatus; enabled: boolean }) {
  if (!lb) return <span className="dim">{enabled ? "Being created…" : "Off"}</span>;
  const pill = { Active: "ok", Waiting: "info", Creating: "info", Draining: "warn", Error: "bad" }[lb.state] ?? "info";
  return (
    <span>
      <span className={`pill ${pill}`}>{lb.state}</span>{" "}
      {lb.ipv4 && <code>{[lb.ipv4, lb.ipv6].filter(Boolean).join(", ")}</code>}
      {lb.targets !== undefined && lb.state !== "Draining" && <span className="dim"> · {lb.healthyTargets ?? 0}/{lb.targets} healthy</span>}
      {lb.active && <span className="dim"> · DNS points here</span>}
      {lb.message && <span className="cloud-msg">{lb.message}</span>}
    </span>
  );
}
