import type { DNSRecord } from "../settings";

// DNS records Kwerft keeps: the console's hostnames and *.<apps domain>
// (Settings) and remote clusters' hostnames under the apps domain
// (docs/phase5.md › DNS for remote clusters).

export function RecordPill({ r }: { r: Pick<DNSRecord, "state" | "message"> }) {
  switch (r.state) {
    case "Managed": return <span className="pill ok" title={r.message}>Managed</span>;
    case "External": return <span className="pill ok" title={r.message}>Yours, points here</span>;
    case "Conflict": return <span className="pill bad" title={r.message}>Conflict</span>;
    case "TakenOver": return <span className="pill warn" title={r.message}>Another installation</span>;
    case "NoZone": return <span className="pill mute" title={r.message}>Not in your zones</span>;
    case "Pending": return <span className="pill info" title={r.message}>Waiting</span>;
    case "Unsupported": return <span className="pill mute" title={r.message}>By hand</span>;
    default: return <span className="pill bad" title={r.message}>Error</span>;
  }
}

const purposes: Record<DNSRecord["purpose"], string> = {
  console: "Console",
  "console-next": "New console hostname",
  "console-previous": "Previous console hostname",
  apps: "Apps",
  app: "App",
};

/** The records as a table; "For" names the purpose, or the project of an app's hostname. */
export function DNSRecordsTable({ records }: { records: DNSRecord[] }) {
  return (
    <div className="scroll-x">
      <table className="t">
        <thead><tr><th>Hostname</th><th>For</th><th>Points to</th><th>State</th></tr></thead>
        <tbody>
          {records.map((r) => (
            <tr key={r.hostname}>
              <td className="nm mono">{r.hostname}{r.zone && <span className="sub">zone {r.zone}</span>}</td>
              <td>{r.purpose === "app" && r.project ? <>project <b>{r.project}</b></> : (purposes[r.purpose] ?? r.purpose)}</td>
              <td className="mono">{r.values.length ? r.values.join(", ") : "—"}</td>
              <td><RecordPill r={r} />{r.message && r.state !== "Managed" && <span className="sub wrap">{r.message}</span>}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
