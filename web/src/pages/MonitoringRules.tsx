import { useState } from "react";
import { Link } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../api";
import {
  RULE_NAME_RE, alertAbilities, alertKeys, alertingUnavailable, alertsApi, conditionOrder, conditions, describeChannels,
  describeCondition, describeScope, durationProblem, goDuration, parseDuration, severities, severityTone, shortDuration, thresholdProblem,
  type AlertCondition, type Channel, type Rule, type RuleInput, type Severity,
} from "../alerts";
import { Dialog } from "../components/Dialog";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { workloads } from "../workloads";
import { ClusterBadge } from "../components/ClusterUI";
import { MonitoringLayout } from "./Monitoring";
import { errText } from "./MonitoringAlerts";
import "../styles/monitoring.css";

const unreachable = "The console could not be reached. Check your connection and try again.";
/** "A container…" → "a container…", but "CPU use…" stays. */
const lcFirst = (s: string) => (/^[A-Z][A-Z]/.test(s) ? s : s.charAt(0).toLowerCase() + s.slice(1));

// Monitoring › Alert rules: what Kwerft watches, how bad it is and who hears
// about it. Default rules can be changed and disabled; deleted, they come back.
export function MonitoringRules() {
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const can = alertAbilities(session.data?.role);
  const rules = useQuery({ queryKey: alertKeys.rules, queryFn: alertsApi.rules, refetchInterval: (q) => (q.state.data?.some((r) => !r.ready && !r.disabled) ? 5000 : 30000), retry: false });
  const channels = useQuery({ queryKey: alertKeys.channels, queryFn: alertsApi.channels, retry: false });
  const [dialog, setDialog] = useState<{ edit?: Rule } | { remove: Rule }>();
  const unavailable = rules.isError && alertingUnavailable(rules.error);
  const list = [...(rules.data ?? [])].sort((a, b) => Number(b.default) - Number(a.default) || a.name.localeCompare(b.name));

  return (
    <MonitoringLayout current="rules"
      actions={can.act && !unavailable ? <button className="btn pri" onClick={() => setDialog({})}><Icon name="plus" />New rule</button> : undefined}>
      {rules.isPending && <p className="loading">Loading alert rules…</p>}
      {unavailable && (
        <div className="empty">
          <h2>Alert rules are not available yet</h2>
          <p>This console cannot reach its alerting service. Once it can, Kwerft's default rules are listed here, ready to be sent to a channel or adjusted.</p>
        </div>
      )}
      {rules.isError && !unavailable && <div className="banner bad" role="alert"><Icon name="alert" /><span>{errText(rules.error)}</span></div>}
      {rules.isSuccess && list.length === 0 && (
        <div className="empty">
          <h2>No alert rules</h2>
          <p>Kwerft creates its default rules within a minute of starting. You can also add your own: pick a condition, where it applies and who hears about it.</p>
          {can.act && <div className="acts"><button className="btn pri" onClick={() => setDialog({})}><Icon name="plus" />New rule</button></div>}
        </div>
      )}
      {list.length > 0 && (
        <>
          {!can.act && session.data && (
            <div className="banner info"><Icon name="shield" /><span>Your role can see alert rules but not change them.</span></div>
          )}
          {channels.isSuccess && channels.data.length === 0 && (
            <div className="banner info" role="status">
              <Icon name="alert" />
              <span>Alerts only show in the console until there is a channel. Add Slack, email, a webhook or ntfy to be notified.</span>
              <Link to="/monitoring/channels" className="btn sm">Channels</Link>
            </div>
          )}
          <div className="card scroll-x">
            <table className="t">
              <thead>
                <tr>
                  <th>Rule</th><th>Condition</th><th>Applies to</th><th>Severity</th><th>Notify</th><th className="num">Firing</th><th>On</th><th><span className="sr">Actions</span></th>
                </tr>
              </thead>
              <tbody>
                {list.map((r) => (
                  <RuleRow key={`${r.cluster ?? ""}/${r.name}`} r={r} channels={channels.data} canEdit={can.act && (r.condition !== "Custom" || can.admin)}
                    onEdit={() => setDialog({ edit: r })} onRemove={() => setDialog({ remove: r })} />
                ))}
              </tbody>
            </table>
          </div>
          <p className="dim note">
            Default rules come with Kwerft: change or disable them as you like; if one is deleted, it comes back with its defaults. Rules without a channel only show in the console.
          </p>
        </>
      )}
      {dialog && "remove" in dialog && <DeleteRuleDialog r={dialog.remove} onClose={() => setDialog(undefined)} />}
      {dialog && !("remove" in dialog) && (
        <RuleDialog edit={dialog.edit} admin={can.admin} readOnly={!can.act || (dialog.edit?.condition === "Custom" && !can.admin)}
          channels={channels.data} onClose={() => setDialog(undefined)} />
      )}
    </MonitoringLayout>
  );
}

