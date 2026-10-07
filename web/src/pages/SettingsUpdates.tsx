// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { Fragment, useEffect, useMemo, useState, type FormEvent, type ReactNode } from "react";
import { Link } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../api";
import { Dialog } from "../components/Dialog";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { ago } from "../workloads";
import { LOCAL, useClusters } from "../clusters";
import {
  DAYS, durationText, enablesAutoPatch, fleetSteps, localTimeZone, needsTypedConfirmation, noticeText, notesBlocks, phasePill, progressLines,
  streamUpgrade, summaryLine, updateKeys, updateNotices, updatesApi, windowText,
  type Available, type ClusterUpdates, type Line, type Policy, type Preflight, type Upgrade, type UpgradeAll, type UpdatePolicy, type Updates,
} from "../updates";
import "../styles/workloads.css";
import "../styles/jobs.css";
import "../styles/settings.css";
import "../styles/updates.css";

const unreachable = "The console could not be reached. Check your connection and try again.";
const errText = (e: unknown) => (e instanceof ApiError ? e.message : unreachable);

// ---- Settings tabs ---------------------------------------------------------------------------

/** Whether the signed-in user may see Settings › Updates (owners and admins). */
function useCanReadUpdates() {
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const role = session.data?.role;
  return !!session.data && !session.data.mustEnrol && (role === "owner" || role === "admin");
}

/**
 * The newest allowed release per component, for the notification on the
 * Overview and the dot on Settings › Updates. Empty for roles that cannot
 * read updates, and while the policy is Off.
 */
export function useUpdateNotices(): Available[] {
  const enabled = useCanReadUpdates();
  const q = useQuery({ queryKey: updateKeys.overview, queryFn: updatesApi.overview, enabled, refetchInterval: 5 * 60_000, retry: false });
  return enabled ? updateNotices(q.data) : [];
}

export type SettingsTab = "general" | "updates";

/** General | Updates, under the Settings header. */
export function SettingsTabs({ current }: { current: SettingsTab }) {
  const canUpdates = useCanReadUpdates();
  const notices = useUpdateNotices();
  if (!canUpdates) return null;
  return (
    <nav className="tabs" aria-label="Settings">
      <Link to="/settings" className={current === "general" ? "on" : undefined} aria-current={current === "general" ? "page" : undefined}>General</Link>
      <Link to="/settings/updates" className={current === "updates" ? "on" : undefined} aria-current={current === "updates" ? "page" : undefined}>
        Updates
        {notices.length > 0 && <span className="upd-dot" title={notices.map((n) => `${noticeText(n).title} available`).join(", ")}><span className="sr"> (update available)</span></span>}
      </Link>
    </nav>
  );
}

// ---- the page --------------------------------------------------------------------------------

/** What the dialog offers: the allowed targets of one component in one cluster, newest first. */
type Target = { cluster: string; offers: Available[]; from?: string };
type Watch = { cluster: string; name: string };

// Settings › Updates: what every cluster runs and could run, the update
// policy, upgrades with their progress (styled like the installer's
// output) and the history. Owners upgrade and set the policy; admins read.
export function SettingsUpdates() {
  const canRead = useCanReadUpdates();
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const queryClient = useQueryClient();
  const updates = useQuery({
    queryKey: updateKeys.overview, queryFn: updatesApi.overview, enabled: canRead,
    refetchInterval: (q) => (q.state.data?.checking || q.state.data?.clusters.some((c) => c.active) ? 3000 : 15000),
  });
  const history = useQuery({ queryKey: updateKeys.history, queryFn: () => updatesApi.history(), enabled: canRead, refetchInterval: 15000 });
  const [dialog, setDialog] = useState<Target>();
  const [all, setAll] = useState<UpgradeAll>();
  const [picked, setPicked] = useState<Watch>();

  // The upgrade in view: the one picked (started here or chosen in the
  // history), else one under way, else the latest if it finished today.
  const watch = useMemo<Watch | undefined>(() => {
    if (picked) return picked;
    const active = updates.data?.clusters.find((c) => c.active)?.active;
    if (active) return { cluster: active.cluster, name: active.name };
    const last = history.data?.[0];
    if (last && last.finishedAt && Date.now() - new Date(last.finishedAt).getTime() < 86400_000) return { cluster: last.cluster, name: last.name };
    return undefined;
  }, [picked, updates.data, history.data]);

  const refresh = () => {
    void queryClient.invalidateQueries({ queryKey: updateKeys.overview });
    void queryClient.invalidateQueries({ queryKey: updateKeys.history });
  };

  return (
    <section className="view settings updates">
      <div className="ph">
        <div>
          <h1>Settings</h1>
          <p>Kwerft and Kubernetes versions, the update policy and upgrades from the console</p>
        </div>
      </div>
      <SettingsTabs current="updates" />
      {session.data && !canRead && (
        <div className="banner info"><Icon name="shield" /><span>Only owners and admins see updates.</span></div>
      )}
      {canRead && updates.isPending && <p className="loading">Loading updates…</p>}
      {updates.isError && <div className="banner bad" role="alert"><Icon name="alert" /><span>{errText(updates.error)}</span></div>}
      {updates.data && (
        <>
          <PausedBanner u={updates.data} onResumed={refresh} />
          <VersionsCard u={updates.data} onUpgrade={setDialog} onUpgradeAll={setAll} onWatch={setPicked} />
          {watch && <ProgressCard key={`${watch.cluster}/${watch.name}`} watch={watch} canCancel={updates.data.canUpgrade} onFinished={refresh}
            onClose={picked ? () => setPicked(undefined) : undefined} />}
          <NotesCard u={updates.data} />
          <PolicyCard u={updates.data} onSaved={refresh} />
          <HistoryCard list={history.data} error={history.error} pending={history.isPending} selected={watch} onSelect={setPicked} />
        </>
      )}
      {dialog && <UpgradeDialog target={dialog} onClose={() => setDialog(undefined)}
        onStarted={(u) => {
          setDialog(undefined);
          setPicked({ cluster: u.cluster, name: u.name });
          refresh();
        }} />}
      {all && <UpgradeAllDialog plan={all} current={updates.data?.current.kwerft} onClose={() => setAll(undefined)}
        onStarted={(w) => {
          setAll(undefined);
          if (w) setPicked(w);
          refresh();
        }} />}
    </section>
  );
}

