import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../api";
import {
  alertAbilities, alertKeys, alertingUnavailable, alertsApi, channelForm, channelInput, channelProblem, channelTarget, channelTypeLabel, channelTypes,
  type Channel, type ChannelForm, type TestResult,
} from "../alerts";
import { Dialog } from "../components/Dialog";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { ago } from "../workloads";
import { MonitoringLayout } from "./Monitoring";
import { errText } from "./MonitoringAlerts";
import "../styles/monitoring.css";

const unreachable = "The console could not be reached. Check your connection and try again.";
const stored = "•••••••••••• stored — enter a new one to replace it";

// Monitoring › Channels: where alerts go. Owners and admins add and change
// them; secrets (webhook URLs, passwords, tokens) are write-only.
export function MonitoringChannels() {
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const can = alertAbilities(session.data?.role);
  const channels = useQuery({
    queryKey: alertKeys.channels, queryFn: alertsApi.channels, retry: false,
    refetchInterval: (q) => (q.state.data?.some((c) => !c.ready) ? 5000 : 30000),
  });
  const rules = useQuery({ queryKey: alertKeys.rules, queryFn: alertsApi.rules, retry: false });
  const [dialog, setDialog] = useState<{ edit?: Channel } | { remove: Channel }>();
  const unavailable = channels.isError && alertingUnavailable(channels.error);
  const list = channels.data ?? [];

  return (
    <MonitoringLayout current="channels"
      actions={can.admin && !unavailable ? <button className="btn pri" onClick={() => setDialog({})}><Icon name="plus" />Add channel</button> : undefined}>
      {channels.isPending && <p className="loading">Loading channels…</p>}
      {unavailable && (
        <div className="empty">
          <h2>Channels are not available yet</h2>
          <p>This console cannot reach its alerting service. Once it can, add Slack, email, a webhook or ntfy here to hear about alerts.</p>
        </div>
      )}
      {channels.isError && !unavailable && <div className="banner bad" role="alert"><Icon name="alert" /><span>{errText(channels.error)}</span></div>}
      {!can.admin && session.data && !unavailable && (
        <div className="banner info"><Icon name="shield" /><span>Only owners and admins add and change channels. You can see them here.</span></div>
      )}
      {channels.isSuccess && list.length === 0 && (
        <div className="empty">
          <h2>No channels yet</h2>
          <p>
            Until there is one, alerts only show in the console. Add a Slack incoming webhook, an email address, a webhook of your own or an ntfy topic for your phone,
            then pick it in the alert rules that should notify.
          </p>
          {can.admin && <div className="acts"><button className="btn pri" onClick={() => setDialog({})}><Icon name="plus" />Add channel</button></div>}
        </div>
      )}
      {list.length > 0 && (
        <div className="card chans">
          {list.map((c) => (
            <ChannelRow key={c.name} c={c} canEdit={can.admin} usedBy={usersOf(c.name, rules.data)}
              onEdit={() => setDialog({ edit: c })} onRemove={() => setDialog({ remove: c })} />
          ))}
        </div>
      )}
      {list.length > 0 && <p className="dim note">A channel notifies for the alert rules that name it. Secrets are stored in the cluster and never shown again.</p>}
      {dialog && "remove" in dialog && <DeleteChannelDialog c={dialog.remove} usedBy={usersOf(dialog.remove.name, rules.data)} onClose={() => setDialog(undefined)} />}
      {dialog && !("remove" in dialog) && <ChannelDialog edit={dialog.edit} onClose={() => setDialog(undefined)} />}
    </MonitoringLayout>
  );
}

const usersOf = (name: string, rules?: { name: string; channels: string[] }[]) => (rules ?? []).filter((r) => r.channels?.includes(name)).map((r) => r.name);