/** The rule as the API takes it back: everything but its status. */
function inputOf(r: Rule): RuleInput {
  return {
    name: r.name, condition: r.condition, threshold: r.threshold, window: r.window, for: r.for, expr: r.expr,
    scope: { projects: r.scope?.projects ?? [], apps: r.scope?.apps ?? [] }, severity: r.severity, channels: r.channels ?? [], disabled: r.disabled,
    cluster: r.cluster,
  };
}

function RuleRow({ r, channels, canEdit, onEdit, onRemove }: { r: Rule; channels?: Channel[]; canEdit: boolean; onEdit: () => void; onRemove: () => void }) {
  const queryClient = useQueryClient();
  const toggle = useMutation({
    mutationFn: (disabled: boolean) => alertsApi.updateRule({ ...inputOf(r), disabled }),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: alertKeys.rules }),
  });
  const disabled = toggle.isPending ? !!toggle.variables : r.disabled;
  return (
    <tr className="click" onClick={onEdit}>
      <td className="nm">
        <span className="rule-name">
          {r.name}
          {r.default && <span className="tag" title="Comes with Kwerft; it returns with its defaults if deleted">default</span>}
          <ClusterBadge cluster={r.cluster} />
          {!r.ready && !r.disabled && <span className="pill warn" title={r.message}>{r.message ? "Not active" : "Setting up"}</span>}
        </span>
        {!r.ready && !r.disabled && r.message && <span className="sub">{r.message}</span>}
      </td>
      <td className="cond">{describeCondition(r)}{r.condition === "Custom" && r.expr && <span className="sub mono ell" title={r.expr}>{r.expr}</span>}</td>
      <td>{describeScope(r)}</td>
      <td><span className={`pill nodot ${severityTone[r.severity] ?? "mute"}`}>{r.severity}</span></td>
      <td className={`notify${r.channels?.length ? "" : " dim"}`}>{describeChannels(r.channels ?? [], channels)}</td>
      <td className="num"><span className={`firing${r.firing > 0 ? " on" : ""}`}>{r.disabled ? "—" : r.firing}</span></td>
      <td onClick={(e) => e.stopPropagation()}>
        <button type="button" role="switch" aria-checked={!disabled} className={`toggle ${disabled ? "" : "on"}`} disabled={!canEdit || toggle.isPending}
          aria-label={`${r.name}: ${disabled ? "disabled" : "enabled"}`}
          title={toggle.isError ? errText(toggle.error) : disabled ? "Enable: evaluate this rule again" : "Disable: keep the rule, stop evaluating it"}
          onClick={() => toggle.mutate(!disabled)}><i /></button>
        {toggle.isError && <span className="sr" role="alert">{errText(toggle.error)}</span>}
      </td>
      <td className="row-acts" onClick={(e) => e.stopPropagation()}>
        {canEdit && (
          <button className="btn sm danger" onClick={onRemove} aria-label={`Delete ${r.name}`} title="Delete"><Icon name="trash" /></button>
        )}
      </td>
    </tr>
  );
}

// ---- editor -------------------------------------------------------------------

type Form = {
  name: string;
  condition: AlertCondition;
  threshold: string;
  window: string;
  for: string;
  expr: string;
  projects: string[];
  apps: string[];
  severity: Severity;
  channels: string[];
  enabled: boolean;
};

function formOf(r?: Rule): Form {
  return {
    name: r?.name ?? "", condition: r?.condition ?? "CrashLooping",
    threshold: r?.threshold !== undefined ? String(r.threshold) : "", window: shortDuration(r?.window), for: shortDuration(r?.for),
    expr: r?.expr ?? "", projects: r?.scope?.projects ?? [], apps: r?.scope?.apps ?? [],
    severity: r?.severity ?? conditions.CrashLooping.severity, channels: r?.channels ?? [], enabled: !(r?.disabled ?? false),
  };
}

function ruleOf(f: Form): RuleInput {
  const info = conditions[f.condition];
  const d = (s: string) => {
    const secs = parseDuration(s);
    return secs ? goDuration(secs) : undefined;
  };
  return {
    name: f.name.trim(),
    condition: f.condition,
    threshold: info.threshold && f.threshold.trim() ? Number(f.threshold) : undefined,
    window: info.window ? d(f.window) : undefined,
    for: info.for ? d(f.for) : undefined,
    expr: f.condition === "Custom" ? f.expr.trim() : undefined,
    scope: { projects: info.scope === "none" ? [] : f.projects, apps: info.scope === "apps" ? f.apps : [] },
    severity: f.severity,
    channels: f.channels,
    disabled: !f.enabled,
  };
}

