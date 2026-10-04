import { useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError } from "../api";
import { accessApi, roleLabel } from "../access";
import { workloads, type Project, type ProjectMember } from "../workloads";
import { Dialog } from "./Dialog";
import { Icon } from "./Icon";
import "../styles/access.css";

type MemberRole = ProjectMember["role"];
const errText = (e: unknown) => (e instanceof ApiError ? e.message : "The console could not be reached.");

// Who reaches a project, besides owners and admins (who reach every one):
// the whole team with their console roles (Team), or only its members, each
// with a role given here (Members). Owners and admins manage it; Kubernetes
// enforces it through the project's RoleBindings.
export function ProjectAccessDialog({ project, onClose }: { project: Project; onClose: () => void }) {
  const queryClient = useQueryClient();
  const access = useQuery({ queryKey: ["project-access", project.name], queryFn: () => workloads.projectAccess(project.name) });
  const team = useQuery({ queryKey: ["members"], queryFn: accessApi.members });
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  // Switching to Members: who keeps access, and as what.
  const [keep, setKeep] = useState<Record<string, MemberRole | "">>();
  const [adding, setAdding] = useState<{ user: string; role: MemberRole }>({ user: "", role: "developer" });

  // Developers and viewers can be members; owners and admins reach every project anyway.
  const candidates = (team.data ?? []).filter((m) => m.role === "developer" || m.role === "viewer");
  const listed = access.data?.members ?? [];

  async function run(f: () => Promise<unknown>) {
    setBusy(true);
    setError(undefined);
    try {
      const out = await f();
      if (out) queryClient.setQueryData(["project-access", project.name], out);
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
      return true;
    } catch (e) {
      setError(errText(e));
      return false;
    } finally {
      setBusy(false);
    }
  }

  async function limit() {
    const members = Object.entries(keep ?? {})
      .filter(([, role]) => role !== "")
      .map(([user, role]) => ({ user, role: role as MemberRole }));
    if (await run(() => workloads.setProjectAccess(project.name, "Members", members))) setKeep(undefined);
  }

  const startLimiting = () =>
    setKeep(Object.fromEntries(candidates.map((m) => [m.email, m.role as MemberRole])));

  const body = () => {
    if (access.isPending || team.isPending) return <p className="loading">Loading…</p>;
    if (access.isError) return <p className="form-error" role="alert">{errText(access.error)}</p>;
    const a = access.data;

    if (a.access === "Team" && !keep) {
      return (
        <>
          <p className="note">
            <b>Everyone</b> on the team reaches <b>{project.name}</b> with their console role. Limit it to its members to keep
            other developers and viewers out: they then no longer see its apps, jobs, logs, metrics or alerts.
          </p>
          <div><button type="button" className="btn" onClick={startLimiting}><Icon name="shield" />Limit to members…</button></div>
        </>
      );
    }

    if (keep) {
      return (
        <>
          <p className="note">Who keeps access to <b>{project.name}</b>? Owners and admins always do.</p>
          {candidates.length === 0 ? <p className="dim note">There are no developers or viewers on the team yet.</p> : (
            <table className="t">
              <thead><tr><th>Member</th><th>In this project</th></tr></thead>
              <tbody>
                {candidates.map((m) => (
                  <tr key={m.id}>
                    <td><span className="nm">{m.name}</span><span className="sub">{m.email}</span></td>
                    <td>
                      <select className="input" aria-label={`Access for ${m.name}`} value={keep[m.email] ?? ""}
                        onChange={(e) => setKeep({ ...keep, [m.email]: e.target.value as MemberRole | "" })}>
                        <option value="">No access</option>
                        <option value="developer">Developer</option>
                        <option value="viewer">Viewer</option>
                      </select>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          <div className="acts">
            <button type="button" className="btn pri" disabled={busy} onClick={limit}>{busy ? "Saving…" : "Limit to these members"}</button>
            <button type="button" className="btn" onClick={() => setKeep(undefined)}>Back</button>
          </div>
        </>
      );
    }

    const addable = candidates.filter((m) => !listed.some((l) => l.user.toLowerCase() === m.email.toLowerCase()));
    return (
      <>
        <p className="note">
          Only these members reach <b>{project.name}</b>, with the role given here (it replaces their console role in this
          project). Owners and admins always do.
        </p>
        <table className="t">
          <thead><tr><th>Member</th><th>Role here</th><th><span className="sr">Actions</span></th></tr></thead>
          <tbody>
            {listed.length === 0 && <tr><td colSpan={3} className="dim">No members: only owners and admins reach this project.</td></tr>}
            {listed.map((m) => (
              <tr key={m.user}>
                <td><span className="nm">{m.name || m.user}</span>{m.name && <span className="sub">{m.user}</span>}{!m.name && <span className="sub">no account</span>}</td>
                <td>
                  <select className="input" aria-label={`Role of ${m.user}`} value={m.role} disabled={busy}
                    onChange={(e) => run(() => workloads.addProjectMember(project.name, m.user, e.target.value as MemberRole))}>
                    <option value="developer">Developer</option>
                    <option value="viewer">Viewer</option>
                  </select>
                </td>
                <td className="nowrap">
                  <button type="button" className="btn sm danger" disabled={busy}
                    onClick={() => run(() => workloads.removeProjectMember(project.name, m.user))}>Remove</button>
                </td>
              </tr>
            ))}
          </tbody>
        </table>
        {addable.length > 0 && (
          <div className="acts" style={{ flexWrap: "wrap" }}>
            <select className="input" aria-label="Member to add" value={adding.user} onChange={(e) => setAdding({ ...adding, user: e.target.value })}>
              <option value="">Add a member…</option>
              {addable.map((m) => <option key={m.id} value={m.email}>{m.name} ({m.email}, {roleLabel[m.role].toLowerCase()})</option>)}
            </select>
            <select className="input" aria-label="Role of the new member" value={adding.role} onChange={(e) => setAdding({ ...adding, role: e.target.value as MemberRole })}>
              <option value="developer">Developer</option>
              <option value="viewer">Viewer</option>
            </select>
            <button type="button" className="btn" disabled={busy || adding.user === ""}
              onClick={async () => { if (await run(() => workloads.addProjectMember(project.name, adding.user, adding.role))) setAdding({ user: "", role: adding.role }); }}>
              Add
            </button>
          </div>
        )}
        <div className="danger-zone">
          <p>Opening the project to the whole team gives every developer and viewer access again, with their console role.</p>
          <div><button type="button" className="btn" disabled={busy} onClick={() => run(() => workloads.setProjectAccess(project.name, "Team", []))}>Open to the whole team</button></div>
        </div>
      </>
    );
  };

  return (
    <Dialog title={`Access to ${project.name}`} wide onClose={onClose} onSubmit={onClose} actions={<button className="btn pri">Done</button>}>
      {body()}
      {error && <p className="form-error" role="alert">{error}</p>}
      <p className="dim note">Changes apply within seconds, also to Kubernetes, and are recorded in the audit log.</p>
    </Dialog>
  );
}