function ChannelRow({ c, canEdit, usedBy, onEdit, onRemove }: { c: Channel; canEdit: boolean; usedBy: string[]; onEdit: () => void; onRemove: () => void }) {
  const queryClient = useQueryClient();
  const [result, setResult] = useState<TestResult>();
  const test = useMutation({
    mutationFn: () => alertsApi.testChannel(c.name),
    onMutate: () => setResult(undefined),
    onSuccess: (r) => setResult(r),
    onError: (e) => setResult({ ok: false, message: errText(e) }),
    onSettled: () => void queryClient.invalidateQueries({ queryKey: alertKeys.channels }),
  });
  return (
    <div className="chan">
      <div>
        <div className="head">
          <b>{c.name}</b>
          <span className="tag">{channelTypeLabel(c.type)}</span>
          {c.ready ? <span className="pill ok">Ready</span> : <span className="pill bad" title={c.message}>Not ready</span>}
        </div>
        <p className="meta">
          {c.type === "email" ? "To " : c.type === "slack" ? "Posts to " : c.type === "ntfy" ? "Topic " : ""}<span className="mono">{channelTarget(c)}</span>
          {c.type === "email" && c.email?.smtpHost && <> · via <span className="mono">{c.email.smtpHost}</span></>}
          {" · "}{c.sendResolved ? "also when resolved" : "firing only"}
          {" · "}{usedBy.length ? `used by ${usedBy.join(", ")}` : "no rule uses it yet"}
        </p>
        {!c.ready && c.message && <p className="meta form-error">{c.message}</p>}
        {result ? (
          <p className={`meta ${result.ok ? "ok-text" : "form-error"}`} role="status">
            {result.ok ? `✓ ${result.message || "Test notification sent."}` : `The test failed: ${result.message || "no details"}`}
          </p>
        ) : (
          <p className="meta">
            {c.lastTest ? (c.lastTestError
              ? <span className="warn-text">Last test {ago(c.lastTest)}: failed, {c.lastTestError}</span>
              : <>Last test {ago(c.lastTest)}: delivered.</>) : "Not tested yet."}
            {!c.secretSet && c.type !== "email" && c.type !== "ntfy" && <span className="warn-text"> No {c.type === "slack" ? "webhook URL" : "URL"} stored.</span>}
          </p>
        )}
      </div>
      {canEdit && (
        <div className="acts">
          <button className="btn sm" onClick={() => test.mutate()} disabled={test.isPending}>{test.isPending ? "Sending…" : "Send test"}</button>
          <button className="btn sm" onClick={onEdit}>Edit</button>
          <button className="btn sm danger" onClick={onRemove} aria-label={`Delete ${c.name}`}><Icon name="trash" />Delete</button>
        </div>
      )}
    </div>
  );
}

// ---- add and edit -------------------------------------------------------------

