import { useState, type FormEvent, type InputHTMLAttributes } from "react";
import { useMutation } from "@tanstack/react-query";
import { ApiError } from "../api";
import { Icon } from "../components/Icon";
import { HOST_RE, sizes, workloads, type App, type AppSpec, type EnvVar, type Size } from "../workloads";

type HC = "none" | "http" | "tcp";
type Form = {
  image: string;
  pullSecret: string;
  pinned: string;
  replicas: string;
  size: Size;
  env: { name: string; value: string; from?: EnvVar["valueFrom"] }[];
  ports: { container: string; public: string; protocol: "TCP" | "UDP" }[];
  hc: HC;
  hcPath: string;
  hcPort: string;
  egress: "none" | "https" | "all";
};

function formOf(spec: AppSpec): Form {
  const hc = spec.healthCheck;
  return {
    image: spec.source.image?.ref ?? "",
    pullSecret: spec.source.image?.pullSecret ?? "",
    pinned: spec.source.git?.pinnedImage ?? "",
    replicas: String(spec.replicas ?? 1),
    size: spec.size ?? "small",
    env: (spec.env ?? []).map((e) => ({ name: e.name, value: e.value ?? "", from: e.valueFrom })),
    ports: (spec.ports ?? []).map((p) => ({ container: String(p.container), public: p.public ?? "", protocol: p.protocol ?? "TCP" })),
    hc: hc ? (hc.http ? "http" : "tcp") : "none",
    hcPath: hc?.http ?? "/healthz",
    hcPort: hc ? String(hc.port) : String(spec.ports?.[0]?.container ?? ""),
    egress: spec.egress ?? "https",
  };
}

// specOf applies the form to the current spec, keeping every field the form
// does not show (command, volumes, allowFrom, custom resources, ...).
function specOf(f: Form, base: AppSpec): AppSpec {
  const spec: AppSpec = structuredClone(base);
  if (spec.source.image) spec.source.image = { ref: f.image.trim(), ...(f.pullSecret.trim() ? { pullSecret: f.pullSecret.trim() } : {}) };
  if (spec.source.git) {
    spec.source.git = { ...spec.source.git };
    if (f.pinned) spec.source.git.pinnedImage = f.pinned;
    else delete spec.source.git.pinnedImage;
  }
  spec.replicas = Number(f.replicas);
  spec.size = f.size;
  spec.env = f.env.filter((e) => e.name.trim() || e.value).map((e) => (e.from ? { name: e.name.trim(), valueFrom: e.from } : { name: e.name.trim(), value: e.value }));
  spec.ports = f.ports.map((p) => ({ container: Number(p.container), protocol: p.protocol, ...(p.public.trim() ? { public: p.public.trim().toLowerCase() } : {}) }));
  if (f.hc === "none") delete spec.healthCheck;
  else spec.healthCheck = { port: Number(f.hcPort), ...(f.hc === "http" ? { http: f.hcPath.trim() || "/" } : {}) };
  spec.egress = f.egress;
  return spec;
}

/** Client-side checks, reported with the same field paths the server uses. */
function check(f: Form, isImage: boolean): { field: string; message: string } | undefined {
  if (isImage && !f.image.trim()) return { field: "spec.source.image.ref", message: "Enter an image, like ghcr.io/acme/api:1.4.2." };
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
      if (e instanceof ApiError) setError({ field: e.field, message: e.message, conflict: e.status === 409 });
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
                  <div className="field full"><label>Repository</label><code>{git?.repository}</code><span className="hint">Branch {git?.branch || "main"} · editing Git sources arrives with builds in Phase 2.</span></div>
                  {f.pinned && (
                    <div className="field full">
                      <label>Pinned image</label><code>{f.pinned}</code>
                      <span><button type="button" className="btn sm" onClick={() => set("pinned", "")}>Follow builds again</button></span>
                    </div>
                  )}
                  {!f.pinned && initial.pinned && <div className="field full"><span className="hint">Saving unpins the app; the next successful build deploys.</span></div>}
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
            <h3>Health check</h3>
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
  return /^spec\.(source\.image\.(ref|pullSecret)|replicas|env\[\d+\]|ports\[\d+\]|healthCheck\.(http|port))/.test(field);
}

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
