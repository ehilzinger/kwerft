import { useEffect, useState } from "react";
import { Link, getRouteApi, useNavigate } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../api";
import { AppStatus } from "../components/AppStatus";
import { Dialog } from "../components/Dialog";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { Replicas } from "../components/Replicas";
import { abilities, ago, sizes, workloads, type App, type Phase, type Revision } from "../workloads";
import { repoLabel, shortSha } from "../builds";
import { BuildStatus } from "../components/BuildStatus";
import { AppBuilds, buildName, buildsKey, useBuilds } from "./AppBuilds";
import { AppLogs } from "./AppLogs";
import { AppMetrics } from "./AppMetrics";
import { AppSettings } from "./AppSettings";
import { errorText } from "./Apps";
import { RunNowDialog } from "./RunNowDialog";
import "../styles/workloads.css";

const route = getRouteApi("/authed/apps/$project/$name");

const tabs = ["overview", "metrics", "logs", "builds", "settings"] as const;
type Tab = (typeof tabs)[number];

// phaseOf mirrors the server's summary (api_workloads.go appPhase) for the
// full App object.
export function phaseOf(app: App): { phase: Phase; reason?: string; message?: string } {
  const c = app.status?.conditions?.find((c) => c.type === "Ready");
  if (!c) return { phase: "pending", reason: "Pending", message: "Waiting for the controller to pick up this app." };
  if ((c.observedGeneration ?? 0) < app.metadata.generation) return { phase: "deploying", reason: "Updating", message: "Rolling out the latest change." };
  if (c.status === "True") return { phase: c.reason === "ScaledToZero" ? "stopped" : "running", reason: c.reason, message: c.message };
  if (c.reason === "Progressing") return { phase: "deploying", reason: c.reason, message: c.message };
  if (c.reason === "AwaitingBuild") return { phase: "pending", reason: c.reason, message: c.message };
  return { phase: "failed", reason: c.reason, message: c.message };
}