function ChannelDialog({ edit, onClose }: { edit?: Channel; onClose: () => void }) {
  const queryClient = useQueryClient();
  const [f, setF] = useState<ChannelForm>(() => channelForm(edit));
  const [error, setError] = useState<{ field?: string; message: string }>();
  const set = <K extends keyof ChannelForm>(k: K, v: ChannelForm[K], field: string = k) => {
    setF((prev) => ({ ...prev, [k]: v }));
    setError((e) => (e?.field === field ? undefined : e));
  };
  const secretStored = !!edit && edit.secretSet && edit.type === f.type;
  const save = useMutation({
    mutationFn: () => (edit ? alertsApi.updateChannel(channelInput(f)) : alertsApi.createChannel(channelInput(f))),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: alertKeys.channels });
      onClose();
    },
    onError: (e) => setError(e instanceof ApiError ? { field: e.field, message: e.message } : { message: unreachable }),
  });
  const err = (field: string) => (error?.field === field ? error.message : undefined);
  const fieldKeys = ["name", "url", "slack.channel", "email.to", "email.from", "email.smtpHost", "email.username", "password", "ntfy.server", "ntfy.topic", "token"];

  function submit() {
    const p = channelProblem(channelInput(f), secretStored);
    if (p) return setError(p);
    setError(undefined);
    save.mutate();
  }

  return (
    <Dialog wide title={edit ? `Edit ${edit.name}` : "Add a channel"} onClose={onClose} onSubmit={submit}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={save.isPending}>{save.isPending ? "Saving…" : edit ? "Save" : "Add channel"}</button>
      </>}>
      {!edit && (
        <div className="field">
          <label id="ch-type">Type</label>
          <div className="seg" role="group" aria-labelledby="ch-type">
            {channelTypes.map((t) => <button type="button" key={t.id} aria-pressed={f.type === t.id} onClick={() => { set("type", t.id); setError(undefined); }}>{t.label}</button>)}
          </div>
        </div>
      )}
      <Field id="ch-name" label="Name" className="mono" value={f.name} onChange={(e) => set("name", e.target.value.toLowerCase())} disabled={!!edit}
        placeholder={f.type === "slack" ? "ops-slack" : f.type === "email" ? "ops-email" : f.type === "ntfy" ? "on-call-phone" : "pager"} autoComplete="off" spellCheck={false}
        error={err("name")} hint={edit ? `${channelTypeLabel(edit.type)} channel. The name cannot change: rules refer to it.` : "Rules refer to it by this name."} />

      {f.type === "slack" && (
        <>
          <Field id="ch-url" label="Incoming webhook URL" className="mono" type="password" value={f.url} onChange={(e) => set("url", e.target.value)}
            placeholder={secretStored ? stored : "https://hooks.slack.com/services/…"} autoComplete="off" spellCheck={false} error={err("url")}
            hint="In Slack: create an app → Incoming Webhooks → Add New Webhook to Workspace, and pick the channel. Kwerft stores it and never shows it again." />
          <Field id="ch-slack-channel" label="Channel (optional)" className="mono" value={f.slackChannel} onChange={(e) => set("slackChannel", e.target.value, "slack.channel")}
            placeholder="#ops-alerts" autoComplete="off" spellCheck={false} error={err("slack.channel")} hint="Empty: the channel the webhook was made for." />
        </>
      )}
      {f.type === "webhook" && (
        <Field id="ch-url" label="URL" className="mono" type="password" value={f.url} onChange={(e) => set("url", e.target.value)}
          placeholder={secretStored ? stored : "https://hooks.example.com/kwerft?token=…"} autoComplete="off" spellCheck={false} error={err("url")}
          hint="Kwerft POSTs Alertmanager's JSON to it for every notification. Stored as a secret, since URLs often carry a token." />
      )}
      {f.type === "email" && (
        <>
          <Field id="ch-to" label="To" value={f.to} onChange={(e) => set("to", e.target.value, "email.to")} placeholder="ops@example.com, oncall@example.com"
            autoComplete="off" spellCheck={false} error={err("email.to")} hint="One or more addresses, separated by commas." />
          <div className="fields">
            <Field id="ch-from" label="From" value={f.from} onChange={(e) => set("from", e.target.value, "email.from")} placeholder="alerts@example.com"
              autoComplete="off" spellCheck={false} error={err("email.from")} />
            <Field id="ch-smtp" label="SMTP server" className="mono" value={f.smtpHost} onChange={(e) => set("smtpHost", e.target.value, "email.smtpHost")}
              placeholder="smtp.example.com:587" autoComplete="off" spellCheck={false} error={err("email.smtpHost")} hint="Host and port; STARTTLS is required." />
            <Field id="ch-user" label="Username (optional)" value={f.username} onChange={(e) => set("username", e.target.value, "email.username")}
              autoComplete="off" spellCheck={false} error={err("email.username")} />
            <Field id="ch-password" label="Password (optional)" type="password" value={f.password} onChange={(e) => set("password", e.target.value)}
              placeholder={secretStored ? stored : undefined} autoComplete="new-password" error={err("password")} />
          </div>
        </>
      )}
      {f.type === "ntfy" && (
        <>
          <div className="fields">
            <Field id="ch-server" label="Server" className="mono" value={f.server} onChange={(e) => set("server", e.target.value, "ntfy.server")}
              placeholder="https://ntfy.sh" autoComplete="off" spellCheck={false} error={err("ntfy.server")} hint="ntfy.sh or your own server." />
            <Field id="ch-topic" label="Topic" className="mono" value={f.topic} onChange={(e) => set("topic", e.target.value, "ntfy.topic")}
              placeholder="acme-alerts-7f3k" autoComplete="off" spellCheck={false} error={err("ntfy.topic")}
              hint="On ntfy.sh anyone who knows the topic can read it: pick one that is hard to guess." />
          </div>
          <Field id="ch-token" label="Access token (optional)" className="mono" type="password" value={f.token} onChange={(e) => set("token", e.target.value)}
            placeholder={secretStored ? stored : "tk_…"} autoComplete="off" spellCheck={false} error={err("token")} hint="For a protected topic." />
        </>
      )}
      <label className="check">
        <input type="checkbox" checked={f.sendResolved} onChange={(e) => set("sendResolved", e.target.checked)} />
        Also notify when an alert is resolved
      </label>
      {error && !fieldKeys.includes(error.field ?? "") && <p className="form-error" role="alert">{error.field ? <><code>{error.field}</code>: </> : null}{error.message}</p>}
    </Dialog>
  );
}

function DeleteChannelDialog({ c, usedBy, onClose }: { c: Channel; usedBy: string[]; onClose: () => void }) {
  const queryClient = useQueryClient();
  const del = useMutation({
    mutationFn: () => alertsApi.deleteChannel(c.name),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: alertKeys.channels });
      void queryClient.invalidateQueries({ queryKey: alertKeys.rules });
      onClose();
    },
  });
  return (
    <Dialog title={`Delete ${c.name}?`} onClose={onClose} onSubmit={() => del.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri danger" disabled={del.isPending}>{del.isPending ? "Deleting…" : "Delete channel"}</button>
      </>}>
      <p style={{ margin: 0 }}>Kwerft forgets the channel and its secret. Alerts keep showing in the console.</p>
      {usedBy.length > 0
        ? <p className="warn-text note" role="status">Used by {usedBy.join(", ")}: {usedBy.length === 1 ? "that rule stops" : "those rules stop"} notifying here.</p>
        : <p className="dim note">No rule uses it.</p>}
      {del.isError && <p className="form-error" role="alert">{errText(del.error)}</p>}
    </Dialog>
  );
}