function PausedBanner({ u, onResumed }: { u: Updates; onResumed: () => void }) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  if (!u.autoPatchPausedBy || u.policy.policy !== "AutoPatch") return null;
  async function resume() {
    setBusy(true);
    setError(undefined);
    try {
      await updatesApi.resumeAutoPatch();
      onResumed();
    } catch (err) {
      setError(errText(err));
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="banner warn" role="status">
      <Icon name="alert" />
      <span>
        <b>AutoPatch is paused.</b> The automatic upgrade <code>{u.autoPatchPausedBy}</code> failed or was rolled back; no patch release is installed
        by itself until an owner resumes it.{error && <span className="form-error"> {error}</span>}
      </span>
      {u.canUpgrade && <button className="btn sm" onClick={resume} disabled={busy}>{busy ? "Resuming…" : "Resume AutoPatch"}</button>}
    </div>
  );
}

// ---- versions --------------------------------------------------------------------------------

function VersionsCard({ u, onUpgrade, onUpgradeAll, onWatch }: {
  u: Updates; onUpgrade: (t: Target) => void; onUpgradeAll: (p: UpgradeAll) => void; onWatch: (w: Watch) => void;
}) {
  const { multi } = useClusters();
  const queryClient = useQueryClient();
  const [checking, setChecking] = useState(false);
  const [error, setError] = useState<string>();
  async function checkNow() {
    setChecking(true);
    setError(undefined);
    try {
      await updatesApi.check();
      await queryClient.invalidateQueries({ queryKey: updateKeys.overview });
    } catch (err) {
      setError(errText(err));
    } finally {
      setChecking(false);
    }
  }
  const off = u.policy.policy === "Off";
  return (
    <div className="card">
      <div className="ch-h">
        <h3>Versions</h3>
        <span className="acts">
          <span className="dim small" title={u.checkedAt ? new Date(u.checkedAt).toLocaleString() : undefined}>
            {off ? "Updates are off" : u.checking ? "Checking for releases…" : u.checkedAt ? `Checked ${ago(u.checkedAt)}` : "Not checked yet"}
          </span>
          <button className="btn sm" onClick={checkNow} disabled={off || checking || u.checking}>{checking || u.checking ? "Checking…" : "Check now"}</button>
          {u.upgradeAll && u.canUpgrade && (
            <button className="btn sm pri" onClick={() => onUpgradeAll(u.upgradeAll!)}
              title={fleetSteps(u.upgradeAll, u.current.kwerft).join(", then ")}>
              Upgrade all to {u.upgradeAll.version}…
            </button>
          )}
        </span>
      </div>
      {(error || u.error) && <div className="bd"><p className="form-error" role="alert">{error ?? `The last check failed: ${u.error}`}</p></div>}
      <div className="scroll-x">
        <table className="t versions">
          <thead><tr>{multi && <th>Cluster</th>}<th>Component</th><th>Running</th><th>Available</th><th><span className="sr">Actions</span></th></tr></thead>
          <tbody>
            {u.clusters.map((c) => (
              <Fragment key={c.name}>
                <VersionRow c={c} component="Kwerft" running={c.kwerft} multi={multi} u={u} onUpgrade={onUpgrade} onWatch={onWatch} first />
                <VersionRow c={c} component="Kubernetes" running={c.kubernetes} multi={multi} u={u} onUpgrade={onUpgrade} onWatch={onWatch} />
              </Fragment>
            ))}
          </tbody>
        </table>
      </div>
      {u.clusters.length > 1 && (
        <div className="bd sep"><p className="dim note">The console upgrades itself first, then each connected cluster follows its release, one after another
          (<b>Upgrade all</b>); a cluster that fails or rolls back stops the rest. Kubernetes is upgraded per cluster.</p></div>
      )}
    </div>
  );
}