export function AppDetail() {
  const { project, name } = route.useParams();
  const { build, tab: tabParam } = route.useSearch();
  const linkedTab = tabs.find((t) => t === tabParam); // ?tab=logs from an alert
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  // In a Members project the role given there counts, not the console role.
  const projects = useQuery({ queryKey: ["projects"], queryFn: workloads.projects });
  const can = abilities(session.data, projects.data?.find((p) => p.name === project));
  const key = ["app", project, name];
  const q = useQuery({ queryKey: key, queryFn: () => workloads.app(project, name), refetchInterval: 5000 });

  const [tab, setTab] = useState<Tab>(build ? "builds" : linkedTab ?? "overview");
  // A link to a build (a commit check, a revision) opens the Builds tab; a
  // link with ?tab= (alert notifications: ?tab=logs) opens that tab.
  useEffect(() => {
    if (build) setTab("builds");
    else if (linkedTab) setTab(linkedTab);
  }, [build, linkedTab]);
  const showTab = (t: Tab) => {
    setTab(t);
    if ((t !== "builds" && build) || (tabParam && t !== tabParam)) void navigate({ to: "/apps/$project/$name", params: { project, name }, search: {}, replace: true });
  };
  const [logPod, setLogPod] = useState<string>(); // a replica's Logs button
  const [dialog, setDialog] = useState<"scale" | "delete" | "run" | { rollback: Revision }>();
  const [notice, setNotice] = useState<{ kind: "info" | "bad"; text: string }>();

  const refresh = (app?: App) => {
    if (app) queryClient.setQueryData(key, app);
    void queryClient.invalidateQueries({ queryKey: key });
    void queryClient.invalidateQueries({ queryKey: ["apps"] });
  };
  const restart = useMutation({
    mutationFn: () => workloads.restart(project, name),
    onSuccess: (app) => {
      refresh(app);
      setNotice({ kind: "info", text: "Restarting: replicas are replaced one at a time." });
    },
    onError: (e) => setNotice({ kind: "bad", text: errorText(e) }),
  });
  // Clears a rollback's pin: the newest successful build runs again.
  const unpin = useMutation({
    mutationFn: (a: App) => {
      const spec = structuredClone(a.spec);
      if (spec.source.git) delete spec.source.git.pinnedImage;
      return workloads.updateApp(project, name, spec, a.metadata.generation);
    },
    onSuccess: (app) => {
      refresh(app);
      void queryClient.invalidateQueries({ queryKey: buildsKey(project, name) });
      setNotice({ kind: "info", text: "Following builds again: the newest successful build rolls out." });
    },
    onError: (e) => setNotice({ kind: "bad", text: errorText(e) }),
  });

  if (q.isPending) return <section className="view"><p className="loading">Loading {name}…</p></section>;
  if (q.isError) {
    return (
      <section className="view">
        <div className="ph"><div><h1>{name}</h1><p className="sub">project {project}</p></div></div>
        <div className="empty">
          <h2>{q.error instanceof ApiError && q.error.status === 404 ? "App not found" : "Could not load this app"}</h2>
          <p>{errorText(q.error)}</p>
          <Link to="/apps" className="btn">Back to apps</Link>
        </div>
      </section>
    );
  }

  const app = q.data;
  const st = phaseOf(app);
  const git = app.spec.source.git;
  const workload = app.spec.volumes?.some((v) => v.size) ? "StatefulSet" : "Deployment"; // shared Volumes keep a Deployment
  const denied = can.deploy ? undefined : "Your role can view this app but not change it.";

  return (
    <section className="view">
      <div className="ph">
        <div>
          <h1>{app.metadata.name} <AppStatus {...st} /></h1>
          <p className="sub">
            {git ? <>Built from <code title={git.repository}>{repoLabel(git.repository)}</code> @ {git.branch || "main"}</> : <>Image <code>{app.spec.source.image?.ref}</code></>}
            {" · "}{workload} · project <Link to="/apps" className="dim">{project}</Link>
            {app.status?.revision ? ` · revision ${app.status.revision}` : ""}
          </p>
        </div>
        <div className="acts">
          <button className="btn" disabled={!can.deploy} title={denied} onClick={() => setDialog("run")}>
            <Icon name="play" />Run as job
          </button>
          <button className="btn" disabled={!can.deploy || restart.isPending} title={denied} onClick={() => restart.mutate()}>
            <Icon name="restart" />{restart.isPending ? "Restarting…" : "Restart"}
          </button>
          <button className="btn" disabled={!can.deploy} title={denied} onClick={() => setDialog("scale")}><Icon name="scale" />Scale</button>
          <button className="btn danger" disabled={!can.deploy} title={denied} onClick={() => setDialog("delete")}><Icon name="trash" />Delete</button>
        </div>
      </div>

      {notice && (
        <div className={`banner ${notice.kind}`} role={notice.kind === "bad" ? "alert" : "status"}>
          <Icon name={notice.kind === "bad" ? "alert" : "restart"} /><span>{notice.text}</span>
          <button className="btn sm" onClick={() => setNotice(undefined)}>Dismiss</button>
        </div>
      )}
      {st.phase === "failed" && (
        <div className="banner bad" role="alert"><Icon name="alert" /><span><b>{st.reason}</b>: {st.message}</span></div>
      )}
      {git?.pinnedImage && (
        <div className="banner warn" role="status">
          <Icon name="alert" /><span>Pinned to <code>{git.pinnedImage}</code> after a rollback. New builds are not deployed until the pin is cleared.</span>
          {can.deploy && <button className="btn sm" disabled={unpin.isPending} onClick={() => unpin.mutate(app)}>{unpin.isPending ? "Unpinning…" : "Follow builds again"}</button>}
        </div>
      )}

      <div className="tabs" role="tablist" aria-label="App">
        {tabs.map((t) => (
          <button key={t} role="tab" id={`tab-${t}`} aria-selected={tab === t} aria-controls={`panel-${t}`} onClick={() => showTab(t)}>
            {t[0]!.toUpperCase() + t.slice(1)}
          </button>
        ))}
      </div>

      <div className="tabpanel" role="tabpanel" id={`panel-${tab}`} aria-labelledby={`tab-${tab}`}>
        {tab === "overview" && <Overview app={app} canDeploy={can.deploy} onRollback={(rev) => setDialog({ rollback: rev })} />}
        {tab === "overview" && <Replicas app={app} onLogs={(pod) => { setLogPod(pod); showTab("logs"); }} />}
        {tab === "metrics" && <AppMetrics app={app} />}
        {tab === "logs" && <AppLogs app={app} pod={logPod} />}
        {tab === "builds" && <AppBuilds app={app} canDeploy={can.deploy} selected={build} />}
        {tab === "settings" && (
          <AppSettings app={app} canEdit={can.deploy} onSaved={(saved) => { refresh(saved); setNotice({ kind: "info", text: "Saved. Rolling out the new revision." }); showTab("overview"); }} />
        )}
      </div>

      {dialog === "run" && <RunNowDialog source={{ kind: "app", project, app: name }} onClose={() => setDialog(undefined)} />}
      {dialog === "scale" && <ScaleDialog app={app} onClose={() => setDialog(undefined)} onDone={refresh} />}
      {dialog === "delete" && (
        <DeleteDialog app={app} onClose={() => setDialog(undefined)}
          onDone={() => { void queryClient.invalidateQueries({ queryKey: ["apps"] }); void navigate({ to: "/apps" }); }} />
      )}
      {typeof dialog === "object" && (
        <RollbackDialog app={app} revision={dialog.rollback} onClose={() => setDialog(undefined)}
          onDone={(saved) => { refresh(saved); setNotice({ kind: "info", text: `Rolling back to the image of revision ${dialog.rollback.number}.` }); }} />
      )}
    </section>
  );
}