function problemOf(f: Form, creating: boolean): { field: string; message: string } | undefined {
  if (creating && !RULE_NAME_RE.test(f.name.trim())) return { field: "name", message: "Use lowercase letters, digits and dashes, like api-restarts." };
  if (creating && f.name.trim().length > 63) return { field: "name", message: "At most 63 characters." };
  const t = thresholdProblem(f.condition, f.threshold);
  if (t) return { field: "threshold", message: t };
  for (const k of ["window", "for"] as const) {
    const p = durationProblem(f[k]);
    if (p) return { field: k, message: p };
  }
  if (f.condition === "Custom" && !f.expr.trim()) return { field: "expr", message: "Enter a MetricsQL expression." };
  return undefined;
}

function RuleDialog({ edit, admin, readOnly, channels, onClose }: { edit?: Rule; admin: boolean; readOnly: boolean; channels?: Channel[]; onClose: () => void }) {
  const queryClient = useQueryClient();
  const projects = useQuery({ queryKey: ["projects"], queryFn: workloads.projects });
  const apps = useQuery({ queryKey: ["apps"], queryFn: () => workloads.apps() });
  const [f, setF] = useState(() => formOf(edit));
  const [error, setError] = useState<{ field?: string; message: string }>();
  const set = <K extends keyof Form>(k: K, v: Form[K]) => {
    setF((prev) => ({ ...prev, [k]: v }));
    setError((e) => (e?.field === k ? undefined : e));
  };
  const info = conditions[f.condition];
  const save = useMutation({
    mutationFn: () => (edit ? alertsApi.updateRule({ ...ruleOf(f), cluster: edit.cluster }) : alertsApi.createRule(ruleOf(f))),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: alertKeys.rules });
      onClose();
    },
    onError: (e) => setError(e instanceof ApiError ? { field: e.field, message: e.message } : { message: unreachable }),
  });
  const err = (field: string) => (error?.field === field ? error.message : undefined);
  const fieldKeys = ["name", "threshold", "window", "for", "expr"];
  const offered = conditionOrder.filter((c) => c !== "Custom" || admin);

  function submit() {
    if (readOnly) return onClose();
    const p = problemOf(f, !edit);
    if (p) return setError(p);
    setError(undefined);
    save.mutate();
  }

  // The preview reads the form as the server will: empty fields take the defaults.
  const preview = describeCondition({
    condition: f.condition,
    threshold: f.threshold.trim() && !thresholdProblem(f.condition, f.threshold) ? Number(f.threshold) : undefined,
    window: durationProblem(f.window) ? undefined : f.window,
    for: durationProblem(f.for) ? undefined : f.for,
  });
  const scopeText = describeScope({ condition: f.condition, scope: { projects: f.projects, apps: info.scope === "apps" ? f.apps : [] } });

  const title = readOnly ? edit?.name ?? "" : edit ? `Edit ${edit.name}` : "New alert rule";
  return (
    <Dialog wide title={title} onClose={onClose} onSubmit={submit}
      actions={readOnly ? <button className="btn pri">Close</button> : <>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={save.isPending}>{save.isPending ? "Saving…" : edit ? "Save" : "Create rule"}</button>
      </>}>
      {readOnly && (
        <p className="dim note">
          {edit?.condition === "Custom" ? "Only owners and admins change rules with a custom expression." : "Your role can see this rule but not change it."}
        </p>
      )}
      {edit?.default && !readOnly && <p className="dim note">A default rule: change it as you like. Deleted, it comes back with its defaults.</p>}
      <fieldset className="plain" disabled={readOnly}>
        {!edit && (
          <Field id="rule-name" label="Name" className="mono" value={f.name} onChange={(e) => set("name", e.target.value.toLowerCase())} autoComplete="off" spellCheck={false}
            placeholder="api-restarts" error={err("name")} hint="Shown on alerts and notifications. It cannot change later." />
        )}
        <div className="field">
          <label htmlFor="rule-condition">Condition</label>
          {edit ? (
            <p className="note" style={{ margin: 0 }}><b>{info.label}</b> · <span className="dim">{info.hint}</span></p>
          ) : (
            <>
              <select id="rule-condition" className="input" value={f.condition}
                onChange={(e) => {
                  const c = e.target.value as AlertCondition;
                  setF((x) => ({ ...x, condition: c, severity: conditions[c].severity, threshold: "", window: "", for: "" }));
                  setError(undefined);
                }}>
                {offered.map((c) => <option key={c} value={c}>{conditions[c].label}: {lcFirst(conditions[c].hint)}</option>)}
              </select>
            </>
          )}
        </div>

        {(info.threshold || info.window || info.for) && (
          <div className="fields">
            {info.threshold && (
              <div className="field">
                <label htmlFor="rule-threshold">{info.threshold.label}</label>
                <div className="with-unit">
                  <input id="rule-threshold" className="input" inputMode="numeric" value={f.threshold} onChange={(e) => set("threshold", e.target.value.trim())}
                    placeholder={String(info.threshold.default)} aria-invalid={!!err("threshold")} aria-describedby="rule-threshold-note" autoComplete="off" />
                  <span className="unit">{info.threshold.suffix}</span>
                </div>
                {err("threshold") ? <span id="rule-threshold-note" className="field-error" role="alert">{err("threshold")}</span>
                  : <span id="rule-threshold-note" className="hint">Empty: {info.threshold.default} {info.threshold.suffix}</span>}
              </div>
            )}
            {info.window && (
              <Field id="rule-window" label={info.window.label} className="mono" value={f.window} onChange={(e) => set("window", e.target.value)}
                placeholder={info.window.default || "not checked"} autoComplete="off" spellCheck={false} error={err("window")}
                hint={info.window.default ? `Like 30m, 2h or 7d. Empty: ${info.window.default}` : "Like 2h or 1d. Empty: only failed runs count."} />
            )}
            {info.for && (
              <Field id="rule-for" label={info.for.label} className="mono" value={f.for} onChange={(e) => set("for", e.target.value)}
                placeholder={info.for.default || "at once"} autoComplete="off" spellCheck={false} error={err("for")}
                hint={info.for.default ? `How long it must hold first. Empty: ${info.for.default}` : "How long it must hold first. Empty: fires at once."} />
            )}
          </div>
        )}

        {f.condition === "Custom" && (admin && !readOnly ? (
          <div className="field">
            <label htmlFor="rule-expr">Expression (MetricsQL)</label>
            <textarea id="rule-expr" className="input mono" rows={4} value={f.expr} onChange={(e) => set("expr", e.target.value)} spellCheck={false} autoComplete="off"
              placeholder={'sum by (namespace, app) (rate(kwerft:http_requests:rate5m{code_class="5xx"}[5m])) > 1'} aria-invalid={!!err("expr")} aria-describedby="rule-expr-note" />
            {err("expr") ? <span id="rule-expr-note" className="field-error" role="alert">{err("expr")}</span>
              : <span id="rule-expr-note" className="hint">Each series it returns is one alert. Keep the <code>namespace</code> and <code>app</code> labels so alerts link to the app.</span>}
          </div>
        ) : (
          <div className="field">
            <label>Expression (MetricsQL)</label>
            <pre className="codebox expr">{edit?.expr || edit?.effectiveExpr || "—"}</pre>
          </div>
        ))}

        <div className="preview" aria-live="polite">
          <span className="k">Fires when</span>
          <span><b>{preview}</b> · {scopeText.charAt(0).toLowerCase() + scopeText.slice(1)}</span>
        </div>

        {info.scope !== "none" && (
          <ScopePicker kind={info.scope} projects={projects.data?.map((p) => p.name) ?? []} apps={apps.data?.map((a) => `${a.project}/${a.name}`) ?? []}
            selectedProjects={f.projects} selectedApps={f.apps} everything={info.everything}
            onChange={(p, a) => setF((x) => ({ ...x, projects: p, apps: a }))} />
        )}

        <div className="field">
          <label id="rule-sev">Severity</label>
          <div className="seg" role="group" aria-labelledby="rule-sev">
            {severities.map((s) => <button type="button" key={s} aria-pressed={f.severity === s} onClick={() => set("severity", s)}>{s}</button>)}
          </div>
          <span className="hint">Critical: someone should look now. Warning: soon. Info: good to know.</span>
        </div>

        <div className="field">
          <label id="rule-channels">Notify</label>
          <div className="scope-pick" role="group" aria-labelledby="rule-channels">
            {(channels ?? []).map((c) => (
              <label key={c.name} className="check">
                <input type="checkbox" checked={f.channels.includes(c.name)}
                  onChange={(e) => set("channels", e.target.checked ? [...f.channels, c.name] : f.channels.filter((x) => x !== c.name))} />
                {c.name} <span className="dim">· {c.type}</span>
              </label>
            ))}
            {f.channels.filter((n) => !channels?.some((c) => c.name === n)).map((n) => (
              <label key={n} className="check"><input type="checkbox" checked onChange={() => set("channels", f.channels.filter((x) => x !== n))} />{n} <span className="dim">· missing</span></label>
            ))}
          </div>
          <span className="hint">
            {channels?.length ? "None: the alert only shows in the console." : <>No channels yet{admin ? <>; add one under <Link to="/monitoring/channels">Channels</Link></> : ": owners and admins add them under Channels"}. Until then alerts only show in the console.</>}
          </span>
        </div>

        <label className="check">
          <input type="checkbox" checked={f.enabled} onChange={(e) => set("enabled", e.target.checked)} />
          Enabled
        </label>

        {edit && edit.condition !== "Custom" && edit.effectiveExpr && (
          <details className="yaml">
            <summary>Expression Kwerft evaluates</summary>
            <pre className="codebox expr">{edit.effectiveExpr}</pre>
          </details>
        )}
      </fieldset>
      {error && !fieldKeys.includes(error.field ?? "") && <p className="form-error" role="alert">{error.field ? <><code>{error.field}</code>: </> : null}{error.message}</p>}
    </Dialog>
  );
}

