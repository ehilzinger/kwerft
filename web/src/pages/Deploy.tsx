// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { useState, type FormEvent } from "react";
import { Link, getRouteApi, useNavigate } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../api";
import { CreateProjectDialog } from "../components/CreateProjectDialog";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { VolumeMounts, checkMounts, volumesOf, type Mount } from "../components/VolumeMounts";
import { HOST_RE, NAME_RE, abilities, sizes, toYAML, workloads, type AppSpec, type GitSource, type Size } from "../workloads";
import { LOCAL, useClusters } from "../clusters";
import { appEnvSet, secretsApi } from "../secrets";
import { settingsApi, underWildcard } from "../settings";
import {
  branchProblem, buildsApi, gitApi, normalizeRepository, notOffered, repoPathProblem, repositoryProblem, shortSha, suggestConnection,
  type CheckQuery, type CheckResult, type Connection,
} from "../builds";
import { Toggle } from "../components/LogViewer";
import { ImportFlow } from "./DeployImport";
import "../styles/workloads.css";
import "../styles/builds.css";

const route = getRouteApi("/authed/apps/new");

type Step = "source" | "runtime" | "network" | "review";
const steps: { id: Step; title: string }[] = [
  { id: "source", title: "Source" },
  { id: "runtime", title: "Runtime" },
  { id: "network", title: "Networking" },
  { id: "review", title: "Review" },
];

type Form = {
  name: string;
  project: string;
  /** compose and template are their own flows (DeployImport.tsx). */
  source: "image" | "git" | "compose" | "template";
  image: string;
  pullSecret: string;
  repo: string;
  branch: string;
  connection: string; // "" = public repository
  builder: "dockerfile" | "railpack";
  dockerfile: string;
  path: string;
  autoDeploy: boolean;
  size: Exclude<Size, "custom">;
  replicas: string;
  hc: "none" | "http" | "tcp";
  hcPath: string;
  hcPort: string;
  env: string;
  /** KEY=value lines stored write-only in the App's own secret set. */
  secretEnv: string;
  port: string;
  exposure: "cluster" | "public";
  domain: string;
  allowFrom: string;
  egress: "none" | "https" | "all";
  mounts: Mount[];
};

type Problem = { step: Step; field: string; message: string };

function parseEnv(text: string): { vars: { name: string; value: string }[]; bad?: number } {
  const vars: { name: string; value: string }[] = [];
  const lines = text.split("\n");
  for (const [i, raw] of lines.entries()) {
    const line = raw.trim();
    if (!line || line.startsWith("#")) continue;
    const eq = line.indexOf("=");
    if (eq <= 0) return { vars, bad: i + 1 };
    vars.push({ name: line.slice(0, eq).trim(), value: line.slice(eq + 1) });
  }
  return { vars };
}

export function gitSourceOf(f: Pick<Form, "repo" | "branch" | "connection" | "builder" | "dockerfile" | "path" | "autoDeploy">): GitSource {
  return {
    repository: normalizeRepository(f.repo),
    branch: f.branch.trim() || "main",
    path: f.path.trim() || "/",
    builder: f.builder,
    ...(f.builder === "dockerfile" ? { dockerfile: f.dockerfile.trim() || "Dockerfile" } : {}),
    ...(f.connection ? { connection: f.connection } : {}),
    autoDeploy: f.autoDeploy,
  };
}

function specOf(f: Form): AppSpec {
  const port = Number(f.port);
  const spec: AppSpec = {
    source: f.source === "git"
      ? { git: gitSourceOf(f) }
      : { image: { ref: f.image.trim(), ...(f.pullSecret.trim() ? { pullSecret: f.pullSecret.trim() } : {}) } },
    replicas: Number(f.replicas),
    size: f.size,
    env: [
      ...parseEnv(f.env).vars,
      ...parseEnv(f.secretEnv).vars.map((v) => ({ name: v.name, valueFrom: { secretKeyRef: { name: appEnvSet(f.name), key: v.name } } })),
    ],
    ports: f.port.trim() ? [{ container: port, ...(f.exposure === "public" && f.domain.trim() ? { public: f.domain.trim().toLowerCase() } : {}) }] : [],
    allowFrom: f.allowFrom.split(/[\s,]+/).filter(Boolean),
    egress: f.egress,
    ...(f.mounts.length ? { volumes: volumesOf(f.mounts) } : {}),
  };
  if (f.hc !== "none") spec.healthCheck = { port: Number(f.hcPort || f.port), ...(f.hc === "http" ? { http: f.hcPath.trim() || "/" } : {}) };
  return spec;
}