function Overview({ app, canDeploy, onRollback }: { app: App; canDeploy: boolean; onRollback: (r: Revision) => void }) {
  const git = app.spec.source.git;
  // Revisions name their build; the builds list has its number.
  const builds = useBuilds(app);
  const numbers = new Map((builds.data ?? []).map((b) => [b.name, b.number]));
  const latest = app.latestBuild;
  const project = app.metadata.namespace;
  const name = app.metadata.name;
  const desired = app.spec.replicas ?? 1;
  const ready = app.status?.readyReplicas ?? 0;
  const size = sizes.find((s) => s.id === (app.spec.size ?? "small"));
  const ports = app.spec.ports ?? [];
  const history = app.status?.history ?? [];
  const running = history[0]?.image;
  const hc = app.spec.healthCheck;
  const svc = `${app.metadata.name}.${app.metadata.namespace}.svc`;

  return (
    <div className="g2">
      <div className="card">
        <h3>Rollout</h3>
        <div className="stats">
          <div><span className="k">Replicas</span><span className="v">{ready}<small> / {desired}</small></span><span className="s">ready</span></div>
          <div><span className="k">Revision</span><span className="v">{app.status?.revision ?? "—"}</span><span className="s">{ago(history[0]?.time)}</span></div>
          <div><span className="k">Size</span><span className="v" style={{ fontSize: 16 }}>{size ? size.label : "Custom"}</span><span className="s">{size?.note ?? "requests and limits"}</span></div>
        </div>
        <div className="bd sep">
          <dl className="kv">
            <dt>Running</dt><dd><code>{app.status?.image || "—"}</code></dd>
            {git && (
              <>
                <dt>Latest build</dt>
                <dd>
                  {latest ? (
                    <>
                      <Link to="/apps/$project/$name" params={{ project, name }} search={{ build: latest.name }}>{buildName(latest)}</Link>{" "}
                      <BuildStatus build={latest} /> <code>{shortSha(latest.commit)}</code> {latest.message && <span className="dim">{latest.message}</span>}
                    </>
                  ) : <span className="dim">None yet. Push to {git.branch || "main"} or press Build now on the Builds tab.</span>}
                </dd>
              </>
            )}
            <dt>Health check</dt><dd>{hc ? (hc.http ? <code>HTTP GET {hc.http} :{hc.port}</code> : <code>TCP :{hc.port}</code>) : <span className="dim">None. A replica counts as ready once it starts.</span>}</dd>
            <dt>Environment</dt><dd>{app.spec.env?.length ? `${app.spec.env.length} variable${app.spec.env.length > 1 ? "s" : ""}` : <span className="dim">None</span>}</dd>
            <dt>Outbound</dt><dd>{{ none: "No internet access", https: "HTTPS to the internet", all: "Unrestricted" }[app.spec.egress ?? "https"]}</dd>
            {(app.spec.volumes?.length ?? 0) > 0 && <><dt>Volumes</dt><dd>{app.spec.volumes!.map((v) => (v.volume ? `${v.path} ← volume ${v.volume}${v.readOnly ? " (read-only)" : ""}` : `${v.path} (${v.size})`)).join(", ")}</dd></>}
          </dl>
        </div>
      </div>

      <div className="card">
        <h3>Reachable at</h3>
        <div className="list">
          {ports.filter((p) => p.public).map((p) => (
            <div className="li" key={`pub-${p.container}-${p.public}`}>
              <span className="ico ok"><Icon name="shield" /></span>
              <div><b><a href={`https://${p.public}`} target="_blank" rel="noreferrer">https://{p.public}</a></b><p>→ port {p.container} · certificate from Let's Encrypt</p></div>
            </div>
          ))}
          {ports.map((p) => (
            <div className="li" key={`int-${p.container}`}>
              <span className="ico info"><Icon name="net" /></span>
              <div><b>{svc}:{p.container}</b><p>Inside the cluster{app.spec.allowFrom?.length ? ` · allowed from ${app.spec.allowFrom.join(", ")}` : " · no other app allowed yet"}</p></div>
            </div>
          ))}
          {ports.length === 0 && <div className="li empty-li">No ports exposed. Add one in Settings to reach this app.</div>}
        </div>
        <h3 className="sep">Revisions</h3>
        <div className="list">
          {history.length === 0 && <div className="li empty-li">No revision rolled out yet.</div>}
          {history.map((r, i) => (
            <div className="li" key={r.number}>
              <div>
                <b>{r.number}</b> · <code title={r.image}>{r.commit ? shortSha(r.commit) : shortImage(r.image)}</code> {i === 0 && <span className="pill info nodot">current</span>}
                <p>
                  {r.build && <><Link to="/apps/$project/$name" params={{ project, name }} search={{ build: r.build }}>build {numbers.get(r.build) ? `#${numbers.get(r.build)}` : r.build}</Link>{" · "}</>}
                  {ago(r.time)}
                </p>
              </div>
              {i > 0 && r.image !== running && (
                <button className="btn sm end" disabled={!canDeploy} title={canDeploy ? `Run ${r.image} again` : "Your role cannot roll back."} onClick={() => onRollback(r)}>Roll back</button>
              )}
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}

function shortImage(image: string) {
  const at = image.lastIndexOf("/");
  return at >= 0 && image.length > 40 ? "…" + image.slice(at) : image;
}

function ScaleDialog({ app, onClose, onDone }: { app: App; onClose: () => void; onDone: (a: App) => void }) {
  const [value, setValue] = useState(String(app.spec.replicas ?? 1));
  const [error, setError] = useState<string>();
  const scale = useMutation({
    mutationFn: (n: number) => workloads.scale(app.metadata.namespace, app.metadata.name, n),
    onSuccess: (a) => { onDone(a); onClose(); },
    onError: (e) => setError(errorText(e)),
  });
  const stateful = !!app.spec.volumes?.some((v) => v.size);
  return (
    <Dialog title={`Scale ${app.metadata.name}`} onClose={onClose}
      onSubmit={() => {
        const n = Number(value);
        if (!Number.isInteger(n) || n < 0 || value.trim() === "") return setError("Enter a whole number, 0 or more.");
        scale.mutate(n);
      }}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={scale.isPending}>{scale.isPending ? "Scaling…" : "Scale"}</button>
      </>}>
      <Field id="scale-replicas" label="Replicas" type="number" min={0} step={1} value={value} onChange={(e) => setValue(e.target.value)} onFocus={(e) => e.target.select()} autoFocus error={error}
        hint={stateful ? "Each replica of a stateful app gets its own volume." : "0 stops the app without deleting it. Replicas spread across nodes when possible."} />
    </Dialog>
  );
}

function DeleteDialog({ app, onClose, onDone }: { app: App; onClose: () => void; onDone: () => void }) {
  const [confirm, setConfirm] = useState("");
  const del = useMutation({ mutationFn: () => workloads.deleteApp(app.metadata.namespace, app.metadata.name), onSuccess: onDone });
  const name = app.metadata.name;
  return (
    <Dialog title={`Delete ${name}?`} onClose={onClose} onSubmit={() => confirm === name && del.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri danger" disabled={confirm !== name || del.isPending}>{del.isPending ? "Deleting…" : "Delete app"}</button>
      </>}>
      <p style={{ margin: 0 }}>This stops every replica and removes the app's service, routes and network policy.{(app.spec.volumes?.length ?? 0) > 0 ? " Its volumes are kept." : ""}</p>
      <Field id="delete-confirm" label={`Type ${name} to confirm`} className="mono" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoFocus autoComplete="off"
        error={del.isError ? errorText(del.error) : undefined} />
    </Dialog>
  );
}

function RollbackDialog({ app, revision, onClose, onDone }: { app: App; revision: Revision; onClose: () => void; onDone: (a: App) => void }) {
  const rb = useMutation({
    mutationFn: () => workloads.rollback(app.metadata.namespace, app.metadata.name, revision.number),
    onSuccess: (a) => { onDone(a); onClose(); },
  });
  return (
    <Dialog title={`Roll back to revision ${revision.number}?`} onClose={onClose} onSubmit={() => rb.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={rb.isPending}>{rb.isPending ? "Rolling back…" : "Roll back"}</button>
      </>}>
      <p style={{ margin: 0 }}>
        Runs <code>{revision.image}</code> again as a new revision. Settings such as environment, replicas and ports stay as they are now.
      </p>
      {app.spec.source.git && <p className="dim" style={{ margin: 0 }}>The app stays pinned to this image, and new builds are not deployed, until you clear the pin.</p>}
      {rb.isError && <p className="form-error" role="alert">{errorText(rb.error)}</p>}
    </Dialog>
  );
}
