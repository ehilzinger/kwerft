// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError } from "../api";
import { Dialog } from "../components/Dialog";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import {
  cloudRuleText, countdown, describePorts, describeSources, firewallApi, firewallKey, nodeStateText, parseSourcesText, ruleProblem, sshCovers,
  type Firewall, type FirewallRule, type FirewallRuleInput,
} from "../firewall";
import { errorText } from "./Apps";
import { FirewallState } from "./SettingsHCloud";
import "../styles/firewall.css";

const unreachable = "The console could not be reached. Check your connection and try again.";

// Network › Server firewall: the installer's base rules (required), the
// ports opened here, and the change waiting for confirmation. The server
// refuses a change that would lock the signed-in user out of SSH; any change
// that takes something away rolls back on the nodes unless kept within 60 s.
export function ServerFirewall() {
  const fw = useQuery({
    queryKey: firewallKey, queryFn: firewallApi.get, retry: false,
    refetchInterval: (q) => (["pending", "applying"].includes(q.state.data?.state ?? "") ? 2000 : 15000),
  });
  const [dialog, setDialog] = useState<{ edit?: FirewallRule } | { remove: FirewallRule }>();

  if (fw.isPending) return <p className="loading">Loading the server firewall…</p>;
  if (fw.isError) {
    if (fw.error instanceof ApiError && fw.error.status === 403) {
      return (
        <div className="empty">
          <h2>Owners and admins manage the server firewall</h2>
          <p>The firewall on the servers themselves: SSH, HTTP(S), the cluster's own traffic and ports opened for services outside Kubernetes.</p>
        </div>
      );
    }
    return <div className="banner bad fw-banner" role="alert"><Icon name="alert" /><span>{errorText(fw.error)}</span></div>;
  }
  const data = fw.data;
  return (
    <>
      <StateBanner fw={data} />
      <div className="toolbar fw-toolbar">
        <ClientHint fw={data} />
        <button className="btn sm pri push" onClick={() => setDialog({})}><Icon name="plus" />Add rule</button>
      </div>
      <div className="card scroll-x">
        <table className="t fw-table fw-rules">
          <thead>
            <tr><th>Port</th><th>Protocol</th><th>Source</th><th>Purpose</th><th>Applies to</th><th>Status</th><th><span className="sr">Actions</span></th></tr>
          </thead>
          <tbody>
            {data.rules.map((r) => (
              <RuleRow key={r.name} r={r} fw={data} onEdit={() => setDialog({ edit: r })} onRemove={() => setDialog({ remove: r })} />
            ))}
          </tbody>
        </table>
      </div>
      <Nodes fw={data} />
      {data.cloud && (
        <div className="card">
          <div className="bd fw-cloud">
            <b>Hetzner Cloud Firewall</b> <FirewallState f={data.cloud} />
            <p className="dim note">
              The rules above, as the nodes confirmed them, also filter traffic before it reaches the Cloud servers (Settings › Hetzner Cloud API).
              Dedicated servers have the host firewall only.
            </p>
          </div>
        </div>
      )}
      <p className="dim note">
        Kwerft refuses to save a firewall change that would block SSH from the address you are using right now, and HTTP(S) to the
        console always stays open. A change that closes something is applied on the servers, then rolled back automatically unless you keep it
        within 60 seconds. Rescue: <code>install.sh --reset-firewall</code> from the Hetzner console.
      </p>
      {dialog && "remove" in dialog && <DeleteDialog r={dialog.remove} onClose={() => setDialog(undefined)} />}
      {dialog && !("remove" in dialog) && dialog.edit?.editable === "sources" && <SSHDialog r={dialog.edit} fw={data} onClose={() => setDialog(undefined)} />}
      {dialog && !("remove" in dialog) && dialog.edit?.editable !== "sources" && (
        <RuleDialog edit={dialog.edit} onClose={() => setDialog(undefined)} />
      )}
    </>
  );
}

// Seconds left, counting down between polls. Each poll restarts it from the
// server's count, so the browser's clock does not matter.
function useRemaining(fw: Firewall): number | undefined {
  const secs = fw.pending?.remainingSeconds;
  const [deadline, setDeadline] = useState<number>();
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    setDeadline(secs === undefined ? undefined : Date.now() + secs * 1000);
    setNow(Date.now());
  }, [secs]);
  useEffect(() => {
    if (deadline === undefined) return;
    const t = setInterval(() => setNow(Date.now()), 500);
    return () => clearInterval(t);
  }, [deadline]);
  return deadline === undefined ? secs : Math.max(0, (deadline - now) / 1000);
}