function check(f: Form): Problem | undefined {
  if (!NAME_RE.test(f.name) || f.name.length > 63) return { step: "source", field: "name", message: "Use lowercase letters, digits and dashes, starting with a letter." };
  if (!f.project) return { step: "source", field: "project", message: "Choose a project." };
  if (f.source === "image" && !f.image.trim()) return { step: "source", field: "image", message: "Enter an image, like ghcr.io/acme/api:1.4.2." };
  if (f.source === "git") {
    const repo = repositoryProblem(normalizeRepository(f.repo));
    if (repo) return { step: "source", field: "repo", message: repo };
    const branch = branchProblem(f.branch.trim());
    if (branch) return { step: "source", field: "branch", message: branch };
    if (f.builder === "dockerfile") {
      const df = repoPathProblem(f.dockerfile.trim()) || (f.dockerfile.trim().endsWith("/") ? "Name the Dockerfile itself, like Dockerfile." : "");
      if (df) return { step: "source", field: "dockerfile", message: df };
    }
    const path = repoPathProblem(f.path.trim());
    if (path) return { step: "source", field: "path", message: path };
  }
  const n = Number(f.replicas);
  if (f.replicas.trim() === "" || !Number.isInteger(n) || n < 0) return { step: "runtime", field: "replicas", message: "Enter a whole number, 0 or more." };
  const env = parseEnv(f.env);
  if (env.bad) return { step: "runtime", field: "env", message: `Line ${env.bad} is not KEY=value.` };
  const badName = env.vars.findIndex((v) => !/^[-._a-zA-Z][-._a-zA-Z0-9]*$/.test(v.name));
  if (badName >= 0) return { step: "runtime", field: "env", message: `"${env.vars[badName]!.name}" is not a valid variable name.` };
  const secret = parseEnv(f.secretEnv);
  if (secret.bad) return { step: "runtime", field: "secretEnv", message: `Line ${secret.bad} is not KEY=value.` };
  const badSecret = secret.vars.find((v) => !/^[-._a-zA-Z][-._a-zA-Z0-9]*$/.test(v.name) || !v.value || env.vars.some((e) => e.name === v.name));
  if (badSecret) return { step: "runtime", field: "secretEnv", message: !badSecret.value ? `${badSecret.name} has no value.` : env.vars.some((e) => e.name === badSecret.name) ? `${badSecret.name} is also a plain variable.` : `"${badSecret.name}" is not a valid variable name.` };
  const validPort = (s: string) => Number.isInteger(Number(s)) && Number(s) >= 1 && Number(s) <= 65535;
  if (f.hc !== "none" && f.hcPort.trim() && !validPort(f.hcPort)) return { step: "runtime", field: "hcPort", message: "A port is a number from 1 to 65535." };
  const mount = checkMounts(f.mounts);
  if (mount) return { step: "runtime", field: `mounts[${mount[0]}]`, message: mount[1] };
  if (f.port.trim() && !validPort(f.port)) return { step: "network", field: "port", message: "A port is a number from 1 to 65535." };
  if (f.hc !== "none" && !f.hcPort.trim() && !f.port.trim()) return { step: "network", field: "port", message: "The health check connects to this port. Enter it, or set a health check port." };
  if (f.exposure === "public") {
    if (!f.port.trim()) return { step: "network", field: "port", message: "A public domain needs the container port it forwards to." };
    if (!HOST_RE.test(f.domain.trim().toLowerCase())) return { step: "network", field: "domain", message: "Enter a hostname, like app.example.com." };
  }
  return undefined;
}

// Server field paths → wizard step and field.
function locate(field: string | undefined): { step: Step; field: string } | undefined {
  if (!field) return undefined;
  if (field === "name") return { step: "source", field: "name" };
  const git = /^spec\.source\.git\.(repository|branch|connection|dockerfile|path)$/.exec(field);
  if (git) return { step: "source", field: git[1] === "repository" ? "repo" : git[1]! };
  if (field.startsWith("spec.source.git")) return { step: "source", field: "repo" };
  if (field.startsWith("spec.source")) return { step: "source", field: "image" };
  if (field === "spec.replicas") return { step: "runtime", field: "replicas" };
  if (field.startsWith("spec.env")) return { step: "runtime", field: "env" };
  if (field.startsWith("spec.healthCheck")) return { step: "runtime", field: "hcPort" };
  const vol = /^spec\.volumes\[(\d+)\]/.exec(field);
  if (vol) return { step: "runtime", field: `mounts[${vol[1]}]` };
  if (field.endsWith(".public")) return { step: "network", field: "domain" };
  if (field.startsWith("spec.ports")) return { step: "network", field: "port" };
  return undefined;
}

