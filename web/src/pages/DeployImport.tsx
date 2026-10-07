// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { useState, type ChangeEvent, type FormEvent, type ReactNode } from "react";
import { Link } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query";
import { ApiError } from "../api";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import {
  countWarnings, createdApps, importApi, paramProblem, parseDotEnv, planLines, templateValues, warningsByService,
  type ImportPlan, type Template, type TemplateParam,
} from "../imports";
import "../styles/imports.css";

// The New App wizard's "Docker Compose" and "Template" sources. Both check
// first (a dry run that shows what would be created, and why something
// would not work as in Docker), then create everything at once.

type Props = {
  kind: "compose" | "template";
  project: string;
  appsDomain?: string;
  /** The source choice and the project field, shown on the first step. */
  top: ReactNode;
  /** Says whether a project is chosen, and asks for one if not. */
  requireProject: () => boolean;
};

type Problem = { field?: string; message: string };

const problemOf = (e: unknown): Problem =>
  e instanceof ApiError ? { field: e.field, message: e.message } : { message: "The console could not be reached. Check your connection and try again." };

const MAX_FILE = 256 << 10;

async function refresh(qc: QueryClient, project: string) {
  await Promise.all([
    qc.invalidateQueries({ queryKey: ["apps"] }),
    qc.invalidateQueries({ queryKey: ["volumes"] }),
    qc.invalidateQueries({ queryKey: ["secret-sets", project] }),
  ]);
}

export function ImportFlow(props: Props) {
  return props.kind === "compose" ? <ComposeImport {...props} /> : <TemplateImport {...props} />;
}

function Steps<S extends string>({ steps, at, reached, onGo }: { steps: { id: S; title: string }[]; at: S; reached: S[]; onGo: (s: S) => void }) {
  return (
    <div className="steps" style={{ gridTemplateColumns: `repeat(${steps.length}, minmax(0, 1fr))` }}>
      {steps.map((s, i) => (
        <button key={s.id} type="button" className={reached.includes(s.id) && s.id !== at ? "done" : ""} aria-current={s.id === at ? "step" : undefined}
          disabled={!reached.includes(s.id)} onClick={() => onGo(s.id)}>
          <span>Step {i + 1}</span><b>{s.title}</b>
        </button>
      ))}
    </div>
  );
}

// ---- Compose ----------------------------------------------------------------------

type ComposeStep = "file" | "review" | "done";