function VersionRow({ c, component, running, multi, u, onUpgrade, onWatch, first }: {
  c: ClusterUpdates; component: "Kwerft" | "Kubernetes"; running?: string; multi: boolean; u: Updates;
  onUpgrade: (t: Target) => void; onWatch: (w: Watch) => void; first?: boolean;
}) {
  const offers = c.available.filter((a) => a.component === component);
  const best = offers.find((a) => a.allowed);
  const active = c.active?.component === component ? c.active : undefined;
  const blockedBy = c.active && !active ? c.active : undefined;
  return (
    <tr>
      {multi && (first ? <td className="nm" rowSpan={2}>{c.name}{!c.connected && <span className="sub">not connected</span>}</td> : null)}
      <td>{component === "Kwerft" ? (c.name === LOCAL ? "Kwerft" : "Kwerft agent") : "Kubernetes (k3s)"}</td>
      <td className="mono">{running || "—"}</td>
      <td>
        {offers.length === 0 ? (
          <span className="dim">{u.policy.policy === "Off" && c.name === LOCAL ? "—" : "Up to date"}</span>
        ) : (
          <div className="offers">
            {offers.map((a) => (
              <span key={a.version} className={a.allowed ? "offer" : "offer dim"} title={a.reason}>
                <span className="mono">{a.version}</span> <span className="pill nodot mute">{a.kind === "Patch" ? "patch" : "minor"}</span>
                {!a.allowed && a.reason && <span className="sub wrap">{a.reason}</span>}
              </span>
            ))}
          </div>
        )}
      </td>
      <td className="row-acts">
        {active ? (
          <button className="btn sm" onClick={() => onWatch({ cluster: c.name, name: active.name })}>
            <span className={`pill ${phasePill(active.phase).pill}`}>{phasePill(active.phase).label}</span> View
          </button>
        ) : best && u.canUpgrade && c.upgradable && c.connected ? (
          <button className="btn sm pri" disabled={!!blockedBy} title={blockedBy ? `Wait for ${blockedBy.name} to finish` : undefined}
            onClick={() => onUpgrade({ cluster: c.name, offers: offers.filter((o) => o.allowed), from: running })}>
            Upgrade…
          </button>
        ) : best && !c.upgradable && c.name !== LOCAL ? (
          <span className="dim small wrap">Re-run the installer of {best.version} on its server</span>
        ) : null}
      </td>
    </tr>
  );
}

// ---- release notes ---------------------------------------------------------------------------

function NotesCard({ u }: { u: Updates }) {
  const kwerft = u.available.filter((a) => a.component === "Kwerft");
  if (!kwerft.length) return null;
  return (
    <div className="card">
      <div className="ch-h"><h3>Release notes</h3></div>
      <div className="bd stack tight">
        {kwerft.map((a, i) => (
          <details key={a.version} className="notes" open={i === 0}>
            <summary><b>Kwerft {a.version}</b> <span className="pill nodot mute">{a.kind === "Patch" ? "patch" : "minor"}</span>{!a.allowed && <span className="dim small"> · {a.reason}</span>}</summary>
            {a.notes ? <Notes md={a.notes} /> : <p className="dim">No notes were published with this release.</p>}
          </details>
        ))}
      </div>
    </div>
  );
}

function Notes({ md }: { md: string }) {
  const blocks = notesBlocks(md);
  return (
    <div className="notes-body">
      {blocks.map((b, i) => {
        switch (b.type) {
          case "h": return b.level <= 2 ? <h4 key={i}>{b.text}</h4> : <h5 key={i}>{b.text}</h5>;
          case "p": return <p key={i}>{b.text}</p>;
          case "ul": return <ul key={i}>{b.items.map((t, j) => <li key={j}>{t}</li>)}</ul>;
          case "code": return <pre key={i}>{b.text}</pre>;
        }
      })}
    </div>
  );
}

// ---- progress --------------------------------------------------------------------------------

/** Follows an Upgrade's live status, reconnecting while the console restarts. */
function useUpgradeStream(watch: Watch) {
  const [upgrade, setUpgrade] = useState<Upgrade>();
  const [reconnecting, setReconnecting] = useState(false);
  const [problem, setProblem] = useState<string>();
  useEffect(() => {
    const ctl = new AbortController();
    let stop = false;
    void (async () => {
      let delay = 1000;
      while (!stop && !ctl.signal.aborted) {
        let finished = false;
        try {
          await streamUpgrade(watch.name, watch.cluster, (e) => {
            if (e.type === "upgrade") {
              setUpgrade(e.upgrade);
              setReconnecting(false);
              setProblem(undefined);
              finished = e.upgrade.finished;
              delay = 1000;
            } else if (e.type === "gone") {
              setProblem("This upgrade no longer exists.");
              stop = true;
            }
          }, ctl.signal);
        } catch (err) {
          if (ctl.signal.aborted) return;
          if (err instanceof ApiError && [401, 403, 404].includes(err.status)) {
            setProblem(err.message);
            return;
          }
        }
        if (stop || finished || ctl.signal.aborted) return;
        // The stream broke: the console restarts during a Kwerft upgrade.
        setReconnecting(true);
        await new Promise((r) => setTimeout(r, delay));
        delay = Math.min(delay * 2, 8000);
      }
    })();
    return () => {
      stop = true;
      ctl.abort();
    };
  }, [watch.cluster, watch.name]);
  return { upgrade, reconnecting, problem };
}