export function Deploy() {
  const search = route.useSearch();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const projects = useQuery({ queryKey: ["projects"], queryFn: workloads.projects });
  const can = abilities(session.data, undefined, projects.data);

  const [step, setStep] = useState<Step>("source");
  const [visited, setVisited] = useState<Set<Step>>(new Set(["source"]));
  const [creatingProject, setCreatingProject] = useState(false);
  const [problem, setProblem] = useState<{ field?: string; message: string }>();
  const [copied, setCopied] = useState(false);
  const [f, setF] = useState<Form>({
    name: "", project: search.project ?? "", source: "image", image: "", pullSecret: "",
    repo: "", branch: "", connection: "", builder: "dockerfile", dockerfile: "Dockerfile", path: "/", autoDeploy: true,
    size: "small", replicas: "1",
    hc: "none", hcPath: "/healthz", hcPort: "", env: "", secretEnv: "", port: "", exposure: "cluster", domain: "", allowFrom: "", egress: "https", mounts: [],
  });
  const set = <K extends keyof Form>(k: K, v: Form[K]) => {
    setF((prev) => ({ ...prev, [k]: v }));
    setProblem((p) => (p?.field === k ? undefined : p)); // the user is fixing it
  };
  const project = f.project || (projects.data?.length === 1 ? projects.data[0]!.name : "");
  const { multi, unreachable } = useClusters();
  const chosenProject = projects.data?.find((p) => p.name === project);
  // Until a hostname is typed, suggest <app>.<apps domain> (Settings).
  const settings = useQuery({ queryKey: ["settings"], queryFn: () => settingsApi.get(), staleTime: 60_000 });
  // A project in a remote cluster: that cluster's apps domain, else the
  // console's, whose records the console keeps for it per hostname.
  const projectCluster = chosenProject?.cluster ?? LOCAL;
  const clusterSettings = useQuery({
    queryKey: ["settings", projectCluster], queryFn: () => settingsApi.get(projectCluster),
    enabled: projectCluster !== LOCAL && !unreachable.includes(projectCluster), staleTime: 60_000, retry: false,
  });
  const remote = projectCluster !== LOCAL ? clusterSettings.data : undefined;
  const ownAppsDomain = remote?.appsDomain && remote.appsDomain !== settings.data?.appsDomain ? remote.appsDomain : undefined;
  const appsDomain = projectCluster === LOCAL ? settings.data?.appsDomain : ownAppsDomain ?? settings.data?.appsDomain;
  const [domainTouched, setDomainTouched] = useState(false);
  const domain = appsDomain && !domainTouched ? `${f.name || "app"}.${appsDomain}` : f.domain;
  const domainHint = projectCluster !== LOCAL && !ownAppsDomain && appsDomain && domain.endsWith("." + appsDomain)
    ? !underWildcard(domain, appsDomain)
      ? `Kwerft keeps DNS records only for names one level below ${appsDomain}; create this one by hand, pointing at cluster ${projectCluster}.`
      : settings.data?.manageRecords
        ? `Kwerft points ${domain} at cluster ${projectCluster} with a DNS record of its own; the cluster gets a Let's Encrypt certificate once it is live, usually within a few minutes.`
        : `Point a DNS record for ${domain} at cluster ${projectCluster}${remote?.publicAddresses.length ? ` (${remote.publicAddresses.join(", ")})` : ""}; it gets a Let's Encrypt certificate automatically.`
    : !appsDomain || !domain.endsWith("." + appsDomain)
      ? `Point its DNS A record at ${projectCluster !== LOCAL ? `cluster ${projectCluster}` : "this server"}. Kwerft gets a Let's Encrypt certificate automatically.`
      : (remote ?? settings.data)?.wildcardDomain && underWildcard(domain, appsDomain)
        ? `Covered by the ${(remote ?? settings.data)!.wildcardDomain} wildcard: no DNS record or certificate to wait for.`
        : `The *.${appsDomain} DNS record covers it; Kwerft gets a Let's Encrypt certificate automatically.`;
  // Git connections that may build in this project; the one for the
  // repository's host is picked until the user chooses.
  const conns = useQuery({ queryKey: ["git-connections"], queryFn: gitApi.connections, enabled: f.source === "git", retry: false, staleTime: 30_000 });
  const usable = (conns.data ?? []).filter((c) => c.projects.length === 0 || !project || c.projects.includes(project));
  const [connTouched, setConnTouched] = useState(false);
  const connection = connTouched ? f.connection : suggestConnection(normalizeRepository(f.repo), usable, project);
  const form = { ...f, project, domain, connection };

  const deploy = useMutation({
    mutationFn: async () => {
      const app = await workloads.createApp(project, f.name, specOf(form));
      // Secret values go into the App's own set (created with the first
      // key, owned by the App); the App waits for them (SecretMissing).
      // One that fails shows there and on the Secrets page.
      for (const v of parseEnv(f.secretEnv).vars) {
        await secretsApi.setKey(project, appEnvSet(f.name), v.name, v.value).catch(() => undefined);
      }
      return app;
    },
    onSuccess: async (app) => {
      queryClient.setQueryData(["app", app.metadata.namespace, app.metadata.name], app);
      await queryClient.invalidateQueries({ queryKey: ["apps"] });
      // A Git app starts with its first build: the branch head. If that does
      // not work out, the Builds tab offers Build now.
      let build: string | undefined;
      if (app.spec.source.git) {
        try {
          build = (await buildsApi.buildNow(app.metadata.namespace, app.metadata.name)).name;
        } catch {
          /* shown on the Builds tab */
        }
      }
      await navigate({ to: "/apps/$project/$name", params: { project: app.metadata.namespace, name: app.metadata.name }, search: build ? { build } : {} });
    },
    onError: (e) => {
      const msg = e instanceof ApiError ? e.message : "The console could not be reached. Check your connection and try again.";
      const at = e instanceof ApiError ? locate(e.field) : undefined;
      if (at) go(at.step, true);
      setProblem({ field: at?.field, message: msg });
    },
  });

  function go(s: Step, force = false) {
    if (!force) {
      // Only move forward past steps that are valid.
      const order = steps.map((x) => x.id);
      const p = check(form);
      if (p && order.indexOf(p.step) < order.indexOf(s)) {
        setStep(p.step);
        setProblem(p);
        return;
      }
    }
    setProblem(undefined);
    setVisited((v) => new Set(v).add(s));
    setStep(s);
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    const order = steps.map((x) => x.id);
    const next = order[order.indexOf(step) + 1];
    if (next) return go(next);
    const p = check(form);
    if (p) {
      setStep(p.step);
      setProblem(p);
      return;
    }
    deploy.mutate();
  }

  const err = (field: string) => (problem?.field === field ? problem.message : undefined);
  const mountErr = /^mounts\[(\d+)\]$/.exec(problem?.field ?? "");
  const spec = specOf(form);
  const yaml = `apiVersion: kwerft.dev/v1alpha1\nkind: App\nmetadata:\n  name: ${f.name || "my-app"}\n  namespace: ${project || "my-project"}\nspec:\n${toYAML(spec, 1)}`;

  const choice = (
    <div className="choice" role="group" aria-label="Source">
      <button type="button" className="opt" aria-pressed={f.source === "image"} onClick={() => set("source", "image")}><b>Container image</b><span>Any registry, public or private</span></button>
      <button type="button" className="opt" aria-pressed={f.source === "git"} onClick={() => set("source", "git")}><b>Git repository</b><span>Built in-cluster, redeployed on every push</span></button>
      <button type="button" className="opt" aria-pressed={f.source === "compose"} onClick={() => set("source", "compose")}><b>Docker Compose</b><span>Paste a compose file; services become apps</span></button>
      <button type="button" className="opt" aria-pressed={f.source === "template"} onClick={() => set("source", "template")}><b>Template</b><span>PostgreSQL, Redis, MinIO, n8n, Plausible…</span></button>
    </div>
  );
  const projectField = (
    <div className="field">
      <label htmlFor="d-proj">Project</label>
      <select id="d-proj" className="input" value={project} aria-invalid={!!err("project")}
        onChange={(e) => (e.target.value === "+new" ? setCreatingProject(true) : set("project", e.target.value))}>
        {!project && <option value="">Choose a project…</option>}
        {projects.data?.map((p) => (
          <option key={p.name} value={p.name}>
            {p.displayName ? `${p.name} — ${p.displayName}` : p.name}{multi ? ` · cluster ${p.cluster ?? LOCAL}` : ""}
          </option>
        ))}
        {can.manageProjects && <option value="+new">New project…</option>}
      </select>
      {err("project") ? <span className="field-error" role="alert">{err("project")}</span>
        : projects.data?.length === 0 ? <span className="hint">No projects yet{can.manageProjects ? "; create one first." : ". Ask an owner or admin to create one."}</span>
        : multi && chosenProject ? <span className="hint">Runs in cluster <b>{chosenProject.cluster ?? LOCAL}</b>{unreachable.includes(chosenProject.cluster ?? LOCAL) ? ", which cannot be reached right now" : ""}.</span> : null}
    </div>
  );

  if (!can.deploy && session.isSuccess) {
    return (
      <section className="view">
        <div className="ph"><div><h1>Deploy an app</h1></div></div>
        <div className="empty"><h2>Your role cannot deploy</h2><p>Viewers can see apps but not create them. Ask an owner or admin for the developer role.</p><Link to="/apps" className="btn">Back to apps</Link></div>
      </section>
    );
  }

  if (f.source === "compose" || f.source === "template") {
    return (
      <section className="view">
        <div className="ph">
          <div>
            <h1>Deploy an app</h1>
            <p className="sub">
              {f.source === "compose"
                ? <>Each service of the file becomes an <code>App</code>; Kwerft shows what it creates, and what it does differently, before anything exists.</>
                : <>Ready-made apps with pinned versions and generated passwords, created as <code>App</code>, <code>Volume</code> and <code>SecretSet</code> resources.</>}
            </p>
          </div>
        </div>
        <ImportFlow key={f.source} kind={f.source} project={project} appsDomain={appsDomain}
          top={<>{choice}<div className="card"><div className="bd fields">{projectField}</div></div></>}
          requireProject={() => {
            if (project) return true;
            setProblem({ field: "project", message: "Choose a project." });
            return false;
          }} />
        {creatingProject && <CreateProjectDialog onClose={() => setCreatingProject(false)} onCreated={(p) => set("project", p.name)} />}
      </section>
    );
  }

  return (
    <section className="view">
      <div className="ph">
        <div>
          <h1>Deploy an app</h1>
          <p className="sub">Everything here becomes one <code>App</code> resource. You can copy it as YAML at the end.</p>
        </div>
      </div>

      <div className="steps">
        {steps.map((s, i) => (
          <button key={s.id} type="button" className={visited.has(s.id) && s.id !== step ? "done" : ""} aria-current={s.id === step ? "step" : undefined}
            disabled={!visited.has(s.id)} onClick={() => go(s.id)}>
            <span>Step {i + 1}</span><b>{s.title}</b>
          </button>
        ))}
      </div>

      <form className="tabpanel" onSubmit={submit} noValidate>
        {step === "source" && (
          <>
            {choice}
            <div className="card"><div className="bd fields">
              <Field id="d-name" label="App name" className="mono" value={f.name} onChange={(e) => set("name", e.target.value.toLowerCase())} placeholder="invoice-renderer"
                autoFocus autoComplete="off" spellCheck={false} maxLength={63} error={err("name")} hint="Also the in-cluster hostname. Lowercase letters, digits and dashes." />
              {projectField}
              {f.source === "image" ? (
                <>
                  <div className="full">
                    <Field id="d-img" label="Image" className="mono" value={f.image} onChange={(e) => set("image", e.target.value)} placeholder="ghcr.io/acme/invoice-renderer:0.3.0"
                      autoComplete="off" spellCheck={false} error={err("image")} hint="Pin a version tag or digest rather than latest, so rollbacks mean something." />
                  </div>
                  <Field id="d-cred" label="Registry credential" className="mono" value={f.pullSecret} onChange={(e) => set("pullSecret", e.target.value)} placeholder="None (public image)"
                    autoComplete="off" spellCheck={false} hint="Name of a docker-registry Secret in the project, for private images." />
                </>
              ) : (
                <GitFields f={form} set={(k, v) => set(k, v as never)} err={err} conns={usable} connsError={conns.error} project={project}
                  onConnection={(v) => { setConnTouched(true); set("connection", v); }} />
              )}
            </div></div>
          </>
        )}

        {step === "runtime" && (
          <>
            <div className="choice three" role="group" aria-label="Size">
              {sizes.map((s) => (
                <button type="button" key={s.id} className="opt" aria-pressed={f.size === s.id} onClick={() => set("size", s.id)}><b>{s.label}</b><span>{s.note}</span></button>
              ))}
            </div>
            <div className="card"><div className="bd fields">
              <Field id="d-rep" label="Replicas" type="number" min={0} value={f.replicas} onChange={(e) => set("replicas", e.target.value)} error={err("replicas")}
                hint="Spread across nodes when possible." />
              <div className="field">
                <label>Health check</label>
                <div className="seg" role="group" aria-label="Health check">
                  {(["none", "http", "tcp"] as const).map((v) => (
                    <button type="button" key={v} aria-pressed={f.hc === v} onClick={() => set("hc", v)}>{{ none: "None", http: "HTTP GET", tcp: "TCP connect" }[v]}</button>
                  ))}
                </div>
              </div>
              {f.hc === "http" && <Field id="d-hc-path" label="Health check path" className="mono" value={f.hcPath} onChange={(e) => set("hcPath", e.target.value)} />}
              {f.hc !== "none" && <Field id="d-hc-port" label="Health check port" className="mono" value={f.hcPort} onChange={(e) => set("hcPort", e.target.value)}
                placeholder={f.port || "8080"} error={err("hcPort")} hint="Defaults to the container port." />}
              <div className="field full">
                <label htmlFor="d-env">Environment (KEY=value, one per line)</label>
                <textarea id="d-env" className="input mono" rows={4} value={f.env} onChange={(e) => set("env", e.target.value)} placeholder={"PDF_TIMEOUT=30s\nS3_BUCKET=acme-invoices"}
                  spellCheck={false} aria-invalid={!!err("env")} aria-describedby="d-env-note" />
                {err("env") ? <span id="d-env-note" className="field-error" role="alert">{err("env")}</span>
                  : <span id="d-env-note" className="hint">Stored in the App resource, readable by everyone with access. Put passwords and tokens under secret variables.</span>}
              </div>
              <div className="field full">
                <label htmlFor="d-secret-env">Secret variables (KEY=value, one per line)</label>
                <textarea id="d-secret-env" className="input mono" rows={2} value={f.secretEnv} onChange={(e) => set("secretEnv", e.target.value)} placeholder="SESSION_SECRET=…"
                  spellCheck={false} autoComplete="off" aria-invalid={!!err("secretEnv")} aria-describedby="d-secret-env-note" />
                {err("secretEnv") ? <span id="d-secret-env-note" className="field-error" role="alert">{err("secretEnv")}</span>
                  : <span id="d-secret-env-note" className="hint">Write-only: kept in the app's own secret set ({appEnvSet(f.name || "app")}), never shown again. Keys of shared sets are picked in the app's settings.</span>}
              </div>
              <div className="field full">
                <label>Shared volumes and secrets</label>
                <VolumeMounts project={project} mounts={f.mounts} onChange={(v) => set("mounts", v)} idPrefix="d-vol"
                  errorAt={mountErr ? [Number(mountErr[1]), problem!.message] : undefined} />
                <span className="hint">Volumes of the project that other apps and jobs can mount too, and Secrets as read-only files, one per key. The app stays a Deployment; its pods run on the volume's node.</span>
              </div>
            </div></div>
          </>
        )}

        {step === "network" && (
          <div className="card"><div className="bd fields">
            <Field id="d-port" label="Container port" className="mono" value={f.port} onChange={(e) => set("port", e.target.value)} placeholder="3000" inputMode="numeric"
              error={err("port")} hint="Leave empty for workers that only connect out." />
            <div className="field">
              <label>Exposure</label>
              <div className="seg" role="group" aria-label="Exposure">
                <button type="button" aria-pressed={f.exposure === "cluster"} onClick={() => set("exposure", "cluster")}>Cluster only</button>
                <button type="button" aria-pressed={f.exposure === "public"} onClick={() => set("exposure", "public")}>Public domain</button>
              </div>
            </div>
            {f.exposure === "public" && (
              <div className="full">
                <Field id="d-dom" label="Domain" className="mono" value={domain} onChange={(e) => { setDomainTouched(true); set("domain", e.target.value); }}
                  placeholder={appsDomain ? `invoices.${appsDomain}` : "invoices.example.com"} error={err("domain")} hint={domainHint} />
              </div>
            )}
            <div className="full">
              <Field id="d-allow" label="Who may connect to this app inside the cluster?" className="mono" value={f.allowFrom} onChange={(e) => set("allowFrom", e.target.value)}
                placeholder="api, storefront/billing-worker" hint="Apps by name, or project/app. Everything else is denied; traffic for the public domain is always allowed." />
            </div>
            <div className="field full">
              <label>Outbound access</label>
              <div className="seg" role="group" aria-label="Outbound access">
                {(["none", "https", "all"] as const).map((v) => (
                  <button type="button" key={v} aria-pressed={f.egress === v} onClick={() => set("egress", v)}>{{ none: "None", https: "HTTPS to internet", all: "Unrestricted" }[v]}</button>
                ))}
              </div>
            </div>
          </div></div>
        )}

        {step === "review" && (
          <div className="g2">
            <div className="card">
              <div className="ch-h"><h3>App resource</h3>
                <button type="button" className="btn sm" onClick={async () => { try { await navigator.clipboard.writeText(yaml); setCopied(true); } catch { /* selectable anyway */ } }}>
                  {copied ? "Copied" : "Copy YAML"}
                </button>
              </div>
              <div className="bd"><pre className="codebox">{yaml}</pre></div>
            </div>
            <div className="card">
              <h3>Kwerft will create</h3>
              <div className="list">
                {spec.source.git && <div className="li"><span className="tag">Build</span><span className="dim">the head of {spec.source.git.branch} with {spec.source.git.builder === "railpack" ? "Railpack" : spec.source.git.dockerfile}; the app starts when it succeeds</span></div>}
                <div className="li"><span className="tag">Deployment</span><span className="dim">{spec.replicas} replica{spec.replicas === 1 ? "" : "s"}, rolling update</span></div>
                {parseEnv(f.secretEnv).vars.length > 0 && (
                  <div className="li"><span className="tag">SecretSet</span><span className="dim">{appEnvSet(f.name)} with {parseEnv(f.secretEnv).vars.map((v) => v.name).join(", ")}, write-only, deleted with the app</span></div>
                )}
                {spec.ports!.length > 0 && <div className="li"><span className="tag">Service</span><span className="dim">ClusterIP :{spec.ports![0]!.container}</span></div>}
                {spec.ports![0]?.public && <div className="li"><span className="tag">HTTPRoute</span><span className="dim">{spec.ports![0].public}, HTTPS with redirect</span></div>}
                {spec.ports![0]?.public && <div className="li"><span className="tag">Domain</span><span className="dim">Gateway listener and certificate</span></div>}
                <div className="li"><span className="tag">NetworkPolicy</span><span className="dim">{spec.allowFrom!.length ? `${spec.allowFrom!.length} source${spec.allowFrom!.length > 1 ? "s" : ""}` : "no app sources"}, egress {spec.egress}</span></div>
              </div>
            </div>
          </div>
        )}

        {problem && !["name", "project", "image", "repo", "branch", "connection", "dockerfile", "path", "replicas", "env", "secretEnv", "hcPort", "port", "domain"].includes(problem.field ?? "") && !problem.field?.startsWith("mounts[") && (
          <div className="banner bad" role="alert"><Icon name="alert" /><span>{problem.message}</span></div>
        )}

        <div className="acts" style={{ justifyContent: step === "source" ? "flex-end" : "space-between" }}>
          {step !== "source" && (
            <button type="button" className="btn" onClick={() => go(steps[steps.findIndex((s) => s.id === step) - 1]!.id, true)}>Back</button>
          )}
          {step === "review" ? (
            <button className="btn pri" disabled={deploy.isPending}><Icon name="rocket" />{deploy.isPending ? "Deploying…" : `Deploy ${f.name || "app"}`}</button>
          ) : (
            <button className="btn pri">{step === "network" ? "Review" : "Continue"}</button>
          )}
        </div>
      </form>

      {creatingProject && <CreateProjectDialog onClose={() => setCreatingProject(false)} onCreated={(p) => set("project", p.name)} />}
    </section>
  );
}