function StateBanner({ fw }: { fw: Firewall }) {
  const queryClient = useQueryClient();
  const remaining = useRemaining(fw);
  const done = () => void queryClient.invalidateQueries({ queryKey: firewallKey });
  const keep = useMutation({ mutationFn: () => firewallApi.confirm(fw.revision), onSettled: done });
  const rollback = useMutation({ mutationFn: firewallApi.rollback, onSettled: done });
  const retry = useMutation({ mutationFn: () => firewallApi.retry(fw.revision), onSettled: done });
  const error = keep.error ?? rollback.error ?? retry.error;
  const errorLine = error ? <p className="form-error" role="alert">{error instanceof ApiError ? error.message : unreachable}</p> : null;

  if (fw.state === "pending" && fw.pending) {
    return (
      <div className="banner warn fw-banner" role="status">
        <Icon name="shield" />
        <span>
          <b>A firewall change is applied on {fw.pending.nodes === 1 ? "1 node" : `${fw.pending.nodes} nodes`} and rolls back in {countdown(remaining ?? 0)}.</b>{" "}
          Check that you can still reach your servers (open a new SSH session), then keep it. If this page stops answering, it rolls back by itself.
        </span>
        <button className="btn sm pri" disabled={keep.isPending} onClick={() => keep.mutate()}>{keep.isPending ? "Keeping…" : "Keep these rules"}</button>
        <button className="btn sm" disabled={rollback.isPending || !fw.canRollBack} onClick={() => rollback.mutate()}
          title={fw.canRollBack ? "Restore the rules as they were before this change" : "Rolls back by itself when the time runs out"}>
          {rollback.isPending ? "Rolling back…" : "Roll back now"}
        </button>
        {errorLine}
      </div>
    );
  }
  if (fw.state === "rolled-back") {
    const at = fw.rolledBack?.at ? new Date(fw.rolledBack.at).toLocaleTimeString() : undefined;
    return (
      <div className="banner bad fw-banner" role="alert">
        <Icon name="alert" />
        <span>
          <b>The last change was rolled back{at ? ` at ${at}` : ""}</b> because nobody kept it in time. The rules below are not what the servers run:
          apply them again, or go back to the rules the servers run.
        </span>
        <button className="btn sm" disabled={retry.isPending} onClick={() => retry.mutate()}>{retry.isPending ? "Applying…" : "Apply again"}</button>
        {fw.canRollBack && (
          <button className="btn sm" disabled={rollback.isPending} onClick={() => rollback.mutate()}>{rollback.isPending ? "Restoring…" : "Discard the change"}</button>
        )}
        {errorLine}
      </div>
    );
  }
  if (fw.state === "failed") {
    return (
      <div className="banner bad fw-banner" role="alert">
        <Icon name="alert" />
        <span><b>A node could not apply the rules</b> and kept the ones it had: {fw.problems?.join("; ")}</span>
        <button className="btn sm" disabled={retry.isPending} onClick={() => retry.mutate()}>Apply again</button>
        {fw.canRollBack && <button className="btn sm" disabled={rollback.isPending} onClick={() => rollback.mutate()}>Discard the change</button>}
        {errorLine}
      </div>
    );
  }
  if (fw.state === "no-agents") {
    return (
      <div className="banner info fw-banner" role="status">
        <Icon name="server" />
        <span>No node agent has reported yet, so rules saved here are not applied; the installer's baseline protects the servers. The agent runs on every node once Kwerft's chart is installed with <code>firewall.agent.enabled</code>.</span>
      </div>
    );
  }
  if (fw.state === "unavailable") {
    return <div className="banner warn fw-banner" role="status"><Icon name="alert" /><span>The console cannot see the firewall's state on the nodes right now.</span></div>;
  }
  if (fw.state === "applying") {
    return <div className="banner info fw-banner" role="status"><Icon name="restart" /><span>Applying on the nodes…{fw.problems?.length ? ` ${fw.problems.join("; ")}` : ""}</span></div>;
  }
  return null;
}

