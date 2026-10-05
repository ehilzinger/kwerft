import { useState, type FormEvent, type InputHTMLAttributes } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { ApiError } from "../api";
import { branchProblem, gitApi, normalizeRepository, repoPathProblem, repositoryProblem } from "../builds";
import { GitFields, gitSourceOf } from "./Deploy";
import { Icon } from "../components/Icon";
import { VolumeMounts, checkMounts, mountsOf, volumesOf, type Mount } from "../components/VolumeMounts";
import { ownDisks as disksOf } from "../mounts";
import { HOST_RE, sizes, workloads, type App, type AppSpec, type EnvVar, type Size } from "../workloads";

type HC = "none" | "http" | "tcp";
type Form = {
  image: string;
  pullSecret: string;
  pinned: string;
  // Git apps
  repo: string;
  branch: string;
  connection: string;
  builder: "dockerfile" | "railpack";
  dockerfile: string;
  path: string;
  autoDeploy: boolean;
  replicas: string;
  size: Size;
  env: { name: string; value: string; from?: EnvVar["valueFrom"] }[];
  ports: { container: string; public: string; protocol: "TCP" | "UDP" }[];
  hc: HC;
  hcPath: string;
  hcPort: string;
  drain: string;
  egress: "none" | "https" | "all";
  mounts: Mount[]; // shared Volumes and Secrets; disks per replica are kept as they are
};

function formOf(spec: AppSpec): Form {
  const hc = spec.healthCheck;
  return {
    image: spec.source.image?.ref ?? "",
    pullSecret: spec.source.image?.pullSecret ?? "",
    pinned: spec.source.git?.pinnedImage ?? "",
    repo: spec.source.git?.repository ?? "",
    branch: spec.source.git?.branch ?? "main",
    connection: spec.source.git?.connection ?? "",
    builder: spec.source.git?.builder === "railpack" ? "railpack" : "dockerfile",
    dockerfile: spec.source.git?.dockerfile ?? "Dockerfile",
    path: spec.source.git?.path ?? "/",
    autoDeploy: spec.source.git?.autoDeploy ?? true,
    replicas: String(spec.replicas ?? 1),
    size: spec.size ?? "small",
    env: (spec.env ?? []).map((e) => ({ name: e.name, value: e.value ?? "", from: e.valueFrom })),
    ports: (spec.ports ?? []).map((p) => ({ container: String(p.container), public: p.public ?? "", protocol: p.protocol ?? "TCP" })),
    hc: hc ? (hc.http ? "http" : "tcp") : "none",
    hcPath: hc?.http ?? "/healthz",
    hcPort: hc ? String(hc.port) : String(spec.ports?.[0]?.container ?? ""),
    drain: String(spec.drainSeconds ?? 5),
    egress: spec.egress ?? "https",
    mounts: mountsOf(spec.volumes),
  };
}

// specOf applies the form to the current spec, keeping every field the form
// does not show (command, volumes, allowFrom, custom resources, ...).
function specOf(f: Form, base: AppSpec): AppSpec {
  const spec: AppSpec = structuredClone(base);
  if (spec.source.image) spec.source.image = { ref: f.image.trim(), ...(f.pullSecret.trim() ? { pullSecret: f.pullSecret.trim() } : {}) };
  if (spec.source.git) {
    spec.source.git = gitSourceOf(f);
    if (f.pinned) spec.source.git.pinnedImage = f.pinned;
  }
  spec.replicas = Number(f.replicas);
  spec.size = f.size;
  spec.env = f.env.filter((e) => e.name.trim() || e.value).map((e) => (e.from ? { name: e.name.trim(), valueFrom: e.from } : { name: e.name.trim(), value: e.value }));
  spec.ports = f.ports.map((p) => ({ container: Number(p.container), protocol: p.protocol, ...(p.public.trim() ? { public: p.public.trim().toLowerCase() } : {}) }));
  if (f.hc === "none") delete spec.healthCheck;
  else spec.healthCheck = { port: Number(f.hcPort), ...(f.hc === "http" ? { http: f.hcPath.trim() || "/" } : {}) };
  spec.egress = f.egress;
  spec.drainSeconds = Number(f.drain);
  spec.volumes = [...ownDisks(base), ...volumesOf(f.mounts)];
  if (spec.volumes.length === 0) delete spec.volumes;
  return spec;
}

