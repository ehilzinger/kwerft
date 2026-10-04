import { useState, type MouseEvent, type ReactNode } from "react";
import { Link, useRouter } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../api";
import {
  alertAbilities, alertKeys, alertLogsLink, alertingUnavailable, alertsApi, sortAlerts, severityTone,
  type Alert, type AlertState,
} from "../alerts";
import { Dialog } from "../components/Dialog";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { ClusterBadge } from "../components/ClusterUI";
import { duration, when } from "../jobs";
import { ago } from "../workloads";
import { MonitoringLayout } from "./Monitoring";
import "../styles/monitoring.css";

const unreachable = "The console could not be reached. Check your connection and try again.";
export const errText = (e: unknown) => (e instanceof ApiError ? e.message : unreachable);

const states: { id: AlertState; label: string }[] = [
  { id: "firing", label: "Firing" },
  { id: "silenced", label: "Silenced" },
  { id: "resolved", label: "Resolved" },
];

/** Firing alerts, shared by this tab, the Overview and the sidebar badge. */
export function useFiringAlerts() {
  return useQuery({ queryKey: alertKeys.alerts("firing"), queryFn: () => alertsApi.alerts("firing"), refetchInterval: 15000, retry: false });
}

// Monitoring › Alerts: what fires now, what is silenced and what resolved in
// the last 24 hours, with the way to the logs and a silence for each.
export function MonitoringAlerts() {
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const can = alertAbilities(session.data?.role);
  const [state, setState] = useState<AlertState>("firing");
  const firing = useFiringAlerts();
  const silenced = useQuery({ queryKey: alertKeys.alerts("silenced"), queryFn: () => alertsApi.alerts("silenced"), refetchInterval: 30000, retry: false });
  const resolved = useQuery({ queryKey: alertKeys.alerts("resolved"), queryFn: () => alertsApi.alerts("resolved"), refetchInterval: 60000, retry: false });
  const byState = { firing, silenced, resolved };
  const current = byState[state];
  const [silencing, setSilencing] = useState<{ alert: Alert; hours: number }>();

  if (firing.isError && alertingUnavailable(firing.error)) {
    return (
      <MonitoringLayout current="alerts">
        <div className="empty">
          <h2>Alerting is not available yet</h2>
          <p>
            This console cannot reach its alerting service. Once it can, alerts from the default rules show up here, for example a crash-looping app or a volume
            that is almost full. The Overview still lists what Kwerft knows without it.
          </p>
          <div className="acts"><Link to="/" className="btn">Go to the Overview</Link></div>
        </div>
      </MonitoringLayout>
    );
  }

  const list = sortAlerts(current.data ?? []);
  return (
    <MonitoringLayout current="alerts">
      <div className="card">
        <div className="ch-h alert-head">
          <h3>Alerts</h3>
          <div className="seg" role="group" aria-label="Alert state">
            {states.map((s) => (
              <button key={s.id} type="button" aria-pressed={state === s.id} onClick={() => setState(s.id)}>
                {s.label}{byState[s.id].data && <span className="n">{byState[s.id].data!.length}</span>}
              </button>
            ))}
          </div>
        </div>
        {current.isPending && <p className="loading pad">Loading alerts…</p>}
        {current.isError && <div className="bd"><div className="banner bad" role="alert"><Icon name="alert" /><span>{errText(current.error)}</span></div></div>}
        {current.isSuccess && list.length === 0 && <div className="bd"><EmptyState state={state} /></div>}
        {list.length > 0 && (
          <div className="list" aria-live="polite">
            {list.map((a) => (
              <AlertRow key={`${a.fingerprint}-${a.startsAt}`} a={a} canAct={can.act} onSilence={(hours) => setSilencing({ alert: a, hours })} />
            ))}
          </div>
        )}
      </div>
      {state === "resolved" && list.length > 0 && <p className="dim note">Resolved alerts are kept for 24 hours.</p>}
      {silencing && <SilenceDialog alert={silencing.alert} hours={silencing.hours} onClose={() => setSilencing(undefined)} />}
    </MonitoringLayout>
  );
}

function EmptyState({ state }: { state: AlertState }) {
  if (state === "firing") {
    return (
      <div className="empty">
        <h2>Nothing is firing</h2>
        <p>
          Alerts show up here when a rule's condition holds, for example an app that keeps crashing or a certificate that does not renew.
          Kwerft starts with a set of default rules; add a channel to hear about alerts in Slack, by email or on your phone.
        </p>
        <div className="acts">
          <Link to="/monitoring/rules" className="btn">Alert rules</Link>
          <Link to="/monitoring/channels" className="btn">Channels</Link>
        </div>
      </div>
    );
  }
  if (state === "silenced") {
    return (
      <div className="empty">
        <h2>No silenced alerts</h2>
        <p>Silence a firing alert for an hour or a day while you work on it. Notifications pause; if it still fires afterwards, it comes back.</p>
      </div>
    );
  }
  return (
    <div className="empty">
      <h2>Nothing resolved in the last 24 hours</h2>
      <p>Alerts that stopped firing are listed here for a day, with how long they lasted.</p>
    </div>
  );
}