const marks: Record<Line["mark"], ReactNode> = {
  ok: <span className="g">✓</span>,
  fail: <span className="x">✗</span>,
  skip: <span className="d">–</span>,
  wait: <span className="d">·</span>,
  run: <span className="spin a" aria-label="running" />,
};

function ProgressCard({ watch, canCancel, onFinished, onClose }: { watch: Watch; canCancel: boolean; onFinished: () => void; onClose?: () => void }) {
  const { upgrade: streamed, reconnecting, problem } = useUpgradeStream(watch);
  // An "Upgrade all" goes on in the agent clusters after the console's own
  // upgrade finished (and its stream ended): follow them on its line.
  const followFleet = !!streamed?.finished && !!streamed.agentClusters && !streamed.agentClusters.finished;
  const polled = useQuery({
    queryKey: updateKeys.upgrade(watch.cluster, watch.name), queryFn: () => updatesApi.upgrade(watch.name, watch.cluster),
    enabled: followFleet, refetchInterval: (q) => (q.state.data?.agentClusters?.finished ? false : 5000),
  });
  const u = followFleet && polled.data ? polled.data : streamed;
  const queryClient = useQueryClient();
  const loaded = useQuery({ queryKey: ["version"], queryFn: api.version, staleTime: Infinity });
  const [newVersion, setNewVersion] = useState<string>();
  const [cancelling, setCancelling] = useState(false);
  const [error, setError] = useState<string>();
  const [showLog, setShowLog] = useState(false);
  const finished = u?.finished ?? false;

  // When it finishes: refresh the page's data; after a Kwerft upgrade, the
  // console may run another version than this page was loaded from.
  useEffect(() => {
    if (!finished || !u) return;
    onFinished();
    if (u.component === "Kwerft" && u.cluster === LOCAL) {
      void api.version().then((v) => {
        if (loaded.data && v.version !== loaded.data.version) setNewVersion(v.version);
      }).catch(() => undefined);
    }
  }, [finished]);

  async function cancel() {
    if (!u) return;
    setCancelling(true);
    setError(undefined);
    try {
      await updatesApi.cancel(u.name, u.cluster);
      void queryClient.invalidateQueries({ queryKey: updateKeys.history });
    } catch (err) {
      setError(errText(err));
    } finally {
      setCancelling(false);
    }
  }

  const pill = u ? phasePill(u.phase) : undefined;
  const summary = u ? summaryLine(u) : undefined;
  return (
    <div className="card upgrade-progress" aria-live="polite">
      <div className="ch-h">
        <h3>
          {u ? <>Upgrade of {u.component === "Kwerft" ? "Kwerft" : "Kubernetes"} to <span className="mono">{u.version}</span></> : "Upgrade"}
          {pill && <span className={`pill ${pill.pill}`}>{pill.label}</span>}
        </h3>
        <span className="acts">
          {u?.cancellable && canCancel && !u.cancelRequestedBy && (
            <button className="btn sm danger" onClick={cancel} disabled={cancelling}>{cancelling ? "Cancelling…" : "Cancel upgrade"}</button>
          )}
          {u?.cancelRequestedBy && !u.finished && <span className="dim small">Cancel requested by {u.cancelRequestedBy}</span>}
          {onClose && <button className="btn sm ghost" onClick={onClose}>Close</button>}
        </span>
      </div>
      {reconnecting && (
        <div className="banner info reconnect" role="status"><span className="spin" aria-hidden="true" /><span><b>Reconnecting…</b> {u?.component === "Kwerft" ? "The console restarts during the upgrade; this takes a few seconds." : "The connection to the console was lost."}</span></div>
      )}
      {newVersion && (
        <div className="banner info" role="status"><Icon name="rocket" /><span>The console now runs Kwerft <b>{newVersion}</b>. Reload the page to use it.</span>
          <button className="btn sm" onClick={() => window.location.reload()}>Reload</button></div>
      )}
      {problem && <div className="bd"><p className="form-error" role="alert">{problem}</p></div>}
      {error && <div className="bd"><p className="form-error" role="alert">{error}</p></div>}
      {!u && !problem && <p className="loading pad">Connecting…</p>}
      {u && (
        <div className="term upgrade-term">
          <div className="a">▸ {u.component === "Kwerft" ? `Kwerft ${u.from?.kwerft ?? ""} → ${u.version}` : `Kubernetes ${u.from?.kubernetes ?? ""} → ${u.version}`}
            <span className="d">  {u.cluster !== LOCAL ? `cluster=${u.cluster} · ` : ""}{u.fleet || u.agentClusters ? "Upgrade all · " : ""}requested by {u.auto ? "AutoPatch" : u.requestedBy ?? "?"}{u.startedAt ? ` · started ${new Date(u.startedAt).toLocaleTimeString()}` : ""}</span>
          </div>
          {(u.phase === "Pending" || u.phase === "Queued") && <div className="d">{u.message || "Waiting for the Upgrade controller…"}</div>}
          <ol className="lines">
            {progressLines(u).map((l, i) => (
              <li key={i} className={`m-${l.mark}`}>
                <span className="mk">{marks[l.mark]}</span>
                <span className="lb">{l.label}</span>
                {l.detail && <span className="dt">{l.detail}</span>}
              </li>
            ))}
          </ol>
          {summary && <div className={`sum ${summary.tone}`}>{summary.text}</div>}
          {u.phase === "Running" && u.message && <div className="d">{u.message}</div>}
        </div>
      )}
      {u && (
        <div className="bd sep upgrade-foot">
          {u.component === "Kwerft" && (
            <button className="btn sm" onClick={() => setShowLog(!showLog)} aria-expanded={showLog}>{showLog ? "Hide the installer log" : "Show the installer log"}</button>
          )}
          {u.component === "Kubernetes" && u.phase === "Failed" && (
            <p className="dim note">
              Kubernetes is never rolled back automatically: k3s cannot downgrade, and restoring etcd resets the cluster.
              {u.backup?.etcdSnapshot && <> The snapshot <code>{u.backup.etcdSnapshot}</code> was taken before the upgrade; restoring it is a manual step
              (<code>k3s server --cluster-reset --cluster-reset-restore-path=…</code>) described in the docs.</>}
            </p>
          )}
          {showLog && <InstallerLog u={u} />}
        </div>
      )}
    </div>
  );
}

