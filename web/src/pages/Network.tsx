import { useState } from "react";
import { Link, useSearch } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { useClusters } from "../clusters";
import { ClusterBadge } from "../components/ClusterUI";
import { RecordPill } from "../components/DNSRecords";
import { Icon } from "../components/Icon";
import { jobs, shortDate, type Domain } from "../jobs";
import { words } from "../workloads";
import { errorText } from "./Apps";
import { TrafficRules } from "./NetworkTraffic";
import { settingsApi, type CertificateState } from "../settings";
import { ServerFirewall } from "./NetworkFirewall";
import "../styles/workloads.css";
import "../styles/jobs.css";

type Tab = "rules" | "firewall" | "domains";

// Network: traffic rules and the server firewall arrive in Phase 4; Domains
// & TLS lists the hostnames Apps claim, with their certificates.
export function Network() {
  const search = useSearch({ strict: false }) as { tab?: Tab };
  const [tab, setTab] = useState<Tab>(search.tab ?? "domains");
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
        {tab === "rules" && <TrafficRules />}
        {tab === "firewall" && <ServerFirewall />}
        {tab === "domains" && <Domains q={domains} />}
      </div>
    </section>
  );
}

function Domains({ q }: { q: { data?: Domain[]; isPending: boolean; isError: boolean; error: unknown } }) {
  const { multi } = useClusters();
  const settings = useQuery({ queryKey: ["settings"], queryFn: () => settingsApi.get(), refetchInterval: 20000 });
  const console_ = settings.data?.consoleDomain ?? window.location.hostname;
  const consoleCert = settings.data?.certificates.find((c) => c.purpose === "console");
  const wildcard = settings.data?.certificates.find((c) => c.purpose === "apps-wildcard");
  if (q.isPending) return <p className="loading">Loading domains…</p>;
  return (
    <>
      {q.isError && <div className="banner bad" role="alert"><Icon name="alert" /><span>{errorText(q.error)}</span></div>}
      <div className="card scroll-x">
        <table className="t">
          <thead><tr><th>Hostname</th><th>Routes to</th>{multi && <th>DNS</th>}<th>Certificate</th><th>Expires</th><th>Listener</th></tr></thead>
          <tbody>
            <tr>
              <td className="nm">{console_}</td>
              <td>Kwerft console <Link to="/settings" className="dim">· Settings</Link></td>
              {multi && <td className="dim">—</td>}
              <td>{consoleCert ? <SettingsCert state={consoleCert.state} message={consoleCert.message} /> : <span className="pill mute nodot">Console</span>}</td>
              <td className="dim">{consoleCert?.notAfter ? shortDate(consoleCert.notAfter) : "—"}</td>
              <td className="mono dim">console</td>
            </tr>
            {wildcard && (
              <tr>
                <td className="nm">{wildcard.hostnames.join(", ")}</td>
                <td className="dim">Apps directly under it <Link to="/settings">· Settings</Link></td>
                {multi && <td className="dim">—</td>}
                <td><SettingsCert state={wildcard.state} message={wildcard.message} /> <span className="tag">DNS-01</span></td>
                <td className="dim">{wildcard.notAfter ? shortDate(wildcard.notAfter) : "—"}</td>
                <td className="mono dim">apps-wildcard</td>
              </tr>
            )}
            {(q.data ?? []).map((d) => (
              <tr key={`${d.project}/${d.name}`}>
                <td className="nm"><a href={`https://${d.hostname}`} target="_blank" rel="noreferrer">{d.hostname}</a></td>
                <td>
                  {d.app ? <Link to="/apps/$project/$name" params={{ project: d.project, name: d.app }}>{d.project}/{d.app}</Link> : <span className="dim">{d.project} · no app</span>}
                  <ClusterBadge cluster={d.cluster} />
                </td>
                {multi && (
                  <td>{d.dns ? <><RecordPill r={d.dns} />{d.dns.values.length > 0 && <span className="sub mono">{d.dns.values.join(", ")}</span>}</> : <span className="dim">—</span>}</td>
                )}
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
      <p className="dim note">Certificates renew automatically about a month before they expire. An apps domain with a DNS-01 wildcard certificate (Settings) serves every app under it from one listener.</p>
    </>
  );
}

function expiresSoon(iso?: string) {
  return !!iso && new Date(iso).getTime() - Date.now() < 14 * 86400000;
}

function SettingsCert({ state, message }: { state: CertificateState["state"]; message?: string }) {
  if (state === "valid") return <span className="pill ok" title={message}>Valid</span>;
  if (state === "issuing") return <span className="pill info" title={message}>Issuing</span>;
  return <span className="pill bad" title={message}>Failed</span>;
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