function ScopePicker({ kind, projects, apps, selectedProjects, selectedApps, everything, onChange }: {
  kind: "apps" | "projects"; projects: string[]; apps: string[]; selectedProjects: string[]; selectedApps: string[]; everything: string;
  onChange: (projects: string[], apps: string[]) => void;
}) {
  const all = selectedProjects.length === 0 && (kind === "projects" || selectedApps.length === 0);
  const flip = (list: string[], x: string, on: boolean) => (on ? [...list, x] : list.filter((y) => y !== x));
  const knownProjects = [...projects, ...selectedProjects.filter((p) => !projects.includes(p))];
  const knownApps = [...apps, ...selectedApps.filter((a) => !apps.includes(a))];
  return (
    <div className="field">
      <label id="rule-scope">Applies to</label>
      <div className="scope-pick" role="group" aria-labelledby="rule-scope">
        <label className="check"><input type="checkbox" checked={all} onChange={() => onChange([], [])} />{everything}</label>
        {knownProjects.map((p) => (
          <label key={`p:${p}`} className="check">
            <input type="checkbox" checked={selectedProjects.includes(p)} onChange={(e) => onChange(flip(selectedProjects, p, e.target.checked), selectedApps)} />
            {p} <span className="dim">· project</span>
          </label>
        ))}
        {kind === "apps" && knownApps.map((a) => (
          <label key={`a:${a}`} className="check">
            <input type="checkbox" checked={selectedApps.includes(a)} onChange={(e) => onChange(selectedProjects, flip(selectedApps, a, e.target.checked))} />
            <span className="mono">{a}</span>
          </label>
        ))}
      </div>
      <span className="hint">{kind === "apps" ? "Pick projects, single apps, or both; the rule watches all of them." : "Pick projects to narrow the rule."}</span>
    </div>
  );
}