function AlertRow({ a, canAct, onSilence }: { a: Alert; canAct: boolean; onSilence: (hours: number) => void }) {
  const queryClient = useQueryClient();
  const logs = alertLogsLink(a, window.location.origin);
  const unsilence = useMutation({
    mutationFn: async () => {
      for (const id of a.silencedBy ?? []) await alertsApi.unsilence(id, a.cluster);
    },
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["alerts"] }),
  });
  const tone = severityTone[a.severity] ?? "info";
  const where = a.project && a.app ? `${a.project}/${a.app}` : a.project ?? a.labels?.node ?? a.labels?.hostname;
  const started = new Date(a.startsAt).toLocaleString();
  return (
    <div className="li">
      <span className={`ico ${a.state === "resolved" ? "ok" : tone}`}><Icon name="alert" /></span>
      <div className="grow">
        <b>{a.rule}</b>{where && <> · {where}</>}<ClusterBadge cluster={a.cluster} />
        {(a.summary || a.description) && <p>{a.summary || a.description}</p>}
        {a.summary && a.description && a.description !== a.summary && <p>{a.description}</p>}
        <div className="meta">
          {a.state === "firing" && <span title={started}>Started {ago(a.startsAt)}</span>}
          {a.state === "silenced" && <span title={started}>Started {ago(a.startsAt)} · silenced{a.silencedUntil ? `, the silence ends ${when(a.silencedUntil)}` : ""}</span>}
          {a.state === "resolved" && <span title={started}>Resolved {ago(a.endsAt)}{a.endsAt ? ` after ${duration(a.startsAt, a.endsAt)}` : ""}</span>}
        </div>
        <div className="acts">
          {logs && <RouterLink href={logs.href} external={logs.external} className="btn sm">Logs</RouterLink>}
          {a.project && a.app && <Link to="/apps/$project/$name" params={{ project: a.project, name: a.app }} className="btn sm">Open app</Link>}
          {a.state === "firing" && canAct && (
            <>
              <button type="button" className="btn sm" onClick={() => onSilence(1)}>Silence 1 h</button>
              <button type="button" className="btn sm" onClick={() => onSilence(24)}>Silence 24 h</button>
            </>
          )}
          {a.state === "silenced" && canAct && (a.silencedBy?.length ?? 0) > 0 && (
            <button type="button" className="btn sm" disabled={unsilence.isPending} onClick={() => unsilence.mutate()}>{unsilence.isPending ? "Unsilencing…" : "Unsilence"}</button>
          )}
        </div>
        {unsilence.isError && <p className="form-error" role="alert">{errText(unsilence.error)}</p>}
      </div>
      <div className="end"><span className={`pill nodot ${tone}`}>{a.severity}</span></div>
    </div>
  );
}

function SilenceDialog({ alert, hours: initial, onClose }: { alert: Alert; hours: number; onClose: () => void }) {
  const queryClient = useQueryClient();
  const [hours, setHours] = useState(initial);
  const [comment, setComment] = useState("");
  const silence = useMutation({
    mutationFn: () => alertsApi.silence({ fingerprint: alert.fingerprint, duration: `${hours}h`, comment: comment.trim() || "Silenced from the Kwerft console", cluster: alert.cluster }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["alerts"] });
      onClose();
    },
  });
  const where = alert.project && alert.app ? ` on ${alert.project}/${alert.app}` : "";
  return (
    <Dialog title={`Silence ${alert.rule}${where}?`} onClose={onClose} onSubmit={() => silence.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={silence.isPending}>{silence.isPending ? "Silencing…" : `Silence for ${hours === 1 ? "1 hour" : `${hours} hours`}`}</button>
      </>}>
      <p className="note" style={{ margin: 0 }}>
        Channels stay quiet about this alert; the silence ends {when(new Date(Date.now() + hours * 3600000).toISOString())}. The alert stays listed under Silenced, and notifies again if it still fires then.
      </p>
      <div className="field">
        <label id="silence-for">For</label>
        <div className="seg" role="group" aria-labelledby="silence-for">
          {[1, 4, 24].map((h) => <button type="button" key={h} aria-pressed={hours === h} onClick={() => setHours(h)}>{h} h</button>)}
        </div>
      </div>
      <Field id="silence-comment" label="Why (optional)" value={comment} onChange={(e) => setComment(e.target.value)} placeholder="Working on it: rolling back the last deploy"
        autoFocus autoComplete="off" maxLength={200} error={silence.isError ? errText(silence.error) : undefined} hint="Shown to others next to the silence." />
    </Dialog>
  );
}

/**
 * A link to a path that may carry search params the router does not type
 * (?tab=logs): an ordinary <a href>, navigated by the router on a plain click.
 */
export function RouterLink({ href, external, className, children }: { href: string; external?: boolean; className?: string; children: ReactNode }) {
  const router = useRouter();
  if (external) return <a href={href} className={className} target="_blank" rel="noreferrer">{children}</a>;
  const onClick = (e: MouseEvent<HTMLAnchorElement>) => {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    void router.navigate({ href });
  };
  return <a href={href} className={className} onClick={onClick}>{children}</a>;
}