function InstallerLog({ u }: { u: Upgrade }) {
  const log = useQuery({
    queryKey: updateKeys.log(u.cluster, u.name), queryFn: () => updatesApi.log(u.name, u.cluster),
    refetchInterval: u.finished ? false : 10000,
  });
  if (log.isPending) return <p className="loading">Loading the log…</p>;
  if (log.isError) return <p className="form-error" role="alert">{errText(log.error)}</p>;
  if (!log.data.log) return <p className="dim note">No log yet: the runner keeps the last 64 KiB of <code>/var/log/kwerft/install.log</code> here after each stage.</p>;
  return (
    <>
      {log.data.truncated && <p className="dim note">The last 64 KiB of <code>/var/log/kwerft/install.log</code>; the whole log is on the server.</p>}
      <pre className="term upgrade-log">{log.data.log}</pre>
    </>
  );
}

// ---- the dialog ------------------------------------------------------------------------------

function UpgradeDialog({ target, onClose, onStarted }: { target: Target; onClose: () => void; onStarted: (u: Upgrade) => void }) {
  const { cluster, offers } = target;
  const [version, setVersion] = useState(offers[0]!.version);
  const a = offers.find((o) => o.version === version) ?? offers[0]!;
  const [accept, setAccept] = useState(false);
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ field?: string; message: string }>();
  const pre = useQuery({
    queryKey: ["upgrade-preflight", cluster, a.component, a.version, accept],
    queryFn: () => updatesApi.preflight({ cluster, component: a.component, version: a.version, acceptDataRollback: accept }),
    retry: false, staleTime: 0, gcTime: 0, refetchOnWindowFocus: false,
  });
  const p = pre.data;
  const typed = p?.confirmVersion ?? needsTypedConfirmation(a.component, a.kind);
  const k8s = a.component === "Kubernetes";
  const fieldError = (f: string) => (error?.field === f ? error.message : undefined);
  const ready = !!p && !p.blocked && !!password && (!typed || confirm.trim() === a.version) && (!p.dataRollback || accept) && !busy;

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!ready) return;
    setBusy(true);
    setError(undefined);
    try {
      const res = await updatesApi.start({
        cluster, component: a.component, version: a.version, acceptDataRollback: accept, password, confirmVersion: typed ? confirm.trim() : undefined,
      });
      onStarted(res.upgrade);
    } catch (err) {
      setError(err instanceof ApiError ? { field: err.field, message: err.message } : { message: unreachable });
      if (err instanceof ApiError && err.status === 409) void pre.refetch();
    } finally {
      setBusy(false);
    }
  }

  return (
    <Dialog title={`Upgrade ${k8s ? "Kubernetes" : "Kwerft"} to ${a.version}`} onClose={onClose} onSubmit={submit} wide
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button type="submit" className="btn pri" disabled={!ready}>{busy ? "Starting…" : `Upgrade to ${a.version}`}</button>
      </>}>
      <p className="upgrade-what">
        <span className="mono">{target.from || p?.from || "?"}</span> → <span className="mono">{a.version}</span>{" "}
        <span className="pill nodot mute">{(p?.kind ?? a.kind) === "Patch" ? "patch" : "minor"}</span>
        {cluster !== LOCAL && <span className="dim"> · cluster {cluster}</span>}
      </p>
      {offers.length > 1 && (
        <div className="field">
          <label htmlFor="upgrade-version">Version</label>
          <select id="upgrade-version" className="input" value={version} onChange={(e) => { setVersion(e.target.value); setAccept(false); setConfirm(""); }}>
            {offers.map((o) => <option key={o.version} value={o.version}>{o.version} ({o.kind === "Patch" ? "patch" : "minor"} release)</option>)}
          </select>
        </div>
      )}
      {k8s ? (
        <div className="banner warn"><Icon name="alert" /><span>
          <b>Kubernetes cannot be rolled back automatically.</b> k3s does not downgrade. Nodes are upgraded one at a time, the control plane first;
          workers are drained. An etcd snapshot is taken first, and a failure stops before the next node.
        </span></div>
      ) : (
        <p className="dim">The console restarts for a few seconds; apps keep serving. If the installer or the checks after it fail, Kwerft rolls back to{" "}
          <span className="mono">{target.from || "the running version"}</span> by itself.</p>
      )}

      <PreflightField pre={pre} />
      {p?.dataRollback && <DataRollback version={a.version} accept={accept} onChange={setAccept} />}
      {typed && (
        <Field id="upgrade-confirm" label={`Type ${a.version} to confirm`} className="mono" value={confirm} autoComplete="off" spellCheck={false}
          onChange={(e) => setConfirm(e.target.value)} error={fieldError("confirmVersion")}
          hint="A minor upgrade of Kubernetes cannot be rolled back automatically." />
      )}
      <Field id="upgrade-password" label="Your password" type="password" value={password} autoComplete="current-password"
        onChange={(e) => setPassword(e.target.value)} error={fieldError("password")}
        hint="Or a current code from your authenticator app." />
      {error && !error.field && <p className="form-error" role="alert">{error.message}</p>}
    </Dialog>
  );
}

