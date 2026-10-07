// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { useState } from "react";
import { Link, getRouteApi, useNavigate } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../api";
import { AppsTabs } from "../components/AppsTabs";
import { Dialog } from "../components/Dialog";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { ClusterBadge, ClusterFilter } from "../components/ClusterUI";
import { inCluster, useClusterFilter, useClusters } from "../clusters";
import { jobs, type Volume, type VolumeClass, type VolumeInUse } from "../jobs";
import { abilities, ago, workloads } from "../workloads";
import { settingsApi } from "../settings";
import { errorText } from "./Apps";
import "../styles/workloads.css";
import "../styles/jobs.css";

const route = getRouteApi("/authed/apps/volumes");
const POLL = 5000;

const classes: { id: VolumeClass; label: string; note: string }[] = [
  { id: "local-nvme", label: "Local NVMe", note: "Fastest; pinned to the node it was created on; cannot grow" },
  { id: "hcloud-volume", label: "Hetzner Cloud Volume", note: "Moves between Cloud nodes; can grow" },
];

export function Volumes() {
  const search = route.useSearch();
  const navigate = useNavigate();
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const can = abilities(session.data);
  const projects = useQuery({ queryKey: ["projects"], queryFn: workloads.projects, refetchInterval: POLL });
  const volumes = useQuery({ queryKey: ["volumes"], queryFn: () => jobs.volumes(), refetchInterval: POLL });
  const [dialog, setDialog] = useState<"create" | { resize: Volume } | { delete: Volume }>();
  const [notice, setNotice] = useState<string>();
  const { multi } = useClusters();

  const project = search.project;
  const setProject = (p?: string) => void navigate({ to: "/apps/volumes", search: p ? { project: p } : {}, replace: true });
  const [cluster, setCluster] = useClusterFilter();
  const projectList = inCluster(projects.data ?? [], cluster);
  const shown = inCluster(volumes.data ?? [], cluster).filter((v) => !project || v.project === project);
  const noProjects = projects.isSuccess && projects.data.length === 0;
  const denied = can.deploy ? undefined : "Your role can view volumes but not change them.";

  return (
    <section className="view">
      <div className="ph">
        <div>
          <h1>Volumes</h1>
          <p>Disks that apps and jobs of a project share by name. Every pod that mounts one runs on the same node.</p>
        </div>
        <div className="acts">
          <button className="btn pri" disabled={!can.deploy || noProjects} title={denied ?? (noProjects ? "Create a project first." : undefined)} onClick={() => setDialog("create")}>
            <Icon name="plus" />New volume
          </button>
        </div>
      </div>
      <AppsTabs current="volumes" />

      {volumes.isError && <div className="banner bad" role="alert"><Icon name="alert" /><span>{errorText(volumes.error)}</span></div>}
      {notice && (
        <div className="banner info" role="status"><Icon name="disk" /><span>{notice}</span><button className="btn sm" onClick={() => setNotice(undefined)}>Dismiss</button></div>
      )}

      {((projects.data?.length ?? 0) > 1 || multi) && (
        <div className="toolbar">
          <ClusterFilter value={cluster} onChange={(c) => { setCluster(c); setProject(undefined); }} />
          {projectList.length > 1 && (
            <div className="seg" role="group" aria-label="Project">
              <button aria-pressed={!project} onClick={() => setProject(undefined)}>All projects</button>
              {projectList.map((p) => <button key={p.name} aria-pressed={project === p.name} onClick={() => setProject(p.name)}>{p.name}</button>)}
            </div>
          )}
        </div>
      )}

      {volumes.isPending ? (
        <p className="loading">Loading volumes…</p>
      ) : shown.length === 0 ? (
        <div className="empty">
          <h2>No volumes{project ? ` in ${project}` : " yet"}</h2>
          <p>A volume is a disk in a project that apps and jobs mount by name: a job writes a file, a server reads it. An app that needs a disk per replica gets one in its own settings instead.</p>
          {can.deploy && !noProjects && <button className="btn pri" onClick={() => setDialog("create")}><Icon name="plus" />New volume</button>}
        </div>
      ) : (
        <div className="card scroll-x">
          <table className="t">
            <thead><tr><th>Volume</th><th className="num">Size</th><th>Class</th><th>Status</th><th>Used by</th><th>Created</th><th><span className="sr">Actions</span></th></tr></thead>
            <tbody>
              {shown.map((v) => (
                <tr key={`${v.project}/${v.name}`}>
                  <td><span className="nm">{v.name}</span><span className="sub">{v.project}<ClusterBadge cluster={v.cluster} /></span></td>
                  <td className="num">{v.size}{v.capacity && v.capacity !== v.size ? <span className="sub">{v.capacity} provisioned</span> : null}</td>
                  <td>{v.class === "hcloud-volume" ? "Cloud Volume" : "Local NVMe"}</td>
                  <td><VolumeStatus v={v} /></td>
                  <td><UsedBy project={v.project} users={v.usedBy} /></td>
                  <td className="dim" title={new Date(v.created).toLocaleString()}>{ago(v.created)}</td>
                  <td className="nowrap">
                    <button className="btn sm" disabled={!can.deploy || v.phase === "deleting" || v.class === "local-nvme"}
                      title={v.class === "local-nvme" ? "Local NVMe volumes cannot grow. Create a larger one and copy the data with a task." : denied}
                      onClick={() => setDialog({ resize: v })}>Resize</button>{" "}
                    <button className="btn sm danger" disabled={!can.deploy || v.phase === "deleting"} title={denied} onClick={() => setDialog({ delete: v })}>Delete</button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {dialog === "create" && <CreateVolumeDialog project={project} onClose={() => setDialog(undefined)} />}
      {typeof dialog === "object" && "resize" in dialog && <ResizeDialog volume={dialog.resize} onClose={() => setDialog(undefined)} />}
      {typeof dialog === "object" && "delete" in dialog && (
        <DeleteVolumeDialog volume={dialog.delete} onClose={() => setDialog(undefined)}
          onWaiting={(w) => setNotice(`${dialog.delete.name} is deleted once ${w.usedBy.join(", ")} no longer mount it. Remove the mount from them, or delete them.`)} />
      )}
    </section>
  );
}

function VolumeStatus({ v }: { v: Volume }) {
  switch (v.phase) {
    case "bound": return <span className="pill ok" title={v.message}>Bound</span>;
    case "deleting": return <span className="pill warn" title={v.message}>{v.reason === "InUse" ? "Deleting · in use" : "Deleting"}</span>;
    case "lost": return <span className="pill bad" title={v.message}>Lost</span>;
    case "failed": return <span className="pill bad" title={v.message}>{v.reason ?? "Failed"}</span>;
    default: return <span className="pill info" title={v.message}>{v.class === "local-nvme" ? "Waits for first mount" : "Pending"}</span>;
  }
}

function UsedBy({ project, users }: { project: string; users: string[] }) {
  if (users.length === 0) return <span className="dim">nothing</span>;
  return (
    <span className="tags">
      {users.map((u) => {
        const [kind, name] = u.split("/") as [string, string];
        if (kind === "App") return <Link key={u} to="/apps/$project/$name" params={{ project, name }} className="tag">app {name}</Link>;
        if (kind === "Schedule") return <Link key={u} to="/jobs/$project/schedules/$name" params={{ project, name }} className="tag">schedule {name}</Link>;
        if (kind === "Task") return <Link key={u} to="/jobs/$project/tasks/$name" params={{ project, name }} className="tag">task {name}</Link>;
        return <span key={u} className="tag">{u}</span>;
      })}
    </span>
  );
}

function CreateVolumeDialog({ project: initialProject, onClose }: { project?: string; onClose: () => void }) {
  const queryClient = useQueryClient();
  const projects = useQuery({ queryKey: ["projects"], queryFn: workloads.projects });
  const [name, setName] = useState("");
  const [project, setProject] = useState(initialProject ?? "");
  const [size, setSize] = useState("10");
  const [cls, setCls] = useState<VolumeClass>("local-nvme");
  const [error, setError] = useState<{ field?: string; message: string }>();
  // Which classes this cluster has: Cloud Volumes need the hcloud CSI driver.
  const available = useQuery({ queryKey: ["volume-classes"], queryFn: settingsApi.volumeClasses, staleTime: 60_000 });
  const classInfo = (id: VolumeClass) => available.data?.find((c) => c.id === id);
  const proj = project || (projects.data?.length === 1 ? projects.data[0]!.name : "");
  const create = useMutation({
    mutationFn: () => jobs.createVolume(proj, { name, size: /^\d+(\.\d+)?$/.test(size.trim()) ? `${size.trim()}Gi` : size.trim(), class: cls }),
    onSuccess: () => { void queryClient.invalidateQueries({ queryKey: ["volumes"] }); onClose(); },
    onError: (e) => setError(e instanceof ApiError ? { field: e.field, message: e.message } : { message: errorText(e) }),
  });
  function submit() {
    if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(name) || name.length > 63) return setError({ field: "name", message: "Lowercase letters, digits and dashes, starting and ending with a letter or digit." });
    if (!proj) return setError({ field: "project", message: "Choose a project." });
    if (!/^\d+(\.\d+)?([KMGT]i)?$/.test(size.trim()) || Number.parseFloat(size) <= 0) return setError({ field: "size", message: "A size in GiB, or with a unit: 500Mi, 20Gi." });
    setError(undefined);
    create.mutate();
  }
  return (
    <Dialog title="New volume" onClose={onClose} onSubmit={submit}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={create.isPending}>{create.isPending ? "Creating…" : "Create volume"}</button>
      </>}>
      <div className="fields">
        <Field id="v-name" label="Name" className="mono" value={name} onChange={(e) => setName(e.target.value.toLowerCase())} placeholder="search-index" autoFocus
          autoComplete="off" spellCheck={false} error={error?.field === "name" ? error.message : undefined} />
        <div className="field">
          <label htmlFor="v-proj">Project</label>
          <select id="v-proj" className="input" value={proj} onChange={(e) => setProject(e.target.value)} aria-invalid={error?.field === "project"}>
            {!proj && <option value="">Choose…</option>}
            {projects.data?.map((p) => <option key={p.name} value={p.name}>{p.name}</option>)}
          </select>
          {error?.field === "project" && <span className="field-error" role="alert">{error.message}</span>}
        </div>
      </div>
      <Field id="v-size" label="Size (GiB)" className="mono" value={size} onChange={(e) => setSize(e.target.value)} inputMode="decimal"
        error={error?.field === "size" ? error.message : undefined} hint="A number means GiB; 500Mi works too." />
      <div className="field">
        <label>Class</label>
        <div className="choice two" role="group" aria-label="Class">
          {classes.map((c) => {
            const info = classInfo(c.id);
            const off = info !== undefined && !info.available;
            return (
              <button type="button" key={c.id} className="opt" aria-pressed={cls === c.id} disabled={off} title={off ? info.reason : undefined}
                onClick={() => setCls(c.id)}><b>{c.label}</b><span>{off ? info.reason : c.note}</span></button>
            );
          })}
        </div>
        {error?.field === "class" && <span className="field-error" role="alert">{error.message}</span>}
      </div>
      {error && !["name", "project", "size", "class"].includes(error.field ?? "") && <p className="form-error" role="alert">{error.message}</p>}
    </Dialog>
  );
}

function ResizeDialog({ volume, onClose }: { volume: Volume; onClose: () => void }) {
  const queryClient = useQueryClient();
  const [size, setSize] = useState(volume.size);
  const [error, setError] = useState<string>();
  const resize = useMutation({
    mutationFn: () => jobs.resizeVolume(volume.project, volume.name, /^\d+(\.\d+)?$/.test(size.trim()) ? `${size.trim()}Gi` : size.trim()),
    onSuccess: () => { void queryClient.invalidateQueries({ queryKey: ["volumes"] }); onClose(); },
    onError: (e) => setError(errorText(e)),
  });
  return (
    <Dialog title={`Resize ${volume.name}`} onClose={onClose} onSubmit={() => resize.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri" disabled={resize.isPending || size.trim() === volume.size}>{resize.isPending ? "Resizing…" : "Resize"}</button>
      </>}>
      <Field id="r-size" label="New size" className="mono" value={size} onChange={(e) => setSize(e.target.value)} autoFocus onFocus={(e) => e.target.select()}
        error={error} hint={`Now ${volume.size}. A volume can only grow; the file system grows online.`} />
    </Dialog>
  );
}

function DeleteVolumeDialog({ volume, onClose, onWaiting }: { volume: Volume; onClose: () => void; onWaiting: (w: VolumeInUse) => void }) {
  const queryClient = useQueryClient();
  const [confirm, setConfirm] = useState("");
  const del = useMutation({
    mutationFn: () => jobs.deleteVolume(volume.project, volume.name),
    onSuccess: (waiting) => {
      void queryClient.invalidateQueries({ queryKey: ["volumes"] });
      if (waiting?.usedBy?.length) onWaiting(waiting);
      onClose();
    },
  });
  return (
    <Dialog title={`Delete ${volume.name}?`} onClose={onClose} onSubmit={() => confirm === volume.name && del.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri danger" disabled={confirm !== volume.name || del.isPending}>{del.isPending ? "Deleting…" : "Delete volume"}</button>
      </>}>
      <p style={{ margin: 0 }}>The disk and all data on it are deleted. It cannot be undone.</p>
      {volume.usedBy.length > 0 && (
        <div className="banner warn"><Icon name="alert" /><span>Mounted by {volume.usedBy.join(", ")}. Deletion waits until none of them mounts it any more.</span></div>
      )}
      <Field id="vd-confirm" label={`Type ${volume.name} to confirm`} className="mono" value={confirm} onChange={(e) => setConfirm(e.target.value)} autoComplete="off"
        error={del.isError ? errorText(del.error) : undefined} />
    </Dialog>
  );
}
