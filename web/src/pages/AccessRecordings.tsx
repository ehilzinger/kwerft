// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { lazy, Suspense, useState } from "react";
import { Link, getRouteApi, useNavigate } from "@tanstack/react-router";
import { useQuery } from "@tanstack/react-query";
import { api } from "../api";
import { podsApi, type Recording, type RecordingFilter } from "../pods";
import { ago } from "../workloads";
import { errorText } from "./Apps";
import "../styles/workloads.css";
import "../styles/jobs.css";
import "../styles/recordings.css";
import { AccessLayout } from "./Access";

// Shell recordings under Access (/access/recordings): every shell session
// into an App replica or a Task's pod, newest first, for owners and admins.
// Recordings hold terminal output (keystrokes are not recorded) and can
// contain secrets, so the server allows only those roles and audits every
// playback and download (recording.go).

// The player bundles xterm.js: load it with the first playback.
const CastPlayerDialog = lazy(() => import("../components/CastPlayer").then((m) => ({ default: m.CastPlayerDialog })));

const route = getRouteApi("/authed/access/recordings");

export type RecordingsSearch = { user?: string; project?: string };

export const recordingsSearch = (s: Record<string, unknown>): RecordingsSearch => ({
  ...(typeof s.user === "string" && s.user ? { user: s.user } : {}),
  ...(typeof s.project === "string" && s.project ? { project: s.project } : {}),
});

export function AccessRecordings() {
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const role = session.data?.role;
  const allowed = role === "owner" || role === "admin";

  return (
    <AccessLayout current="recordings">
      <p className="dim">Shell recordings · kept 90 days · output only, keystrokes are not recorded</p>
      {session.isPending ? null : allowed ? (
        <Recordings />
      ) : (
        <div className="empty">
          <h2>Only owners and admins can see shell recordings</h2>
          <p>Recordings show everything a shell displayed, which can include secrets. Ask an owner or admin if you need to review a session.</p>
        </div>
      )}
    </AccessLayout>
  );
}

