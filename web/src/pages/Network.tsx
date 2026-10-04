import { useState } from "react";
import { Link } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { Icon } from "../components/Icon";
import { jobs, shortDate, type Domain } from "../jobs";
import { words } from "../workloads";
import { errorText } from "./Apps";
import "../styles/workloads.css";
import "../styles/jobs.css";

type Tab = "rules" | "firewall" | "domains";

// Network: traffic rules and the server firewall arrive in Phase 4; Domains
// & TLS lists the hostnames Apps claim, with their certificates.
export function Network() {
  const [tab, setTab] = useState<Tab>("domains");
  const domains = useQuery({ queryKey: ["domains"], queryFn: () => jobs.domains(), refetchInterval: 15000 });
  const tabs: { id: Tab; label: string; n?: number }[] = [
    { id: "rules", label: "Traffic rules" },
    { id: "firewall", label: "Server firewall" },
    { id: "domains", label: "Domains & TLS", n: domains.data ? domains.data.length + 1 : undefined },
  ];
  return (
    <section className="view">
      <div className="ph"><div><h1>Network</h1><p>Traffic rules between apps, the server firewall, and public domains</p></div></div>
      <div className="tabs" role="tablist" aria-label="Network">
        {tabs.map((t) => (
          <button key={t.id} role="tab" id={`tab-${t.id}`} aria-selected={tab === t.id} aria-controls={`panel-${t.id}`} onClick={() => setTab(t.id)}>
            {t.label}{t.n !== undefined && <span className="n">{t.n}</span>}
          </button>
        ))}
      </div>
      <div className="tabpanel" role="tabpanel" id={`panel-${tab}`} aria-labelledby={`tab-${tab}`}>
        {tab === "rules" && (
          <div className="empty">
            <h2>Traffic rules arrive in Phase 4</h2>
            <p>Rules between apps and projects as Cilium policies, with the connections each one allowed and dropped from Hubble. Until then, an app's “who may connect” setting decides, and projects are isolated from each other.</p>
          </div>
        )}
        {tab === "firewall" && (
          <div className="empty">
            <h2>The server firewall arrives in Phase 4</h2>
            <p>Edit the host firewall from here, mirrored to the Hetzner Cloud Firewall, with lock-out protection. Until then the installer's baseline applies: SSH, HTTP and HTTPS open; cluster traffic only on the private network.</p>
          </div>
        )}
        {tab === "domains" && <Domains q={domains} />}
      </div>
    </section>
  );
}

function Domains({ q }: { q: { data?: Domain[]; isPending: boolean; isError: boolean; error: unknown } }) {
  const console_ = window.location.hostname;
  if (q.isPending) return <p className="loading">Loading domains…</p>;
  return (
    <>
      {q.isError && <div className="banner bad" role="alert"><Icon name="alert" /><span>{errorText(q.error)}</span></div>}
      <div className="card scroll-x">
        <table className="t">
          <thead><tr><th>Hostname</th><th>Routes to</th><th>Certificate</th><th>Expires</th><th>Listener</th></tr></thead>
          <tbody>
            <tr>
              <td className="nm">{console_}</td>
              <td>Kwerft console</td>
              <td><span className="pill mute nodot" title="Set up by the installer with the Gateway's own listener">Installer</span></td>
              <td className="dim">—</td>
              <td className="mono dim">console</td>
            </tr>
            {(q.data ?? []).map((d) => (
              <tr key={`${d.project}/${d.name}`}>
                <td className="nm"><a href={`https://${d.hostname}`} target="_blank" rel="noreferrer">{d.hostname}</a></td>
                <td>
                  {d.app ? <Link to="/apps/$project/$name" params={{ project: d.project, name: d.app }}>{d.project}/{d.app}</Link> : <span className="dim">{d.project} · no app</span>}
                </td>
                <td><CertStatus d={d} /></td>
                <td className={expiresSoon(d.notAfter) ? "warn-text" : undefined} title={d.notAfter ? new Date(d.notAfter).toLocaleString() : undefined}>{d.notAfter ? shortDate(d.notAfter) : <span className="dim">—</span>}</td>
                <td className="mono dim">{d.listener ?? "—"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {(q.data?.length ?? 0) === 0 && !q.isError && (
        <p className="dim note">No app has a public domain yet. Give an app's port a public hostname in its settings; Kwerft adds an HTTPS listener and gets a Let's Encrypt certificate.</p>
      )}
      <p className="dim note">Certificates renew automatically about a month before they expire. Wildcard domains, redirects and per-domain options arrive with DNS-01 in a later phase.</p>
    </>
  );
}

function expiresSoon(iso?: string) {
  return !!iso && new Date(iso).getTime() - Date.now() < 14 * 86400000;
}

function CertStatus({ d }: { d: Domain }) {
  switch (d.certificate) {
    case "valid": return <span className="pill ok" title={d.message}>Valid</span>;
    case "issuing": return <span className="pill info" title={d.message}>Issuing</span>;
    case "disabled": return <span className="pill mute" title={d.message}>No issuer</span>;
    case "failed": return <span className="pill bad" title={d.message}>{words(d.reason) || "Failed"}</span>;
    default: return <span className="pill mute" title={d.message}>Pending</span>;
  }
}
