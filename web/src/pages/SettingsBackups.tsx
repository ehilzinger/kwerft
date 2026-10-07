// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { useEffect, useRef, useState, type FormEvent } from "react";
import { Link } from "@tanstack/react-router";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError } from "../api";
import { CopyButton } from "../components/CopyButton";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { ago } from "../workloads";
import {
  backupKeys, backupsApi, describeEtcdUpload, etcdFetchCommand, etcdUploadPill, groupKey, recoveryKeyFile, recoveryKeyFileName,
  recoveryKeyProblem, targetPill, type BackupTarget, type BucketCheck, type EtcdUpload, type TargetInput,
} from "../backups";
import "../styles/backups.css";

const unreachable = "The console could not be reached. Check your connection and try again.";

// Settings › Backups: the S3 bucket backups go to (Hetzner Object Storage),
// its access keys (write-only), the recovery key (made on the first save and
// shown exactly once), and k3s's etcd snapshots to the same bucket (uploaded
// by Kwerft's agent on each etcd node, encrypted; their state per node and
// how to read one back for a restore). Owners and admins only.
export function BackupsCard() {
  // Above the form, so nothing that re-renders the form can lose it.
  const [newKey, setNewKey] = useState<string>();
  const target = useQuery({
    queryKey: backupKeys.target, queryFn: backupsApi.target,
    refetchInterval: (q) => (q.state.data?.state === "Pending" ? 5000 : 30000),
  });
  return (
    <section className="card" id="backups">
      <h2>Backups</h2>
      <div className="bd stack">
        {target.isPending && <p className="loading">Loading the backup target…</p>}
        {target.isError && <div className="banner bad" role="alert"><Icon name="alert" /><span>{target.error instanceof ApiError ? target.error.message : unreachable}</span></div>}
        {target.data && <TargetForm t={target.data} onKey={setNewKey} />}
      </div>
      {newKey && <RecoveryKeyDialog recoveryKey={newKey} host={target.data?.defaultPrefix ?? window.location.hostname} onDone={() => setNewKey(undefined)} />}
    </section>
  );
}