function ClientHint({ fw }: { fw: Firewall }) {
  if (!fw.client.verifiable) {
    return <span className="pill warn nodot" title="A proxy or tunnel hides your address; Kwerft will not narrow SSH from here.">Your address is hidden ({fw.client.ip})</span>;
  }
  return fw.client.ssh
    ? <span className="dim">Your address <span className="mono">{fw.client.ip}</span> reaches SSH</span>
    : <span className="pill warn nodot">Your address {fw.client.ip} is not allowed to SSH</span>;
}

function statusPill(r: FirewallRule, fw: Firewall) {
  if (r.reason === "Invalid") return <span className="pill bad" title={r.message}>Invalid</span>;
  if (r.disabled) return <span className="pill off">Off</span>;
  if (r.name === "ssh" && r.sources.length > 0) {
    const covered = fw.client.verifiable && sshCovers(r.sources, fw.client.ip);
    return covered ? <span className="pill info nodot">your IP is covered</span> : <span className="pill warn nodot">your IP is not covered</span>;
  }
  if (r.required) return <span className="pill mute nodot" title="Part of the installer's firewall">required</span>;
  switch (r.reason) {
    case "Applied": return <span className="pill ok" title={r.message}>Applied</span>;
    case "PendingConfirmation": return <span className="pill warn" title={r.message}>Pending</span>;
    case "RolledBack": return <span className="pill bad" title={r.message}>Rolled back</span>;
    case "Failed": case "NotApplied": return <span className="pill bad" title={r.message}>Not applied</span>;
    default: return <span className="pill mute" title={r.message}>Applying</span>;
  }
}

function RuleRow({ r, fw, onEdit, onRemove }: { r: FirewallRule; fw: Firewall; onEdit: () => void; onRemove: () => void }) {
  const editable = r.editable !== "none";
  return (
    <tr className={editable ? "click" : undefined} onClick={editable ? onEdit : undefined}>
      <td className="mono">{describePorts(r)}</td>
      <td>{r.protocol === "ICMP" ? "ICMP" : r.protocol}</td>
      <td className="mono">{describeSources(r.sources)}</td>
      <td>
        {r.description || <span className="dim">{r.name}</span>}
        {r.description && <span className="sub mono">{r.name}</span>}
        {r.reason === "Invalid" && r.message && <span className="sub warn-text">{r.message}</span>}
      </td>
      <td>{r.nodes === "control-plane" ? "Control plane" : "All nodes"}</td>
      <td>
        {statusPill(r, fw)} {r.required && r.name === "ssh" && <span className="pill mute nodot">required</span>}
        {r.cloudFirewall && cloudRuleText[r.cloudFirewall] && (
          <> <span className={`pill nodot ${cloudRuleText[r.cloudFirewall]!.tone}`} title={cloudRuleText[r.cloudFirewall]!.title}>{cloudRuleText[r.cloudFirewall]!.label}</span></>
        )}
      </td>
      <td className="row-acts" onClick={(e) => e.stopPropagation()}>
        {r.editable === "all" && <button className="btn sm danger" onClick={onRemove} aria-label={`Delete ${r.name}`} title="Delete"><Icon name="trash" /></button>}
        {r.editable === "sources" && <button className="btn sm" onClick={onEdit}>Sources</button>}
      </td>
    </tr>
  );
}