const ownDisks = (spec: AppSpec) => disksOf(spec.volumes);

/** Client-side checks, reported with the same field paths the server uses. */
function check(f: Form, isImage: boolean): { field: string; message: string } | undefined {
  if (isImage && !f.image.trim()) return { field: "spec.source.image.ref", message: "Enter an image, like ghcr.io/acme/api:1.4.2." };
  if (!isImage) {
    const repo = repositoryProblem(normalizeRepository(f.repo));
    if (repo) return { field: "spec.source.git.repository", message: repo };
    const branch = branchProblem(f.branch.trim());
    if (branch) return { field: "spec.source.git.branch", message: branch };
    const df = f.builder === "dockerfile" ? repoPathProblem(f.dockerfile.trim()) : "";
    if (df) return { field: "spec.source.git.dockerfile", message: df };
    const path = repoPathProblem(f.path.trim());
    if (path) return { field: "spec.source.git.path", message: path };
  }
  const n = Number(f.replicas);
  if (f.replicas.trim() === "" || !Number.isInteger(n) || n < 0) return { field: "spec.replicas", message: "Enter a whole number, 0 or more." };
  for (const [i, e] of f.env.entries()) {
    if (!e.name.trim() && e.value) return { field: `spec.env[${i}].name`, message: "Give this variable a name." };
  }
  for (const [i, p] of f.ports.entries()) {
    const c = Number(p.container);
    if (!Number.isInteger(c) || c < 1 || c > 65535) return { field: `spec.ports[${i}].container`, message: "A port is a number from 1 to 65535." };
    if (p.public.trim() && !HOST_RE.test(p.public.trim().toLowerCase())) return { field: `spec.ports[${i}].public`, message: "Enter a hostname, like app.example.com." };
  }
  if (f.hc !== "none") {
    const c = Number(f.hcPort);
    if (!Number.isInteger(c) || c < 1 || c > 65535) return { field: "spec.healthCheck.port", message: "A port is a number from 1 to 65535." };
  }
  const d = Number(f.drain);
  if (f.drain.trim() === "" || !Number.isInteger(d) || d < 0 || d > 300) return { field: "spec.drainSeconds", message: "Enter whole seconds, from 0 to 300." };
  const m = checkMounts(f.mounts);
  if (m) return { field: `mounts[${m[0]}]`, message: m[1] };
  return undefined;
}