/** The live preflight of a dialog: each check, and Check again. */
function PreflightField({ pre }: { pre: { data?: Preflight; isFetching: boolean; isError: boolean; error: unknown; refetch: () => Promise<unknown> } }) {
  const p = pre.data;
  return (
    <div className="field">
      <span className="label">Preflight</span>
      {pre.isFetching && !p && <p className="loading"><span className="spin" aria-hidden="true" /> Checking nodes, disk, the release and its image…</p>}
      {pre.isError && <p className="form-error" role="alert">{errText(pre.error)}</p>}
      {p && (
        <ul className="checks" aria-busy={pre.isFetching}>
          {p.checks.map((c, i) => (
            <li key={i} className={c.ok ? "c-ok" : c.warning ? "c-warn" : "c-bad"}>
              <span className="mk" aria-label={c.ok ? "passed" : c.warning ? "warning" : "failed"}>{c.ok ? "✓" : c.warning ? "!" : "✗"}</span>
              <span><b>{checkLabel(c.check)}</b> {c.message}</span>
            </li>
          ))}
        </ul>
      )}
      {p && <button type="button" className="btn sm linkish" onClick={() => void pre.refetch()} disabled={pre.isFetching}>{pre.isFetching ? "Checking…" : "Check again"}</button>}
    </div>
  );
}

function DataRollback({ version, accept, onChange }: { version: string; accept: boolean; onChange: (v: boolean) => void }) {
  return (
    <label className="check">
      <input type="checkbox" checked={accept} onChange={(e) => onChange(e.target.checked)} />
      <span>
        <b>Accept a data rollback</b>
        <small>{version} is not rollback-safe: if the upgrade fails, the rollback also restores the console&apos;s database from the copy taken
          before it, and changes made in between are lost.</small>
      </span>
    </label>
  );
}

// "Upgrade all": the console first (its preflight, as for one upgrade),
// then every agent cluster behind it, one after another.
function UpgradeAllDialog({ plan, current, onClose, onStarted }: {
  plan: UpgradeAll; current?: string; onClose: () => void; onStarted: (w?: Watch) => void;
}) {
  const [accept, setAccept] = useState(false);
  const [password, setPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ field?: string; message: string }>();
  const pre = useQuery({
    queryKey: ["upgrade-preflight", LOCAL, "Kwerft", plan.version, accept],
    queryFn: () => updatesApi.preflight({ cluster: LOCAL, component: "Kwerft", version: plan.version, acceptDataRollback: accept }),
    enabled: plan.console, retry: false, staleTime: 0, gcTime: 0, refetchOnWindowFocus: false,
  });
  const p = pre.data;
  const ready = !!password && !busy && (!plan.console || (!!p && !p.blocked && (!p.dataRollback || accept)));

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!ready) return;
    setBusy(true);
    setError(undefined);
    try {
      const res = await updatesApi.startAll({ version: plan.version, acceptDataRollback: accept, password });
      const first = res.upgrade ?? res.members[0];
      onStarted(first ? { cluster: first.cluster, name: first.name } : undefined);
    } catch (err) {
      setError(err instanceof ApiError ? { field: err.field, message: err.message } : { message: unreachable });
      if (err instanceof ApiError && err.status === 409 && plan.console) void pre.refetch();
    } finally {
      setBusy(false);
    }
  }

  const steps = fleetSteps(plan, current);
  return (
    <Dialog title={`Upgrade all to Kwerft ${plan.version}`} onClose={onClose} onSubmit={submit} wide
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button type="submit" className="btn pri" disabled={!ready}>{busy ? "Starting…" : `Upgrade ${steps.length} ${steps.length === 1 ? "cluster" : "clusters"}`}</button>
      </>}>
      <div className="field">
        <span className="label">One after another</span>
        <ol className="fleet-plan">{steps.map((s) => <li key={s}>{s}</li>)}</ol>
        {plan.skipped.length > 0 && (
          <ul className="checks">
            {plan.skipped.map((s) => (
              <li key={s.cluster} className={s.warning ? "c-warn" : "c-ok"}>
                <span className="mk" aria-label={s.warning ? "warning" : "skipped"}>{s.warning ? "!" : "–"}</span>
                <span><b>{s.cluster}</b> left out: {s.reason}.</span>
              </li>
            ))}
          </ul>
        )}
      </div>
      <p className="dim">
        {plan.console ? "The console upgrades first and restarts for a few seconds; each agent cluster starts once the one before it is done. " : "Each agent cluster starts once the one before it is done. "}
        Each one rolls back by itself if it fails, and a cluster that fails or rolls back stops the rest: they are cancelled before anything changes there.
      </p>
      {plan.console && <PreflightField pre={pre} />}
      {p?.dataRollback && <DataRollback version={plan.version} accept={accept} onChange={setAccept} />}
      <Field id="upgrade-all-password" label="Your password" type="password" value={password} autoComplete="current-password"
        onChange={(e) => setPassword(e.target.value)} error={error?.field === "password" ? error.message : undefined}
        hint="Or a current code from your authenticator app." />
      {error && error.field !== "password" && <p className="form-error" role="alert">{error.message}</p>}
    </Dialog>
  );
}