function Recordings() {
  const search = route.useSearch();
  const navigate = useNavigate({ from: "/access/recordings" });
  const filter: RecordingFilter = { user: search.user, project: search.project };
  const filtered = !!(search.user || search.project);
  const q = useQuery({
    queryKey: ["recordings", filter],
    queryFn: () => podsApi.recordings(filter),
    refetchInterval: (query) => (query.state.data?.some((r) => r.live) ? 10000 : 60000),
  });
  // The filter choices come from the unfiltered list (the same query when no filter is set).
  const all = useQuery({ queryKey: ["recordings", { user: undefined, project: undefined }], queryFn: () => podsApi.recordings(), enabled: filtered });
  const source = filtered ? all.data : q.data;
  const users = choices(source?.map((r) => r.user), search.user);
  const projects = choices(source?.map((r) => r.project), search.project);
  const [playing, setPlaying] = useState<Recording>();

  const set = (patch: RecordingsSearch) => void navigate({ search: (prev: RecordingsSearch) => recordingsSearch({ ...prev, ...patch }) });
  const list = q.data ?? [];

  return (
    <div className="recordings">
      <div className="toolbar" style={{ margin: "14px 0" }}>
        <select className="input" aria-label="Who" value={search.user ?? ""} onChange={(e) => set({ user: e.target.value || undefined })}>
          <option value="">Everyone</option>
          {users.map((u) => <option key={u} value={u}>{u}</option>)}
        </select>
        <select className="input" aria-label="Project" value={search.project ?? ""} onChange={(e) => set({ project: e.target.value || undefined })}>
          <option value="">All projects</option>
          {projects.map((p) => <option key={p} value={p}>{p}</option>)}
        </select>
        {filtered && <button type="button" className="btn sm" onClick={() => set({ user: undefined, project: undefined })}>Clear</button>}
        <p className="recordings-note push">Playing or downloading a recording is written to the audit log.</p>
      </div>

      {q.isPending ? (
        <p className="loading">Loading recordings…</p>
      ) : q.isError ? (
        <div className="banner bad" role="alert"><span>Could not load recordings: {errorText(q.error)}</span></div>
      ) : list.length === 0 ? (
        <div className="empty">
          <h2>{filtered ? "No recordings match" : "No shell sessions yet"}</h2>
          <p>{filtered ? "Nobody opened a shell with these filters in the last 90 days." : "Every shell opened from App or Task detail is recorded and listed here."}</p>
        </div>
      ) : (
        <div className="card" style={{ overflow: "hidden" }}>
          <div className="scroll-x">
            <table className="t">
              <thead>
                <tr>
                  <th>Started</th><th>Who</th><th>Project</th><th>App or run</th><th>Pod / container</th><th>Shell</th>
                  <th className="num">Duration</th><th>Ended</th><th className="num">Size</th><th aria-label="Actions" />
                </tr>
              </thead>
              <tbody>
                {list.map((r) => (
                  <tr key={r.id}>
                    <td title={new Date(r.started).toLocaleString()}>
                      {when(r.started)}<span className="sub">{ago(r.started)}</span>
                    </td>
                    <td>{r.user}<span className="sub mono">{r.ip}</span></td>
                    <td>{r.project}</td>
                    <td><Owner r={r} /></td>
                    <td className="mono">{r.pod}<span className="sub">{r.container}{r.debugContainer ? ` · via ${r.debugContainer}` : ""}</span></td>
                    <td>{shellLabel(r.shell)}</td>
                    <td className="num">{r.live ? <span className="dim">so far </span> : null}{seconds(r.durationSeconds)}</td>
                    <td><Ending r={r} /></td>
                    <td className="num">{bytes(r.bytes)}</td>
                    <td className="row-acts">
                      <button type="button" className="btn sm" onClick={() => setPlaying(r)} aria-label={`Play the session of ${r.user} in ${r.pod}`} title="Replay what the terminal showed">Play</button>
                      <a className="btn sm" href={podsApi.recordingURL(r.id)} download aria-label={`Download the session of ${r.user} in ${r.pod}`} title="asciicast v2: plays with asciinema">Download</a>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}
      {list.length >= 500 && <p className="recordings-note" style={{ marginTop: 8 }}>Showing the newest 500. Filter by person or project to see older ones.</p>}

      {playing && (
        <Suspense fallback={null}>
          <CastPlayerDialog rec={playing} onClose={() => setPlaying(undefined)} />
        </Suspense>
      )}
    </div>
  );
}

function Owner({ r }: { r: Recording }) {
  if (r.kind === "task" && r.task) {
    return <>
      <Link to="/jobs/$project/tasks/$name" params={{ project: r.project, name: r.task }}>{r.task}</Link>
      <span className="sub">run</span>
    </>;
  }
  if (r.app) {
    return <>
      <Link to="/apps/$project/$name" params={{ project: r.project, name: r.app }}>{r.app}</Link>
      <span className="sub">app</span>
    </>;
  }
  return <span className="dim">—</span>;
}

const endings: Record<string, [string, string]> = {
  exited: ["Exited", "ok"],
  idle: ["Idle timeout", "mute"],
  maxDuration: ["Time limit", "mute"],
  signedOut: ["Signed out", "mute"],
  disconnected: ["Disconnected", "mute"],
  recordingFull: ["Size limit", "warn"],
  noShell: ["No shell in image", "warn"],
  error: ["Error", "bad"],
};

function Ending({ r }: { r: Recording }) {
  if (r.live) return <span className="pill info">Live</span>;
  if (!r.ended) return <span className="pill warn" title="The console stopped during the session; the recording ends where it was cut.">Cut short</span>;
  const [label, tone] = endings[r.reason ?? ""] ?? [r.reason || "Ended", "mute"];
  return <span className={`pill ${tone}`}>{label}{r.reason === "exited" && r.exitCode !== undefined ? ` (${r.exitCode})` : ""}</span>;
}

function choices(values: string[] | undefined, current?: string) {
  const set = new Set(values ?? []);
  if (current) set.add(current);
  return [...set].sort();
}

export function shellLabel(shell: string) {
  return shell === "auto" ? "bash or sh" : shell === "debug" ? "debug toolbox" : shell;
}

function when(iso: string) {
  const d = new Date(iso);
  const today = new Date().toDateString() === d.toDateString();
  return today
    ? d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
    : d.toLocaleString([], { day: "2-digit", month: "short", hour: "2-digit", minute: "2-digit" });
}

function seconds(s: number) {
  const t = Math.round(s);
  if (t < 60) return `${t} s`;
  if (t < 3600) return `${Math.floor(t / 60)} min ${t % 60 ? `${t % 60} s` : ""}`.trim();
  return `${Math.floor(t / 3600)} h ${Math.floor((t % 3600) / 60)} min`;
}

function bytes(b: number) {
  if (b < 1024) return `${b} B`;
  if (b < 1 << 20) return `${(b / 1024).toFixed(b < 10 << 10 ? 1 : 0)} KiB`;
  return `${(b / (1 << 20)).toFixed(1)} MiB`;
}