// ---- deletion -----------------------------------------------------------------

function DeleteRuleDialog({ r, onClose }: { r: Rule; onClose: () => void }) {
  const queryClient = useQueryClient();
  const done = () => {
    void queryClient.invalidateQueries({ queryKey: alertKeys.rules });
    onClose();
  };
  const del = useMutation({ mutationFn: () => alertsApi.deleteRule(r.name, r.cluster), onSuccess: done });
  const disable = useMutation({ mutationFn: () => alertsApi.updateRule({ ...inputOf(r), disabled: true }), onSuccess: done });
  const error = del.error ?? disable.error;
  return (
    <Dialog title={`Delete ${r.name}?`} onClose={onClose} onSubmit={() => del.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        {r.default && !r.disabled && <button type="button" className="btn" disabled={disable.isPending} onClick={() => disable.mutate()}>{disable.isPending ? "Disabling…" : "Disable instead"}</button>}
        <button className="btn pri danger" disabled={del.isPending}>{del.isPending ? "Deleting…" : r.default ? "Delete anyway" : "Delete rule"}</button>
      </>}>
      {r.default ? (
        <p style={{ margin: 0 }}>
          <b>{r.name}</b> is one of Kwerft's default rules: deleted, it comes back within a minute with its default settings, and your changes are lost.
          To stop it, disable it instead.
        </p>
      ) : (
        <p style={{ margin: 0 }}>Kwerft stops evaluating it{r.firing > 0 ? `; its ${r.firing} firing ${r.firing === 1 ? "alert resolves" : "alerts resolve"}` : ""}. This cannot be undone.</p>
      )}
      {error && <p className="form-error" role="alert">{errText(error)}</p>}
    </Dialog>
  );
}
