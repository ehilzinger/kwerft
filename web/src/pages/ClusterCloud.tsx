// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { useState, type FormEvent } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { ApiError } from "../api";
import { Field } from "../components/Field";
import { clustersApi, clustersKey, type Cluster, type ClusterCloud } from "../clusterAdmin";
import { ago } from "../workloads";
import { FirewallState, LBState } from "./SettingsHCloud";
import "../styles/firewall.css";

const unreachable = "The console could not be reached. Check your connection and try again.";

// A Hetzner Cloud cluster's own Cloud Firewall and Load Balancer (Clusters ›
// a cluster › Overview). The console keeps the settings on the Cluster and
// hands them, with the project's Cloud API token from Settings, to Kwerft in
// that cluster, which reports back here (cluster_hcloud.go).
export function ClusterCloudCard({ c, cloud }: { c: Cluster; cloud: ClusterCloud }) {
  const queryClient = useQueryClient();
  const [firewall, setFirewall] = useState(cloud.firewall);
  const [lb, setLB] = useState(cloud.loadBalancer.enabled);
  const [lbType, setLBType] = useState(cloud.loadBalancer.type ?? "");
  const [lbLocation, setLBLocation] = useState(cloud.loadBalancer.location ?? "");
  const [error, setError] = useState<{ field?: string; message: string }>();
  const [saved, setSaved] = useState<string>();
  const [busy, setBusy] = useState(false);
  const fieldError = (f: string) => (error?.field === f ? error.message : undefined);
  const changed = firewall !== cloud.firewall || lb !== cloud.loadBalancer.enabled ||
    lbType !== (cloud.loadBalancer.type ?? "") || lbLocation !== (cloud.loadBalancer.location ?? "");
  const handOver = c.conditions?.find((x) => x.type === "HetznerCloud");
  const st = cloud.status;

  async function save(e: FormEvent) {
    e.preventDefault();
    setError(undefined);
    setSaved(undefined);
    setBusy(true);
    try {
      await clustersApi.saveCloud(c.name, { firewall, loadBalancer: { enabled: lb, type: lbType.trim() || undefined, location: lbLocation.trim() || undefined } });
      await queryClient.invalidateQueries({ queryKey: clustersKey });
      setSaved(c.connected ? "Saved. The cluster applies it within a minute." : "Saved. The cluster applies it once its agent is connected.");
    } catch (err) {
      setError(err instanceof ApiError ? { field: err.field, message: err.message } : { message: unreachable });
    } finally {
      setBusy(false);
    }
  }

  return (
    <section className="card">
      <h3>Hetzner Cloud</h3>
      <div className="bd stack">
        {handOver?.status === "False" && <div className="banner warn" role="status"><span>{handOver.message}</span></div>}
        {st?.message && <div className="banner warn" role="status"><span>{st.message}</span></div>}
        <dl className="kv">
          <dt>Servers</dt>
          <dd>
            {st?.servers?.length
              ? st.servers.map((srv) => `${srv.name} (${srv.location ?? "?"})`).join(", ")
              : <span className="dim">{c.connected ? "None reported yet." : "Reported once the cluster's agent is connected."}</span>}
          </dd>
          <dt>Cloud Firewall</dt><dd><FirewallState f={st?.firewall} /></dd>
          <dt>Load Balancer</dt><dd><LBState lb={st?.loadBalancer} enabled={cloud.loadBalancer.enabled} /></dd>
          {st?.syncedAt && <><dt>Last sync</dt><dd>{ago(st.syncedAt)}</dd></>}
        </dl>
        <form className="stack tight" onSubmit={save}>
          <div className="field full">
            <label className="check">
              <input type="checkbox" checked={firewall === "sync"} onChange={(e) => setFirewall(e.target.checked ? "sync" : "off")} />
              <span>
                <b>Mirror the server firewall to a Hetzner Cloud Firewall</b>
                <small>One Cloud Firewall for this cluster&apos;s servers with the rules its nodes confirmed; the required rules always stay open.</small>
              </span>
            </label>
          </div>
          <div className="field full">
            <label className="check">
              <input type="checkbox" checked={lb} onChange={(e) => setLB(e.target.checked)} />
              <span>
                <b>Put a Hetzner Load Balancer in front of the ingress</b>
                <small>
                  TCP 80 and 443 to the cluster&apos;s servers over its private network, keeping client addresses. It costs extra in the
                  Hetzner project; point the cluster&apos;s DNS at its address.
                </small>
              </span>
            </label>
          </div>
          {lb && (
            <div className="fields">
              <Field id="cluster-lb-type" label="Type" className="mono" value={lbType} placeholder="lb11"
                onChange={(e) => setLBType(e.target.value)} error={fieldError("loadBalancer.type")} />
              <Field id="cluster-lb-location" label="Location" className="mono" value={lbLocation} placeholder="as the first server"
                onChange={(e) => setLBLocation(e.target.value)} error={fieldError("loadBalancer.location")} />
            </div>
          )}
          {fieldError("loadBalancer.enabled") && <p className="form-error" role="alert">{fieldError("loadBalancer.enabled")}</p>}
          {!lb && cloud.loadBalancer.enabled && <p className="dim note">The Load Balancer is deleted ten minutes after it is turned off.</p>}
          {firewall === "off" && cloud.firewall === "sync" && <p className="dim note">Kwerft removes the cluster&apos;s Cloud Firewall; the host firewall keeps protecting the servers.</p>}
          {saved && <p className="ok-text" role="status">{saved}</p>}
          {error && !error.field && <p className="form-error" role="alert">{error.message}</p>}
          <div className="actions">
            <button className="btn pri" disabled={busy || !changed}>{busy ? "Saving…" : "Save"}</button>
          </div>
        </form>
      </div>
    </section>
  );
}