function TargetForm({ t, onKey }: { t: BackupTarget; onKey: (key: string) => void }) {
  const queryClient = useQueryClient();
  const [endpoint, setEndpoint] = useState(t.endpoint ?? "https://fsn1.your-objectstorage.com");
  const [region, setRegion] = useState(t.region ?? "");
  const [bucket, setBucket] = useState(t.bucket ?? "");
  const [prefix, setPrefix] = useState(t.prefix ?? "");
  const [accessKey, setAccessKey] = useState("");
  const [secretKey, setSecretKey] = useState("");
  const [haveKey, setHaveKey] = useState(false);
  const [recoveryKey, setRecoveryKey] = useState("");
  const [etcd, setEtcd] = useState(t.etcdSnapshots.enabled);
  const [etcdSchedule, setEtcdSchedule] = useState(t.etcdSnapshots.schedule ?? "");
  const [etcdRetention, setEtcdRetention] = useState(t.etcdSnapshots.retention ? String(t.etcdSnapshots.retention) : "");
  const [error, setError] = useState<{ field?: string; message: string }>();
  const [busy, setBusy] = useState<"check" | "save">();
  const [check, setCheck] = useState<BucketCheck>();
  const [saved, setSaved] = useState<string>();
  const fieldError = (f: string) => (error?.field === f ? error.message : undefined);
  const fail = (err: unknown) => setError(err instanceof ApiError ? { field: err.field, message: err.message } : { message: unreachable });
  const keyProblem = haveKey ? recoveryKeyProblem(recoveryKey) : undefined;
  const pill = targetPill(t);
  const usedPrefix = prefix.trim() || t.prefix || t.defaultPrefix;

  function input(): TargetInput {
    const retention = Number(etcdRetention);
    return {
      endpoint: endpoint.trim(), region: region.trim() || undefined, bucket: bucket.trim(), prefix: prefix.trim() || undefined,
      accessKey: accessKey.trim() || undefined, secretKey: secretKey.trim() || undefined,
      recoveryKey: haveKey && recoveryKey.trim() ? recoveryKey.trim() : undefined,
      etcdSnapshots: { enabled: etcd, schedule: etcd ? etcdSchedule.trim() || undefined : undefined, retention: etcd && retention > 0 ? retention : undefined },
    };
  }

  async function runCheck() {
    setError(undefined);
    setCheck(undefined);
    setSaved(undefined);
    setBusy("check");
    try {
      // Without new keys the server checks the saved target with the stored ones.
      const i = input();
      setCheck(await backupsApi.check(i.accessKey ? i : {}));
    } catch (err) {
      fail(err);
    } finally {
      setBusy(undefined);
    }
  }

  async function save(e: FormEvent) {
    e.preventDefault();
    if (keyProblem) return;
    setError(undefined);
    setCheck(undefined);
    setSaved(undefined);
    setBusy("save");
    try {
      const res = await backupsApi.saveTarget(input());
      queryClient.setQueryData(backupKeys.target, res.settings);
      void queryClient.invalidateQueries({ queryKey: backupKeys.plans });
      setAccessKey("");
      setSecretKey("");
      setRecoveryKey("");
      setHaveKey(false);
      if (res.check) setCheck(res.check);
      setSaved(res.planCreated
        ? "Saved. The plan “cluster” backs up everything daily at 03:00 UTC and keeps each backup 14 days."
        : "Saved. Velero checks the bucket within a minute.");
      if (res.recoveryKey) onKey(res.recoveryKey);
    } catch (err) {
      fail(err);
    } finally {
      setBusy(undefined);
    }
  }

  return (
    <>
      <div className="backup-state">
        <span className={`pill ${pill.pill}`}>{pill.label}</span>
        {t.message && <span className="dim">{t.message}</span>}
        {t.lastSuccessfulAt && <span className="dim">Last successful backup {ago(t.lastSuccessfulAt)}.</span>}
        {t.configured && <Link to="/backups" className="end">Plans and backups →</Link>}
      </div>
      <p className="dim note">
        Velero backs up your projects, their volumes (file by file) and Kwerft&apos;s own state to an S3 bucket, such as Hetzner Object
        Storage, all of it encrypted with the recovery key. With such a backup and the recovery key, <code>install.sh --restore</code> rebuilds this console on a new server.
      </p>
      <form className="stack tight" onSubmit={save}>
        <div className="fields">
          <Field id="bk-endpoint" label="Endpoint" className="mono" value={endpoint} onChange={(e) => setEndpoint(e.target.value)}
            placeholder="https://fsn1.your-objectstorage.com" autoComplete="off" spellCheck={false} error={fieldError("endpoint")}
            hint="Hetzner Object Storage: https://<location>.your-objectstorage.com" />
          <Field id="bk-region" label="Region" className="mono" value={region} onChange={(e) => setRegion(e.target.value)}
            placeholder="from the endpoint, e.g. fsn1" autoComplete="off" spellCheck={false} error={fieldError("region")} />
          <Field id="bk-bucket" label="Bucket" className="mono" value={bucket} onChange={(e) => setBucket(e.target.value)}
            placeholder="acme-kwerft" autoComplete="off" spellCheck={false} error={fieldError("bucket")}
            hint="Create it first; keep it private." />
          <Field id="bk-prefix" label="Prefix" className="mono" value={prefix} onChange={(e) => setPrefix(e.target.value)}
            placeholder={t.prefix || t.defaultPrefix} autoComplete="off" spellCheck={false} error={fieldError("prefix")}
            hint={`Backups go to ${usedPrefix}/velero, etcd snapshots to ${usedPrefix}/etcd. Two consoles must not share one.`} />
          <Field id="bk-access" label="Access key" className="mono" value={accessKey} onChange={(e) => setAccessKey(e.target.value)}
            placeholder={t.credentialsSet ? "•••••••• stored — enter a new one to replace it" : "Object Storage access key"}
            autoComplete="off" spellCheck={false} error={fieldError("accessKey")} />
          <Field id="bk-secret" label="Secret key" className="mono" type="password" value={secretKey} onChange={(e) => setSecretKey(e.target.value)}
            placeholder={t.credentialsSet ? "•••••••• stored" : "Object Storage secret key"} autoComplete="new-password" spellCheck={false}
            error={fieldError("secretKey")} hint="Kwerft checks the keys can list, write and delete in the bucket, stores them, and never shows them again." />
        </div>

        {t.recoveryKeySet ? (
          <p className="dim note">The recovery key was created{t.recoveryKeyCreatedAt ? ` ${ago(t.recoveryKeyCreatedAt)}` : ""} and shown once. It never changes; Kwerft cannot show it again.</p>
        ) : (
          <div className="field full">
            <label className="check">
              <input type="checkbox" checked={haveKey} onChange={(e) => setHaveKey(e.target.checked)} />
              <span>
                <b>Use the recovery key of earlier backups</b>
                <small>For a console rebuilt by hand next to backups it made before. Otherwise saving creates a new key and shows it once.</small>
              </span>
            </label>
            {haveKey && (
              <Field id="bk-recovery" label="Recovery key" className="mono" value={recoveryKey} onChange={(e) => setRecoveryKey(e.target.value)}
                placeholder="ABCD-EFGH-…" autoComplete="off" spellCheck={false} error={keyProblem ?? fieldError("recoveryKey")} />
            )}
          </div>
        )}

        <div className="field full">
          <label className="check">
            <input type="checkbox" checked={etcd} onChange={(e) => setEtcd(e.target.checked)} />
            <span>
              <b>Send k3s&apos;s etcd snapshots to the bucket too</b>
              <small>
                The cluster state as k3s snapshots it, to {usedPrefix}/etcd/&lt;node&gt;, encrypted with a key derived from the recovery key.
                Local snapshots go on either way.
              </small>
            </span>
          </label>
        </div>
        {etcd && (
          <div className="fields">
            <Field id="bk-etcd-schedule" label="Snapshot schedule" className="mono" value={etcdSchedule} onChange={(e) => setEtcdSchedule(e.target.value)}
              placeholder="0 */6 * * *" error={fieldError("etcdSnapshots.schedule")} hint="Takes effect when the installer runs again on the first server." />
            <Field id="bk-etcd-keep" label="Snapshots kept" className="mono" inputMode="numeric" value={etcdRetention} onChange={(e) => setEtcdRetention(e.target.value)}
              placeholder="28" error={fieldError("etcdSnapshots.retention")} />
          </div>
        )}

        {t.configured && t.etcdSnapshots.enabled && <EtcdUploads uploads={t.etcdUploads ?? []} />}

        {check && <CheckResult check={check} />}
        {saved && <p className="ok-text" role="status">{saved}</p>}
        {error && !error.field && <p className="form-error" role="alert">{error.message}</p>}
        <div className="actions">
          <button type="button" className="btn" disabled={!!busy || (!t.credentialsSet && !accessKey.trim())} onClick={runCheck}>
            {busy === "check" ? "Checking…" : "Check connection"}
          </button>
          <button className="btn pri" disabled={!!busy || !!keyProblem || !bucket.trim()}>{busy === "save" ? "Checking and saving…" : t.configured ? "Save" : "Save and start backups"}</button>
        </div>
      </form>
    </>
  );
}