function Nodes({ fw }: { fw: Firewall }) {
  if (fw.nodes.length === 0) return null;
  return (
    <div className="card scroll-x">
      <table className="t fw-table">
        <thead><tr><th>Node</th><th>Role</th><th>Firewall</th><th>Last report</th></tr></thead>
        <tbody>
          {fw.nodes.map((n) => {
            const tone = n.state === "in-sync" ? "ok" : n.state === "pending" || n.state === "waiting" ? "warn" : n.state === "rolled-back" || n.state === "error" || n.state === "unprepared" ? "bad" : "mute";
            return (
              <tr key={n.name}>
                <td className="nm">{n.name}</td>
                <td>{n.controlPlane ? "Control plane" : "Worker"}</td>
                <td><span className={`pill ${tone}`}>{nodeStateText[n.state] ?? n.state}</span>{n.message && <span className="sub">{n.message}</span>}</td>
                <td className="dim">{n.updatedAt ? new Date(n.updatedAt).toLocaleTimeString() : "—"}</td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// ---- dialogs -----------------------------------------------------------------

type Form = { name: string; protocol: "TCP" | "UDP"; port: string; endPort: string; sources: string; nodes: "all" | "control-plane"; description: string; enabled: boolean };

function formOf(r?: FirewallRule): Form {
  return {
    name: r?.name ?? "", protocol: r?.protocol === "UDP" ? "UDP" : "TCP", port: r ? String(r.port) : "", endPort: r?.endPort ? String(r.endPort) : "",
    sources: (r?.sources ?? []).join("\n"), nodes: r?.nodes ?? "all", description: r?.description ?? "", enabled: !(r?.disabled ?? false),
  };
}

function inputOf(f: Form): FirewallRuleInput {
  return {
    name: f.name.trim(), protocol: f.protocol, port: Number(f.port), endPort: f.endPort.trim() ? Number(f.endPort) : undefined,
    sources: parseSourcesText(f.sources), nodes: f.nodes, description: f.description.trim(), disabled: !f.enabled,
  };
}

function RuleDialog({ edit, onClose }: { edit?: FirewallRule; onClose: () => void }) {
  const queryClient = useQueryClient();
  const [f, setF] = useState(() => formOf(edit));
  const [error, setError] = useState<{ field?: string; message: string }>();
  const set = <K extends keyof Form>(k: K, v: Form[K]) => {
    setF((x) => ({ ...x, [k]: v }));
    setError((e) => (e?.field === k ? undefined : e));
  };
  const save = useMutation({
    mutationFn: () => (edit ? firewallApi.update(edit.name, inputOf(f)) : firewallApi.create(inputOf(f))),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: firewallKey });
      onClose();
    },
    onError: (e) => setError(e instanceof ApiError ? { field: e.field, message: e.message } : { message: unreachable }),
  });
  const err = (field: string) => (error?.field === field ? error.message : undefined);
  const submit = () => {
    const p = ruleProblem(f, !edit);
    if (p) return setError(p);
    setError(undefined);
    save.mutate();
  };
  return (
    <Dialog title={edit ? `Edit ${edit.name}` : "Open a port"} onClose={onClose} onSubmit={submit}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={save.isPending}>{save.isPending ? "Saving…" : edit ? "Save" : "Add rule"}</button>
      </>}>
      <p className="dim note">For services outside Kubernetes that listen on the servers themselves. Apps get their traffic through the ingress (HTTPS) and need no rule here.</p>
      {!edit && (
        <Field id="fw-name" label="Name" className="mono" value={f.name} onChange={(e) => set("name", e.target.value.toLowerCase())} autoComplete="off" spellCheck={false}
          placeholder="game-server" error={err("name")} hint="It cannot change later." />
      )}
      <div className="field">
        <label id="fw-proto">Protocol</label>
        <div className="seg" role="group" aria-labelledby="fw-proto">
          {(["TCP", "UDP"] as const).map((p) => <button type="button" key={p} aria-pressed={f.protocol === p} onClick={() => set("protocol", p)}>{p}</button>)}
        </div>
      </div>
      <div className="fields">
        <Field id="fw-port" label="Port" className="mono" inputMode="numeric" value={f.port} onChange={(e) => set("port", e.target.value.trim())} error={err("port")} placeholder="27015" />
        <Field id="fw-end" label="Up to (optional)" className="mono" inputMode="numeric" value={f.endPort} onChange={(e) => set("endPort", e.target.value.trim())} error={err("endPort")}
          hint="For a range of ports." />
      </div>
      <div className="field">
        <label htmlFor="fw-sources">Allowed from</label>
        <textarea id="fw-sources" className="input mono" rows={3} value={f.sources} onChange={(e) => set("sources", e.target.value)} spellCheck={false}
          placeholder="Anyone. Or addresses and ranges, like 203.0.113.0/24" aria-invalid={!!err("sources")} aria-describedby="fw-sources-note" />
        {err("sources") ? <span id="fw-sources-note" className="field-error" role="alert">{err("sources")}</span>
          : <span id="fw-sources-note" className="hint">One per line, IPv4 or IPv6. Empty: anyone on the internet.</span>}
      </div>
      <div className="field">
        <label id="fw-nodes">Applies to</label>
        <div className="seg" role="group" aria-labelledby="fw-nodes">
          <button type="button" aria-pressed={f.nodes === "all"} onClick={() => set("nodes", "all")}>All nodes</button>
          <button type="button" aria-pressed={f.nodes === "control-plane"} onClick={() => set("nodes", "control-plane")}>Control plane</button>
        </div>
      </div>
      <Field id="fw-desc" label="Purpose" value={f.description} onChange={(e) => set("description", e.target.value)} error={err("description")} placeholder="Game server for the team" />
      <label className="check"><input type="checkbox" checked={f.enabled} onChange={(e) => set("enabled", e.target.checked)} />Enabled</label>
      {error && !["name", "port", "endPort", "sources", "description"].includes(error.field ?? "") && <p className="form-error" role="alert">{error.message}</p>}
    </Dialog>
  );
}

// SSHDialog narrows (or opens) public SSH. The server checks it keeps this
// browser's address; the form says so before saving.
function SSHDialog({ r, fw, onClose }: { r: FirewallRule; fw: Firewall; onClose: () => void }) {
  const queryClient = useQueryClient();
  const [anyone, setAnyone] = useState(r.sources.length === 0);
  const [text, setText] = useState(r.sources.join("\n"));
  const [error, setError] = useState<string>();
  const sources = anyone ? [] : parseSourcesText(text);
  const covered = sshCovers(sources, fw.client.ip);
  const save = useMutation({
    mutationFn: () => firewallApi.update(r.name, { sources }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: firewallKey });
      onClose();
    },
    onError: (e) => setError(e instanceof ApiError ? e.message : unreachable),
  });
  const mine = fw.client.ip.includes(":") ? `${fw.client.ip}/128` : `${fw.client.ip}/32`;
  return (
    <Dialog title="Who may connect with SSH" onClose={onClose} onSubmit={() => { setError(undefined); save.mutate(); }}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={save.isPending || (!anyone && sources.length === 0)}>{save.isPending ? "Saving…" : "Save"}</button>
      </>}>
      <p className="dim note">SSH (TCP 22) on every node. The private network always reaches it, and sessions already open stay open.</p>
      <label className="check"><input type="checkbox" checked={anyone} onChange={(e) => { setAnyone(e.target.checked); setError(undefined); }} />Anyone on the internet</label>
      {!anyone && (
        <div className="field">
          <label htmlFor="ssh-sources">Only from</label>
          <textarea id="ssh-sources" className="input mono" rows={4} value={text} onChange={(e) => { setText(e.target.value); setError(undefined); }} spellCheck={false}
            placeholder="198.51.100.0/24" aria-describedby="ssh-note" />
          <span id="ssh-note" className="hint">
            One address or range per line, like your office or VPN.{" "}
            {fw.client.verifiable && !covered && (
              <button type="button" className="btn ghost sm" onClick={() => setText((t) => (t.trim() ? `${t.trim()}\n${mine}` : mine))}>Add my address ({fw.client.ip})</button>
            )}
          </span>
        </div>
      )}
      {!anyone && sources.length > 0 && (fw.client.verifiable
        ? <p className={covered ? "ok-text note" : "warn-text note"} role="status">{covered ? `Your address ${fw.client.ip} keeps SSH access.` : `Your address ${fw.client.ip} would lose SSH access; Kwerft will refuse this.`}</p>
        : <p className="warn-text note" role="status">Kwerft cannot see your real address ({fw.client.ip}), so it will not narrow SSH from this browser.</p>)}
      <p className="dim note">Narrowing SSH is applied on the nodes and rolls back after 60 seconds unless you keep it: open a new SSH session to check first.</p>
      {error && <p className="form-error" role="alert">{error}</p>}
    </Dialog>
  );
}

function DeleteDialog({ r, onClose }: { r: FirewallRule; onClose: () => void }) {
  const queryClient = useQueryClient();
  const del = useMutation({
    mutationFn: () => firewallApi.remove(r.name),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: firewallKey });
      onClose();
    },
  });
  return (
    <Dialog title={`Delete ${r.name}?`} onClose={onClose} onSubmit={() => del.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri danger" disabled={del.isPending}>{del.isPending ? "Deleting…" : "Delete rule"}</button>
      </>}>
      <p>{r.protocol} {describePorts(r)} closes on {r.nodes === "control-plane" ? "the control-plane nodes" : "every node"} for {describeSources(r.sources) === "any" ? "everyone" : describeSources(r.sources)}.
        It rolls back after 60 seconds unless you keep the change.</p>
      {del.isError && <p className="form-error" role="alert">{del.error instanceof ApiError ? del.error.message : unreachable}</p>}
    </Dialog>
  );
}