const checkLabels: Record<string, string> = {
  Target: "Release", ImagePullable: "Image", NodesReady: "Nodes", DiskSpace: "Disk", NoOtherOperation: "Other operations",
  ReleaseInstall: "Install", InstallerNode: "Installer node", AgentSkew: "Agents", DataRollback: "Rollback", AgentTarget: "Console",
};
const checkLabel = (c: string) => checkLabels[c] ?? c;

// ---- policy ----------------------------------------------------------------------------------

const policies: { id: Policy; title: string; text: string }[] = [
  { id: "Off", title: "Off", text: "No release checks and no outbound requests. Upgrade by re-running the installer." },
  { id: "Notify", title: "Notify", text: "Check for releases every 6 hours and show them here and on the Overview. Every upgrade is a click." },
  { id: "AutoPatch", title: "AutoPatch", text: "Also install patch releases of the running minor in the maintenance window. Minor releases always need a click." },
];

const durations = ["1h", "2h", "3h", "4h", "6h", "8h"];

function PolicyCard({ u, onSaved }: { u: Updates; onSaved: () => void }) {
  const canEdit = u.canUpgrade;
  const [policy, setPolicy] = useState<Policy>(u.policy.policy);
  const [channel, setChannel] = useState(u.policy.channel);
  const [k8sPatches, setK8sPatches] = useState(u.policy.kubernetesPatches);
  const [days, setDays] = useState<string[]>(u.policy.window?.days ?? ["Sun"]);
  const [start, setStart] = useState(u.policy.window?.start ?? "03:00");
  const [duration, setDuration] = useState(u.policy.window?.duration ?? "2h");
  const [zone, setZone] = useState(u.policy.window?.timeZone ?? localTimeZone());
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<{ field?: string; message: string }>();
  const [saved, setSaved] = useState(false);
  const [password, setPassword] = useState("");
  const fieldError = (f: string) => (error?.field === f ? error.message : undefined);
  const needsWindow = policy === "AutoPatch";
  const kubernetesPatches = policy === "AutoPatch" && k8sPatches;
  // Turning unattended upgrades on is confirmed like starting one.
  const needsPassword = enablesAutoPatch(u.policy, { policy, kubernetesPatches });

  async function save(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    setSaved(false);
    const p: UpdatePolicy = {
      policy, channel, kubernetesPatches,
      window: needsWindow || u.policy.window ? { days, start, duration, timeZone: zone.trim() || undefined } : undefined,
    };
    try {
      await updatesApi.setPolicy(needsPassword ? { ...p, password } : p);
      setSaved(true);
      setPassword("");
      onSaved();
    } catch (err) {
      setError(err instanceof ApiError ? { field: err.field, message: err.message } : { message: unreachable });
    } finally {
      setBusy(false);
    }
  }

  const toggleDay = (d: string) => setDays(days.includes(d) ? days.filter((x) => x !== d) : DAYS.filter((x) => x === d || days.includes(x)));

  return (
    <form className="card" onSubmit={save}>
      <div className="ch-h"><h3>Update policy</h3>{!canEdit && <span className="dim small">Only owners change the policy</span>}</div>
      <div className="bd stack">
        <div className="radio three" role="group" aria-label="Update policy">
          {policies.map((o) => (
            <button key={o.id} type="button" className="opt" aria-pressed={policy === o.id} disabled={!canEdit} onClick={() => setPolicy(o.id)}>
              <span className="r" /><b>{o.title}</b><span>{o.text}</span>
            </button>
          ))}
        </div>
        <div className="fields">
          <div className="field">
            <label htmlFor="upd-channel">Channel</label>
            <select id="upd-channel" className="input" value={channel} disabled={!canEdit || policy === "Off"} onChange={(e) => setChannel(e.target.value as "stable" | "edge")}>
              <option value="stable">stable: releases</option>
              <option value="edge">edge: release candidates too</option>
            </select>
          </div>
          {policy === "AutoPatch" && (
            <label className="check">
              <input type="checkbox" checked={k8sPatches} disabled={!canEdit} onChange={(e) => setK8sPatches(e.target.checked)} />
              <span><b>Also Kubernetes patch versions</b><small>k3s patches tested with the running release, after Kwerft&apos;s. Minor versions of Kubernetes never install by themselves.</small></span>
            </label>
          )}
        </div>
        {(needsWindow || u.policy.window) && (
          <fieldset className="window" disabled={!canEdit}>
            <legend>Maintenance window{needsWindow ? "" : " (used by AutoPatch)"}</legend>
            <div className="field">
              <span className="label" id="upd-days">Days</span>
              <div className="seg" role="group" aria-labelledby="upd-days">
                {DAYS.map((d) => <button key={d} type="button" aria-pressed={days.includes(d)} onClick={() => toggleDay(d)}>{d}</button>)}
              </div>
              {fieldError("window.days") ? <span className="field-error" role="alert">{fieldError("window.days")}</span> : <span className="hint">{days.length === 0 || days.length === 7 ? "Every day" : `${days.length} ${days.length === 1 ? "day" : "days"} a week`}</span>}
            </div>
            <div className="fields3">
              <Field id="upd-start" label="Starts at" type="time" value={start} onChange={(e) => setStart(e.target.value)} error={fieldError("window.start")} />
              <div className="field">
                <label htmlFor="upd-duration">For</label>
                <select id="upd-duration" className="input" value={duration} onChange={(e) => setDuration(e.target.value)}>
                  {(durations.includes(duration) ? durations : [duration, ...durations]).map((d) => <option key={d} value={d}>{d}</option>)}
                </select>
                {fieldError("window.duration") && <span className="field-error" role="alert">{fieldError("window.duration")}</span>}
              </div>
              <Field id="upd-zone" label="Time zone" value={zone} onChange={(e) => setZone(e.target.value)} placeholder="UTC" spellCheck={false}
                error={fieldError("window.timeZone")} />
            </div>
            <p className="hint">
              An upgrade that has not started by the end of the window waits for the next one.
              {u.nextWindow && u.policy.window && <> Next window: {new Date(u.nextWindow).toLocaleString()} ({windowText(u.policy.window)}).</>}
              {u.windowOpen && " The window is open now."}
            </p>
          </fieldset>
        )}
        {canEdit && needsPassword && (
          <Field id="upd-password" label="Your password" type="password" value={password} autoComplete="current-password"
            onChange={(e) => setPassword(e.target.value)} error={fieldError("password")}
            hint={`${policy === u.policy.policy ? "Kubernetes patches then install" : "Patch releases then install"} without anyone clicking: confirm with your password or a current authenticator code.`} />
        )}
        {error && (!error.field || error.field === "window" || error.field === "policy" || error.field === "channel") && <p className="form-error" role="alert">{error.message}</p>}
        {canEdit && (
          <div className="actions start">
            <button type="submit" className="btn pri" disabled={busy || (needsPassword && !password)}>{busy ? "Saving…" : "Save policy"}</button>
            {saved && <span className="ok-text" role="status">Saved.</span>}
          </div>
        )}
      </div>
    </form>
  );
}