// The etcd snapshot agents' uploads, per node, and how to read a snapshot
// back for k3s --cluster-reset.
function EtcdUploads({ uploads }: { uploads: EtcdUpload[] }) {
  const node = uploads.length > 1 ? uploads[0].node : undefined;
  return (
    <div className="etcd-uploads">
      {uploads.length === 0 ? (
        <p className="dim note">No etcd snapshot uploaded yet. The agent on each etcd node uploads every snapshot k3s takes within a few minutes.</p>
      ) : (
        <ul className="etcd-list" aria-label="etcd snapshot uploads">
          {uploads.map((u) => {
            const pill = etcdUploadPill(u);
            return (
              <li key={u.node}>
                <span className={`pill ${pill.pill}`}>{pill.label}</span>
                <span className="mono">{u.node}</span>
                <span className={u.message ? "problem" : "dim"}>{describeEtcdUpload(u)}</span>
              </li>
            );
          })}
        </ul>
      )}
      <details className="yaml">
        <summary>Restoring the cluster state from an etcd snapshot</summary>
        <div className="stack tight note">
          <p className="dim">
            Only Kwerft reads the snapshots back: <code>kwerft etcd-snapshot fetch</code> decrypts one with the recovery key, using the same
            {" "}<code>backups:</code> block and key files as <code>install.sh --restore</code> (the image ghcr.io/ehilzinger/kwerft holds the binary;
            {" "}<code>kwerft etcd-snapshot list</code> shows what is there). Then, on the server, with k3s stopped and the old server&apos;s token:
          </p>
          <pre className="mono">{`${etcdFetchCommand(node)}
k3s server --cluster-reset --cluster-reset-restore-path=<file> --token=<old token>`}</pre>
          <p className="dim">
            The token is in <code>/var/lib/rancher/k3s/server/token</code> of the old server and in every Cluster backup
            {" "}(<code>kwerft-system/cluster-local-join</code>): without it k3s cannot read the snapshot&apos;s certificates and secrets.
          </p>
        </div>
      </details>
    </div>
  );
}