type GitForm = Pick<Form, "repo" | "branch" | "connection" | "builder" | "dockerfile" | "path" | "autoDeploy">;

// The Git repository source: repository with a check, branch, connection,
// how to build, and whether pushes deploy. Shared by the wizard and the
// app's settings.
export function GitFields<F extends GitForm>({ f, set, err, conns, connsError, project, onConnection, idPrefix = "d" }: {
  f: F;
  set: (k: keyof GitForm, v: string | boolean) => void;
  err: (field: string) => string | undefined;
  conns: Connection[];
  connsError: unknown;
  project: string;
  onConnection: (v: string) => void;
  idPrefix?: string;
}) {
  const id = (s: string) => `${idPrefix}-${s}`;
  const query: CheckQuery = {
    repository: normalizeRepository(f.repo), branch: f.branch.trim() || undefined, connection: f.connection || undefined,
    path: f.path.trim() || undefined, dockerfile: f.builder === "dockerfile" ? f.dockerfile.trim() || "Dockerfile" : undefined,
  };
  const key = JSON.stringify(query);
  const [check, setCheck] = useState<{ key: string; result?: CheckResult; error?: string }>();
  const [checking, setChecking] = useState(false);
  const shown = check?.key === key ? check : undefined;
  const branch = f.branch.trim() || "main";
  const known = conns.some((c) => c.name === f.connection);

  async function runCheck() {
    const problem = repositoryProblem(query.repository);
    if (problem) return setCheck({ key, error: problem });
    setChecking(true);
    try {
      setCheck({ key, result: await gitApi.check(query) });
    } catch (e) {
      setCheck({ key, error: notOffered(e) ? "This console cannot check repositories yet; the first build will tell." : e instanceof ApiError ? e.message : "The console could not be reached." });
    } finally {
      setChecking(false);
    }
  }

  return (
    <>
      <div className="full">
        <div className="git-row">
          <Field id={id("repo")} label="Repository" className="mono" value={f.repo} onChange={(e) => set("repo", e.target.value)}
            onBlur={() => f.repo.trim() && set("repo", normalizeRepository(f.repo))}
            placeholder="github.com/acme/invoice-renderer" autoComplete="off" spellCheck={false} error={err("repo")}
            hint="HTTPS (github.com/acme/api) or SSH (git@github.com:acme/api.git)." />
          <button type="button" className="btn" onClick={runCheck} disabled={checking || !f.repo.trim()}>{checking ? "Checking…" : "Check"}</button>
        </div>
        {shown && <CheckLine check={shown} builder={f.builder} dockerfile={query.dockerfile} branch={branch} onBranch={(b) => set("branch", b)} />}
      </div>
      <Field id={id("branch")} label="Branch" className="mono" value={f.branch} onChange={(e) => set("branch", e.target.value)} placeholder="main"
        autoComplete="off" spellCheck={false} error={err("branch")} />
      <div className="field">
        <label htmlFor={id("conn")}>Access</label>
        <select id={id("conn")} className="input" value={f.connection} onChange={(e) => onConnection(e.target.value)} aria-invalid={!!err("connection")}
          aria-describedby={`${id("conn")}-note`}>
          <option value="">Public repository (no credentials)</option>
          {conns.map((c) => <option key={c.name} value={c.name}>{c.name} · {c.url.replace(/^https:\/\//, "")}{c.owner ? `/${c.owner}` : ""}{c.ready ? "" : " (not ready)"}</option>)}
          {f.connection && !known && <option value={f.connection}>{f.connection}</option>}
        </select>
        {err("connection") ? <span id={`${id("conn")}-note`} className="field-error" role="alert">{err("connection")}</span>
          : <span id={`${id("conn")}-note`} className="hint">
            {notOffered(connsError) ? "Private repositories need a Git connection, which this console cannot manage yet."
              : conns.length === 0 ? `No Git connections${project ? ` for ${project}` : ""} yet. Owners and admins add them in Settings for private repositories.`
              : "A Git connection clones private repositories and reports build status on commits."}
          </span>}
      </div>
      <div className="field">
        <label>Build with</label>
        <div className="seg" role="group" aria-label="Build with">
          <button type="button" aria-pressed={f.builder === "dockerfile"} onClick={() => set("builder", "dockerfile")}>Dockerfile</button>
          <button type="button" aria-pressed={f.builder === "railpack"} onClick={() => set("builder", "railpack")}>Railpack</button>
        </div>
        <span className="hint">{f.builder === "dockerfile" ? "Builds the repository's Dockerfile with rootless BuildKit." : "Detects the language (Node, Go, Python, …) and builds without a Dockerfile."}</span>
      </div>
      {f.builder === "dockerfile" && (
        <Field id={id("dockerfile")} label="Dockerfile" className="mono" value={f.dockerfile} onChange={(e) => set("dockerfile", e.target.value)} placeholder="Dockerfile"
          autoComplete="off" spellCheck={false} error={err("dockerfile")} hint="Relative to the directory." />
      )}
      <Field id={id("path")} label="Directory" className="mono" value={f.path} onChange={(e) => set("path", e.target.value)} placeholder="/"
        autoComplete="off" spellCheck={false} error={err("path")} hint="The build context inside the repository, for monorepos." />
      <div className="field full">
        <label id={id("push-label")}>On push</label>
        <span>
          <Toggle on={f.autoDeploy} onChange={(v) => set("autoDeploy", v)}>
            {f.autoDeploy ? <>Build and deploy every push to <code>{branch}</code> · pull requests get a build check only</> : "Build only when someone presses Build now"}
          </Toggle>
        </span>
      </div>
    </>
  );
}

function CheckLine({ check, builder, dockerfile, branch, onBranch }: {
  check: { result?: CheckResult; error?: string }; builder: string; dockerfile?: string; branch: string; onBranch: (b: string) => void;
}) {
  const r = check.result;
  if (!r) return <p className="form-error check-line" role="status">{check.error}</p>;
  if (!r.ok) {
    return (
      <p className="check-line" role="status">
        <span className="form-error">{r.message}</span>
        {r.defaultBranch && r.defaultBranch !== branch && <button type="button" className="btn sm" onClick={() => onBranch(r.defaultBranch!)}>Use {r.defaultBranch}</button>}
      </p>
    );
  }
  return (
    <p className="check-line" role="status">
      <span className="ok-text">
        ✓ {r.connection ? `Access through ${r.connection}` : "Reachable"}
        {r.head && <> · last commit {shortSha(r.head.sha)} “{r.head.message.split("\n")[0]}”{r.head.author ? ` by ${r.head.author}` : ""}</>}
      </span>
      {builder === "dockerfile" && r.dockerfile === true && <span className="ok-text">· {dockerfile} found</span>}
      {builder === "dockerfile" && r.dockerfile === false && <span className="warn-text">· No {dockerfile} there: fix the path, or build with Railpack.</span>}
    </p>
  );
}