// ---- history ---------------------------------------------------------------------------------

function HistoryCard({ list, error, pending, selected, onSelect }: {
  list?: Upgrade[]; error: unknown; pending: boolean; selected?: Watch; onSelect: (w: Watch) => void;
}) {
  const { multi } = useClusters();
  return (
    <div className="card">
      <div className="ch-h"><h3>History</h3>{list && list.length > 0 && <span className="dim small">The newest 20 upgrades per cluster are kept</span>}</div>
      {pending ? <p className="loading pad">Loading…</p> : error ? <p className="form-error pad" role="alert">{errText(error)}</p> : !list?.length ? (
        <div className="li empty-li">No upgrades yet.</div>
      ) : (
        <div className="scroll-x">
          <table className="t history">
            <thead><tr><th>Upgrade</th>{multi && <th>Cluster</th>}<th>Requested</th><th className="num">Took</th><th>Result</th></tr></thead>
            <tbody>
              {list.map((u) => {
                const p = phasePill(u.phase);
                const sel = selected?.name === u.name && selected.cluster === u.cluster;
                return (
                  <tr key={`${u.cluster}/${u.name}`} className={sel ? "click sel" : "click"} onClick={() => onSelect({ cluster: u.cluster, name: u.name })}>
                    <td className="nm">
                      <button type="button" className="linkbtn" onClick={(e) => { e.stopPropagation(); onSelect({ cluster: u.cluster, name: u.name }); }}>
                        {u.component} {u.version}
                      </button>
                      <span className="sub">{u.from ? `from ${u.component === "Kwerft" ? u.from.kwerft ?? "?" : u.from.kubernetes ?? "?"}` : u.name}</span>
                    </td>
                    {multi && <td>{u.cluster}</td>}
                    <td><span title={new Date(u.createdAt).toLocaleString()}>{ago(u.createdAt)}</span><span className="sub">{u.auto ? "AutoPatch" : u.requestedBy}</span></td>
                    <td className="num">{u.startedAt && u.finishedAt ? durationText(new Date(u.finishedAt).getTime() - new Date(u.startedAt).getTime()) : "—"}</td>
                    <td><span className={`pill ${p.pill}`}>{p.label}</span>{u.finished && u.phase !== "Succeeded" && u.reason && <span className="sub">{u.reason}</span>}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}