export function AppSettings({ app, canEdit, onSaved }: { app: App; canEdit: boolean; onSaved: (a: App) => void }) {
  // The form starts from the spec as loaded and is not reset by polling; the
  // generation sent on save makes the server refuse if someone else changed
  // the settings since.
  const [base, setBase] = useState(app);
  const [initial, setInitial] = useState(() => formOf(app.spec));
  const [f, setF] = useState(initial);
  const [error, setError] = useState<{ field?: string; message: string; conflict?: boolean }>();
  const set = <K extends keyof Form>(k: K, v: Form[K]) => setF((prev) => ({ ...prev, [k]: v }));
  const isImage = !!base.spec.source.image;
  const git = base.spec.source.git;
  const dirty = JSON.stringify(f) !== JSON.stringify(initial);
  const stale = app.metadata.generation > base.metadata.generation;
  const conns = useQuery({ queryKey: ["git-connections"], queryFn: gitApi.connections, enabled: !!git, retry: false, staleTime: 30_000 });
  const project = base.metadata.namespace;
  const usable = (conns.data ?? []).filter((c) => c.projects.length === 0 || c.projects.includes(project));

  const save = useMutation({
    mutationFn: () => workloads.updateApp(base.metadata.namespace, base.metadata.name, specOf(f, base.spec), base.metadata.generation),
    onSuccess: (saved) => {
      setBase(saved);
      const next = formOf(saved.spec);
      setInitial(next);
      setF(next);
      setError(undefined);
      onSaved(saved);
    },
    onError: (e) => {
      // spec.volumes[i] counts the disks per replica first; the form lists shared mounts only.
      const vol = /^spec\.volumes\[(\d+)\]/.exec(e instanceof ApiError ? e.field ?? "" : "");
      const field = vol ? `mounts[${Number(vol[1]) - ownDisks(base.spec).length}]` : e instanceof ApiError ? e.field : undefined;
      if (e instanceof ApiError) setError({ field, message: e.message, conflict: e.status === 409 });
      else setError({ message: "The console could not be reached. Check your connection and try again." });
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    const problem = check(f, isImage);
    if (problem) return setError(problem);
    setError(undefined);
    save.mutate();
  }

  function reload() {
    setBase(app);
    const next = formOf(app.spec);
    setInitial(next);
    setF(next);
    setError(undefined);
  }

  const err = (field: string) => (error?.field === field ? error.message : undefined);
  const errAt = (prefix: string) => (error?.field?.startsWith(prefix) ? error.message : undefined);
  const mountErr = /^mounts\[(\d+)\]$/.exec(error?.field ?? "");
  const nextRevision = (app.status?.revision ?? 0) + 1;
  const ro = !canEdit;

  return (
    <form className="tabpanel" onSubmit={submit} noValidate>
      {ro && <div className="banner info"><Icon name="shield" /><span>Your role can view these settings but not change them.</span></div>}
      {stale && !save.isPending && (
        <div className="banner warn">
          <Icon name="alert" /><span>These settings changed since you opened them (now generation {app.metadata.generation}).</span>
          <button type="button" className="btn sm" onClick={reload}>Load the latest</button>
        </div>
      )}
      <fieldset disabled={ro} style={{ border: 0, padding: 0, margin: 0, display: "grid", gap: 16, minWidth: 0 }}>
        <div className="g2e">
          <div className="card">
            <h3>Source</h3>
            <div className="bd fields">
              {isImage ? (
                <>
                  <Input id="s-image" label="Image" className="mono full" value={f.image} onChange={(v) => set("image", v)} error={err("spec.source.image.ref")}
                    hint="Changing the tag rolls out a new revision." />
                  <Input id="s-secret" label="Registry credential" className="mono full" value={f.pullSecret} onChange={(v) => set("pullSecret", v)} placeholder="None (public image)"
                    error={err("spec.source.image.pullSecret")} hint="Name of a docker-registry Secret in this project." />
                </>
              ) : (
                <>
                  <GitFields f={f} set={(k, v) => set(k, v as never)} err={(k) => err(gitField(k))} conns={usable} connsError={conns.error} project={project}
                    onConnection={(v) => set("connection", v)} idPrefix="s" />
                  {f.pinned && (
                    <div className="field full">
                      <label>Pinned image</label><code>{f.pinned}</code>
                      <span className="hint">A rollback pinned this image: builds still run, but none is deployed.</span>
                      <span><button type="button" className="btn sm" onClick={() => set("pinned", "")}>Follow builds again</button></span>
                    </div>
                  )}
                  {!f.pinned && initial.pinned && <div className="field full"><span className="hint">Saving unpins the app; the newest successful build deploys.</span></div>}
                </>
              )}
            </div>
          </div>
          <div className="card">
            <h3>Scaling &amp; resources</h3>
            <div className="bd fields">
              <Input id="s-replicas" label="Replicas" type="number" min={0} value={f.replicas} onChange={(v) => set("replicas", v)} error={err("spec.replicas")} />
              <div className="field">
                <label htmlFor="s-size">Size</label>
                <select id="s-size" className="input" value={f.size} onChange={(e) => set("size", e.target.value as Size)}>
                  {sizes.map((s) => <option key={s.id} value={s.id}>{s.label} · {s.note}</option>)}
                  {f.size === "custom" && <option value="custom">Custom requests and limits</option>}
                </select>
              </div>
              <div className="field full">
                <label>Outbound access</label>
                <div className="seg" role="group" aria-label="Outbound access">
                  {(["none", "https", "all"] as const).map((v) => (
                    <button type="button" key={v} aria-pressed={f.egress === v} onClick={() => set("egress", v)}>
                      {{ none: "None", https: "HTTPS to internet", all: "Unrestricted" }[v]}
                    </button>
                  ))}
                </div>
              </div>
            </div>
          </div>
        </div>

        <div className="card">
          <div className="ch-h">
            <h3>Environment</h3>
            <button type="button" className="btn sm" onClick={() => set("env", [...f.env, { name: "", value: "" }])}><Icon name="plus" />Add variable</button>
          </div>
          <div className="scroll-x">
            <table className="t">
              <thead><tr><th style={{ width: "34%" }}>Name</th><th>Value</th><th style={{ width: 1 }}></th></tr></thead>
              <tbody>
                {f.env.length === 0 && <tr><td colSpan={3} className="dim">No variables.</td></tr>}
                {f.env.map((e, i) => (
                  <tr key={i}>
                    <td>
                      <input className="input mono" aria-label={`Variable ${i + 1} name`} value={e.name} placeholder="NAME"
                        aria-invalid={!!errAt(`spec.env[${i}]`)} onChange={(ev) => set("env", f.env.map((x, j) => (j === i ? { ...x, name: ev.target.value } : x)))} />
                      {errAt(`spec.env[${i}]`) && <span className="field-error" role="alert">{errAt(`spec.env[${i}]`)}</span>}
                    </td>
                    <td>
                      {e.from ? (
                        <span className="tag">{e.from.secretKeyRef ? `secret · ${e.from.secretKeyRef.name}/${e.from.secretKeyRef.key}` : e.from.configMapKeyRef ? `config · ${e.from.configMapKeyRef.name}/${e.from.configMapKeyRef.key}` : `field · ${e.from.fieldRef?.fieldPath}`}</span>
                      ) : (
                        <input className="input mono" aria-label={`Variable ${i + 1} value`} value={e.value}
                          onChange={(ev) => set("env", f.env.map((x, j) => (j === i ? { ...x, value: ev.target.value } : x)))} />
                      )}
                    </td>
                    <td><button type="button" className="btn ghost sm danger" onClick={() => set("env", f.env.filter((_, j) => j !== i))} aria-label={`Remove ${e.name || "variable"}`}>Remove</button></td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>

        <div className="g2e">
          <div className="card">
            <div className="ch-h">
              <h3>Ports</h3>
              <button type="button" className="btn sm" onClick={() => set("ports", [...f.ports, { container: "", public: "", protocol: "TCP" }])}><Icon name="plus" />Add port</button>
            </div>
            <div className="scroll-x">
              <table className="t">
                <thead><tr><th style={{ width: 110 }}>Container</th><th>Public hostname</th><th style={{ width: 1 }}></th></tr></thead>
                <tbody>
                  {f.ports.length === 0 && <tr><td colSpan={3} className="dim">No ports: the app is not reachable.</td></tr>}
                  {f.ports.map((p, i) => (
                    <tr key={i}>
                      <td>
                        <input className="input mono" inputMode="numeric" aria-label={`Port ${i + 1}`} value={p.container} placeholder="8080"
                          aria-invalid={!!err(`spec.ports[${i}].container`)} onChange={(ev) => set("ports", f.ports.map((x, j) => (j === i ? { ...x, container: ev.target.value } : x)))} />
                      </td>
                      <td>
                        <input className="input mono" aria-label={`Port ${i + 1} public hostname`} value={p.public} placeholder="Cluster only"
                          aria-invalid={!!err(`spec.ports[${i}].public`)} onChange={(ev) => set("ports", f.ports.map((x, j) => (j === i ? { ...x, public: ev.target.value } : x)))} />
                        {errAt(`spec.ports[${i}]`) && <span className="field-error" role="alert">{errAt(`spec.ports[${i}]`)}</span>}
                      </td>
                      <td><button type="button" className="btn ghost sm danger" onClick={() => set("ports", f.ports.filter((_, j) => j !== i))}>Remove</button></td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
          <div className="card">
            <h3>Health &amp; draining</h3>
            <div className="bd fields">
              <div className="field full">
                <label>Check</label>
                <div className="seg" role="group" aria-label="Health check">
                  {(["none", "http", "tcp"] as const).map((v) => (
                    <button type="button" key={v} aria-pressed={f.hc === v} onClick={() => set("hc", v)}>{{ none: "None", http: "HTTP GET", tcp: "TCP connect" }[v]}</button>
                  ))}
                </div>
              </div>
              {f.hc === "http" && <Input id="s-hc-path" label="Path" className="mono" value={f.hcPath} onChange={(v) => set("hcPath", v)} error={err("spec.healthCheck.http")} />}
              {f.hc !== "none" && <Input id="s-hc-port" label="Port" className="mono" value={f.hcPort} onChange={(v) => set("hcPort", v)} error={err("spec.healthCheck.port")} />}
              {f.hc === "none" && <span className="hint full">Without a check, a replica gets traffic as soon as it starts, and a hung one is never replaced.</span>}
              {f.ports.length > 0 && <Input id="s-drain" label="Drain (seconds)" className="mono" inputMode="numeric" value={f.drain} onChange={(v) => set("drain", v)} error={err("spec.drainSeconds")}
                hint="A replica being replaced keeps answering this long before it is told to stop, while traffic moves away. 0 stops it at once." />}
            </div>
          </div>
        </div>

        <div className="card">
          <h3>Volumes</h3>
          <div className="bd fields">
            {ownDisks(base.spec).length > 0 && (
              <div className="field full">
                <label>Disks per replica</label>
                <span>{ownDisks(base.spec).map((v) => `${v.path} (${v.size}${v.class ? `, ${v.class}` : ""})`).join(", ")}</span>
                <span className="hint">Each replica keeps its own disk, so the app runs as a StatefulSet. These cannot be changed here.</span>
              </div>
            )}
            <div className="field full">
              <label>Shared volumes and secrets</label>
              <VolumeMounts project={base.metadata.namespace} mounts={f.mounts} onChange={(v) => set("mounts", v)} idPrefix="s-vol"
                errorAt={mountErr ? [Number(mountErr[1]), error!.message] : undefined} />
              <span className="hint">Volumes of the project that other apps and jobs mount too; the pods run on the volume's node. A secret is mounted read-only, one file per key; until it exists, new replicas wait.</span>
            </div>
          </div>
        </div>
      </fieldset>

      {error && !error.field && (
        <div className="banner bad" role="alert">
          <Icon name="alert" /><span>{error.message}</span>
          {error.conflict && <button type="button" className="btn sm" onClick={reload}>Load the latest</button>}
        </div>
      )}
      {error?.field && !knownField(error.field) && <div className="banner bad" role="alert"><Icon name="alert" /><span><code>{error.field}</code>: {error.message}</span></div>}

      {canEdit && (
        <div className="banner info">
          <Icon name="rocket" />
          <span>Saving creates <b>revision {nextRevision}</b> and rolls out one replica at a time{f.hc !== "none" ? <>; the health check must pass before traffic moves</> : null}.</span>
          <button className="btn pri sm" disabled={!dirty || save.isPending}>{save.isPending ? "Saving…" : "Save & deploy"}</button>
        </div>
      )}
    </form>
  );
}

function knownField(field: string) {
  return /^(spec\.(source\.image\.(ref|pullSecret)|source\.git\.(repository|branch|connection|dockerfile|path)|replicas|env\[\d+\]|ports\[\d+\]|healthCheck\.(http|port)|drainSeconds)|mounts\[\d+\])/.test(field);
}

/** GitFields' field names → the server's field paths. */
const gitField = (k: string) => `spec.source.git.${k === "repo" ? "repository" : k}`;

type InputProps = { id: string; label: string; value: string; onChange: (v: string) => void; error?: string; hint?: string; className?: string } & Omit<InputHTMLAttributes<HTMLInputElement>, "onChange" | "value" | "className">;

// A compact labelled input for the settings cards (Field, with "full" spanning both columns).
function Input({ id, label, value, onChange, error, hint, className = "", ...rest }: InputProps) {
  const full = className.includes("full");
  const mono = className.includes("mono");
  return (
    <div className={`field ${full ? "full" : ""}`}>
      <label htmlFor={id}>{label}</label>
      <input id={id} className={`input ${mono ? "mono" : ""}`} value={value} onChange={(e) => onChange(e.target.value)} aria-invalid={!!error}
        aria-describedby={error ? `${id}-error` : hint ? `${id}-hint` : undefined} {...rest} />
      {error ? <span id={`${id}-error`} className="field-error" role="alert">{error}</span> : hint ? <span id={`${id}-hint`} className="hint">{hint}</span> : null}
    </div>
  );
}