function ComposeImport({ project, top, requireProject }: Props) {
  const qc = useQueryClient();
  const [step, setStep] = useState<ComposeStep>("file");
  const [text, setText] = useState("");
  const [envText, setEnvText] = useState("");
  const [plan, setPlan] = useState<ImportPlan>();
  const [problem, setProblem] = useState<Problem>();
  const env = parseDotEnv(envText);

  const run = useMutation({
    mutationFn: (dryRun: boolean) => importApi.compose(project, text, env.env, dryRun),
    onSuccess: async (p) => {
      setPlan(p);
      setProblem(undefined);
      if (p.dryRun) return setStep("review");
      setStep("done");
      await refresh(qc, project);
    },
    onError: (e) => {
      const p = problemOf(e);
      if (p.field === "compose") setStep("file");
      setProblem(p);
    },
  });

  function submit(e: FormEvent) {
    e.preventDefault();
    if (step === "review") return run.mutate(false);
    if (!requireProject()) return;
    if (!text.trim()) return setProblem({ field: "compose", message: "Paste a Compose file, or upload one." });
    if (env.bad) return setProblem({ field: "env", message: `Line ${env.bad} is not KEY=value.` });
    run.mutate(true);
  }

  async function upload(e: ChangeEvent<HTMLInputElement>) {
    const file = e.target.files?.[0];
    e.target.value = "";
    if (!file) return;
    if (file.size > MAX_FILE) return setProblem({ field: "compose", message: "The file is larger than 256 KiB." });
    setText(await file.text());
    setPlan(undefined);
    setProblem(undefined);
  }

  const err = (field: string) => (problem?.field === field ? problem.message : undefined);
  const apps = plan?.apps.length ?? 0;
  return (
    <>
      <Steps steps={[{ id: "file", title: "Compose file" }, { id: "review", title: "Review" }]} at={step === "done" ? "review" : step}
        reached={step === "done" ? [] : plan ? ["file", "review"] : ["file"]} onGo={(s) => { setProblem(undefined); setStep(s); }} />
      <form className="tabpanel" onSubmit={submit} noValidate>
        {step === "file" && (
          <>
            {top}
            <div className="card"><div className="bd fields">
              <div className="field full">
                <div className="imp-label">
                  <label htmlFor="c-file">Compose file</label>
                  <label className="btn sm imp-upload">Upload…<input type="file" accept=".yml,.yaml,application/yaml,text/yaml" onChange={upload} /></label>
                </div>
                <textarea id="c-file" className="input mono" rows={18} value={text} spellCheck={false} aria-invalid={!!err("compose")} aria-describedby="c-file-note"
                  onChange={(e) => { setText(e.target.value); setPlan(undefined); setProblem((p) => (p?.field === "compose" ? undefined : p)); }}
                  placeholder={"services:\n  web:\n    image: ghcr.io/acme/web:1.4.2\n    ports: [\"8080:3000\"]\n    environment:\n      DATABASE_URL: postgres://web:${DB_PASSWORD}@db:5432/web\n  db:\n    image: postgres:18.6\n    volumes: [\"db-data:/var/lib/postgresql\"]\nvolumes:\n  db-data:"} />
                {err("compose") ? <span id="c-file-note" className="field-error" role="alert">{err("compose")}</span>
                  : <span id="c-file-note" className="hint">Each service becomes an app, named volumes become Volumes (5 GiB unless <code>x-kwerft: {"{size: 20Gi}"}</code> says otherwise). Nothing is created before you confirm.</span>}
              </div>
              <div className="field full">
                <label htmlFor="c-env">Values for <code>{"${VAR}"}</code> references (KEY=value, like a .env file)</label>
                <textarea id="c-env" className="input mono" rows={3} value={envText} spellCheck={false} autoComplete="off" aria-invalid={!!err("env")} aria-describedby="c-env-note"
                  onChange={(e) => { setEnvText(e.target.value); setPlan(undefined); setProblem((p) => (p?.field === "env" ? undefined : p)); }} placeholder="DB_PASSWORD=…" />
                {err("env") ? <span id="c-env-note" className="field-error" role="alert">{err("env")}</span>
                  : <span id="c-env-note" className="hint">Used only to fill in the file. Variables whose names look secret (PASSWORD, TOKEN, KEY, …) go write-only into each app's own secret set.</span>}
              </div>
            </div></div>
          </>
        )}
        {step === "review" && plan && <PlanReview plan={plan} />}
        {step === "done" && plan && <Created plan={plan} project={project} />}

        {problem && !["compose", "env"].includes(problem.field ?? "") && (
          <div className="banner bad" role="alert"><Icon name="alert" /><span>{problem.message}</span></div>
        )}

        <div className="acts" style={{ justifyContent: step === "file" ? "flex-end" : "space-between" }}>
          {step === "review" && <button type="button" className="btn" onClick={() => setStep("file")}>Back</button>}
          {step === "file" && <button className="btn pri" disabled={run.isPending}>{run.isPending ? "Checking…" : "Check"}</button>}
          {step === "review" && (
            <button className="btn pri" disabled={run.isPending || !!plan?.problems.length}>
              <Icon name="rocket" />{run.isPending ? "Creating…" : `Create ${apps} app${apps === 1 ? "" : "s"}`}
            </button>
          )}
          {step === "done" && (
            <>
              <button type="button" className="btn" onClick={() => { setPlan(undefined); setText(""); setEnvText(""); setStep("file"); }}>Import another file</button>
              <Link to="/apps" className="btn pri">Go to apps</Link>
            </>
          )}
        </div>
      </form>
    </>
  );
}

// ---- templates ----------------------------------------------------------------------

type TemplateStep = "template" | "params" | "review" | "done";

function TemplateImport({ project, appsDomain, top, requireProject }: Props) {
  const qc = useQueryClient();
  const catalog = useQuery({ queryKey: ["templates"], queryFn: importApi.templates, staleTime: Infinity });
  const [step, setStep] = useState<TemplateStep>("template");
  const [chosen, setChosen] = useState<Template>();
  const [values, setValues] = useState<Record<string, string>>({});
  const [touched, setTouched] = useState<Set<string>>(new Set());
  const [plan, setPlan] = useState<ImportPlan>();
  const [problem, setProblem] = useState<Problem>();
  const vals = chosen ? templateValues(chosen, appsDomain, values, touched) : {};

  const run = useMutation({
    mutationFn: (dryRun: boolean) => importApi.template(project, chosen!.id, vals, dryRun),
    onSuccess: async (p) => {
      setPlan(p);
      setProblem(undefined);
      if (p.dryRun) return setStep("review");
      setStep("done");
      await refresh(qc, project);
    },
    onError: (e) => {
      const p = problemOf(e);
      if (p.field?.startsWith("parameters.")) {
        setStep("params");
        p.field = p.field.slice("parameters.".length);
      }
      setProblem(p);
    },
  });

  function choose(t: Template) {
    if (!requireProject()) return;
    if (t.id !== chosen?.id) {
      setChosen(t);
      setValues({});
      setTouched(new Set());
      setPlan(undefined);
    }
    setProblem(undefined);
    setStep("params");
  }

  function setValue(key: string, v: string) {
    setValues({ ...vals, [key]: v });
    setTouched((t) => new Set(t).add(key));
    setPlan(undefined); // the plan was for other values
    setProblem((p) => (p?.field === key ? undefined : p));
  }

  function submit(e: FormEvent) {
    e.preventDefault();
    if (!chosen) return;
    if (step === "review") return run.mutate(false);
    if (!requireProject()) return;
    for (const p of chosen.parameters) {
      const msg = paramProblem(p, vals[p.key] ?? "");
      if (msg) return setProblem({ field: p.key, message: msg });
    }
    run.mutate(true);
  }

  const err = (field: string) => (problem?.field === field ? problem.message : undefined);
  const reached: TemplateStep[] = step === "done" ? [] : ["template", ...(chosen ? ["params" as const] : []), ...(plan ? ["review" as const] : [])];
  return (
    <>
      <Steps steps={[{ id: "template", title: "Template" }, { id: "params", title: "Settings" }, { id: "review", title: "Review" }]}
        at={step === "done" ? "review" : step} reached={reached} onGo={(s) => { setProblem(undefined); setStep(s); }} />
      <form className="tabpanel" onSubmit={submit} noValidate>
        {step === "template" && (
          <>
            {top}
            {catalog.isPending && <p className="loading">Loading templates…</p>}
            {catalog.isError && <div className="banner bad" role="alert"><Icon name="alert" /><span>{problemOf(catalog.error).message}</span></div>}
            <div className="tpl-grid" role="group" aria-label="Templates">
              {catalog.data?.map((t) => (
                <button type="button" key={t.id} className="opt tpl" aria-pressed={chosen?.id === t.id} onClick={() => choose(t)}>
                  <span className="tpl-top"><b>{t.title}</b><span className="pill mute nodot">{t.category}</span></span>
                  <span>{t.description}</span>
                  <span className="tpl-images">{t.images.join(" · ")}</span>
                </button>
              ))}
            </div>
          </>
        )}

        {step === "params" && chosen && (
          <div className="card">
            <div className="ch-h"><h3>{chosen.title}</h3><span className="hint">Versions pinned {chosen.pinned}</span></div>
            <div className="bd fields">
              {chosen.parameters.map((p) => <ParamField key={p.key} p={p} value={vals[p.key] ?? ""} error={err(p.key)} onChange={(v) => setValue(p.key, v)} appsDomain={appsDomain} />)}
            </div>
          </div>
        )}
        {step === "review" && plan && <PlanReview plan={plan} />}
        {step === "done" && plan && <Created plan={plan} project={project} />}

        {problem && !(step === "params" && chosen?.parameters.some((p) => p.key === problem.field)) && (
          <div className="banner bad" role="alert"><Icon name="alert" /><span>{problem.message}</span></div>
        )}

        {step !== "template" && (
          <div className="acts" style={{ justifyContent: "space-between" }}>
            {step === "params" && <button type="button" className="btn" onClick={() => setStep("template")}>Back</button>}
            {step === "review" && <button type="button" className="btn" onClick={() => setStep("params")}>Back</button>}
            {step === "params" && <button className="btn pri" disabled={run.isPending}>{run.isPending ? "Checking…" : "Review"}</button>}
            {step === "review" && (
              <button className="btn pri" disabled={run.isPending || !!plan?.problems.length}><Icon name="rocket" />{run.isPending ? "Creating…" : `Create ${chosen?.title}`}</button>
            )}
            {step === "done" && (
              <>
                <button type="button" className="btn" onClick={() => { setPlan(undefined); setChosen(undefined); setStep("template"); }}>Another template</button>
                <Link to="/apps" className="btn pri">Go to apps</Link>
              </>
            )}
          </div>
        )}
      </form>
    </>
  );
}

function ParamField({ p, value, error, onChange, appsDomain }: { p: TemplateParam; value: string; error?: string; onChange: (v: string) => void; appsDomain?: string }) {
  const id = `t-${p.key}`;
  const hint = p.type === "hostname" && !p.required ? `${p.hint ?? ""}${appsDomain ? "" : " No apps domain is set, so nothing is suggested."}`.trim() : p.hint;
  if (p.type === "enum") {
    return (
      <div className="field">
        <label htmlFor={id}>{p.label}</label>
        <select id={id} className="input" value={value} onChange={(e) => onChange(e.target.value)} aria-invalid={!!error} aria-describedby={`${id}-note`}>
          {p.options?.map((o) => <option key={o} value={o}>{o}</option>)}
        </select>
        {error ? <span id={`${id}-note`} className="field-error" role="alert">{error}</span> : p.hint ? <span id={`${id}-note`} className="hint">{p.hint}</span> : null}
      </div>
    );
  }
  const mono = p.type !== "text" || p.key === "timezone";
  return (
    <div className={p.type === "hostname" || p.type === "apps" ? "full" : undefined}>
      <Field id={id} label={p.label + (p.required ? "" : p.type === "hostname" || p.type === "apps" ? " (optional)" : "")} className={mono ? "mono" : undefined}
        value={value} error={error} hint={hint} autoComplete="off" spellCheck={false}
        placeholder={p.type === "hostname" ? `${p.suffix ? "console" : "app"}.example.com` : p.type === "apps" ? "api, shop/worker" : p.default}
        onChange={(e) => onChange(p.type === "name" || p.type === "hostname" || p.type === "identifier" ? e.target.value.toLowerCase() : e.target.value)} />
    </div>
  );
}

// ---- the plan ------------------------------------------------------------------------

function PlanReview({ plan }: { plan: ImportPlan }) {
  const lines = planLines(plan);
  const groups = warningsByService(plan.warnings);
  const warnings = countWarnings(plan.warnings);
  return (
    <>
      {plan.problems.length > 0 && (
        <div className="banner bad" role="alert">
          <Icon name="alert" />
          <span>Nothing can be created yet:
            <ul className="imp-problems">{plan.problems.map((p, i) => <li key={i}><b>{p.object}</b>{p.field ? <> <code>{p.field}</code></> : null}: {p.message}</li>)}</ul>
          </span>
        </div>
      )}
      <div className={groups.length ? "g2" : undefined}>
        <div className="card">
          <h3>Kwerft will create</h3>
          <div className="list">
            {lines.map((l) => (
              <div className="li" key={`${l.kind}/${l.name}`}>
                <span className="tag">{l.kind}</span>
                <div>
                  <b className="mono">{l.name}</b>
                  <p>{l.detail}</p>
                  {l.public.map((h) => <p key={h} className="imp-public">https://{h}</p>)}
                </div>
              </div>
            ))}
          </div>
          {plan.renames.length > 0 && (
            <>
              <h3 className="sep">Renamed to valid names</h3>
              <div className="list">
                {plan.renames.map((r) => <div className="li" key={`${r.kind}/${r.from}`}><span className="tag">{r.kind}</span><span className="mono">{r.from} → {r.to}</span></div>)}
              </div>
            </>
          )}
        </div>
        {groups.length > 0 && (
          <div className="card">
            <h3>{warnings === 0 ? "Notes" : `${warnings} warning${warnings === 1 ? "" : "s"}`}</h3>
            <div className="imp-warnings">
              {groups.map(([service, ws]) => (
                <div key={service || "-"} className="imp-group">
                  <b>{service ? <span className="mono">{service}</span> : "The file"}</b>
                  <ul>
                    {ws.map((w, i) => (
                      <li key={i} className={w.level}>
                        <Icon name={w.level === "warning" ? "alert" : "gear"} />
                        <span>{w.key && !w.message.startsWith(w.key) && <><code>{w.key}</code> </>}{w.message}</span>
                      </li>
                    ))}
                  </ul>
                </div>
              ))}
            </div>
          </div>
        )}
      </div>
      {plan.notes && <div className="banner info"><Icon name="key" /><span>{plan.notes}</span></div>}
    </>
  );
}

function Created({ plan, project }: { plan: ImportPlan; project: string }) {
  const apps = createdApps(plan.created);
  return (
    <>
      <div className="banner info" role="status">
        <Icon name="rocket" />
        <span>Created {plan.created.length} object{plan.created.length === 1 ? "" : "s"} in {project}. Apps start once their images are pulled; generated secrets appear within seconds, and apps wait for them.</span>
      </div>
      {plan.notes && <div className="card"><h3>Next</h3><div className="bd"><p className="imp-notes">{plan.notes}</p></div></div>}
      <div className="card">
        <h3>Created</h3>
        <div className="list">
          {plan.created.map((o) => {
            const [kind, name] = o.split("/") as [string, string];
            return (
              <div className="li" key={o}>
                <span className="tag">{kind}</span>
                {kind === "App" && apps.includes(name)
                  ? <Link to="/apps/$project/$name" params={{ project, name }} className="mono">{name}</Link>
                  : kind === "SecretSet" ? <Link to="/secrets" search={{ project, set: name }} className="mono">{name}</Link>
                  : <span className="mono">{name}</span>}
              </div>
            );
          })}
        </div>
      </div>
    </>
  );
}