function CheckResult({ check }: { check: BucketCheck }) {
  return (
    <p className="ok-text note" role="status">
      ✓ The bucket answers: these keys can list, write and delete.
      {check.hasBackups ? " The prefix already holds backups; only their recovery key reads them." : check.hasObjects ? " The prefix already holds other objects." : " The prefix is empty."}
    </p>
  );
}

// The recovery key, once: copy it or download it, and confirm it is stored
// before the dialog closes. Escape does not dismiss it.
export function RecoveryKeyDialog({ recoveryKey, host, onDone }: { recoveryKey: string; host: string; onDone: () => void }) {
  const ref = useRef<HTMLDialogElement>(null);
  const [stored, setStored] = useState(false);
  useEffect(() => {
    const d = ref.current;
    if (d && !d.open) d.showModal();
    return () => {
      if (d?.open) d.close();
    };
  }, []);

  function download() {
    const blob = new Blob([recoveryKeyFile(recoveryKey, host, new Date())], { type: "text/plain" });
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = recoveryKeyFileName(host);
    a.click();
    window.setTimeout(() => URL.revokeObjectURL(url), 1000);
  }

  return (
    <dialog ref={ref} className="dlg" aria-labelledby="rk-title" onCancel={(e) => e.preventDefault()}>
      <form onSubmit={(e) => { e.preventDefault(); if (stored) onDone(); }}>
        <h2 id="rk-title">Store the recovery key</h2>
        <div className="dlg-bd stack">
          <p>
            Everything Velero writes to the bucket is encrypted with this key: volume data, apps, settings and secrets. <b>Without it no backup
            can be read</b>, not even by you: restoring this console on
            a new server asks for it. Kwerft shows it <b>only now</b>.
          </p>
          <div className="recovery-key mono" aria-label="Recovery key">
            {groupKey(recoveryKey).map((g, i) => <span key={i}>{g}</span>)}
          </div>
          <div className="actions start">
            <CopyButton text={recoveryKey} label="Copy" className="btn" />
            <button type="button" className="btn" onClick={download}>Download as a file</button>
          </div>
          <label className="check">
            <input type="checkbox" checked={stored} onChange={(e) => setStored(e.target.checked)} />
            <span><b>I stored the recovery key</b><small>somewhere safe outside this server, such as a password manager.</small></span>
          </label>
        </div>
        <div className="dlg-ft">
          <button className="btn pri" disabled={!stored}>Done</button>
        </div>
      </form>
    </dialog>
  );
}
