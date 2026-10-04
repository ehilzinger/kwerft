import { useState } from "react";
import { Link, useNavigate } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../api";
import { AppStatus } from "../components/AppStatus";
import { CreateProjectDialog } from "../components/CreateProjectDialog";
import { Dialog } from "../components/Dialog";
import { Icon } from "../components/Icon";
import { abilities, ago, hostOf, workloads, type AppSummary, type Project } from "../workloads";
import "../styles/workloads.css";

const POLL = 5000;

export function Apps() {
  const navigate = useNavigate();
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const can = abilities(session.data);
  const projects = useQuery({ queryKey: ["projects"], queryFn: workloads.projects, refetchInterval: POLL });
  const apps = useQuery({ queryKey: ["apps"], queryFn: () => workloads.apps(), refetchInterval: POLL });

  const [project, setProject] = useState<string>(); // undefined = all
  const [problemsOnly, setProblemsOnly] = useState(false);
  const [q, setQ] = useState("");
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState<Project>();

  const all = apps.data ?? [];
  const query = q.trim().toLowerCase();
  const shown = all.filter(
    (a) =>
      (!project || a.project === project) &&
      (!problemsOnly || isProblem(a)) &&
      (!query || `${a.name} ${a.project} ${a.image} ${a.source.repository ?? ""}`.toLowerCase().includes(query)),
  );
  const selected = projects.data?.find((p) => p.name === project);
  const noProjects = projects.isSuccess && projects.data.length === 0;

  const deployButton = (
    <Link to="/apps/new" search={{ project }} className="btn pri" disabled={!can.deploy || noProjects}
      title={!can.deploy ? "Your role can view apps but not deploy them." : noProjects ? "Create a project first." : undefined}
      aria-disabled={!can.deploy || noProjects}>
      <Icon name="plus" />Deploy app
    </Link>
  );

  return (
    <section className="view">
      <div className="ph">
        <div>
          <h1>Apps</h1>
          <p>Deployments and stateful services across your projects.</p>
        </div>
        <div className="acts">
          {can.manageProjects && <button className="btn" onClick={() => setCreating(true)}><Icon name="plus" />New project</button>}
          {deployButton}
        </div>
      </div>

      {(apps.isError || projects.isError) && (
        <div className="banner bad" role="alert"><Icon name="alert" /><span>{errorText(apps.error ?? projects.error)}</span></div>
      )}

      {noProjects ? (
        <div className="empty">
          <h2>Start with a project</h2>
          <p>Apps live in projects. Each project is its own namespace with a resource quota and network isolation, so a project per product or environment works well.</p>
          {can.manageProjects ? (
            <button className="btn pri" onClick={() => setCreating(true)}><Icon name="plus" />Create a project</button>
          ) : (
            <p className="dim">Ask an owner or admin to create one.</p>
          )}
        </div>
      ) : (
        <>
          <div className="toolbar">
            <input className="input" value={q} onChange={(e) => setQ(e.target.value)} placeholder="Filter by name or image" aria-label="Filter apps" style={{ width: 260 }} />
            <div className="seg" role="group" aria-label="Project">
              <button aria-pressed={!project} onClick={() => setProject(undefined)}>All projects</button>
              {projects.data?.map((p) => (
                <button key={p.name} aria-pressed={project === p.name} onClick={() => setProject(p.name)} title={p.displayName}>
                  {p.name}<span className="n">{p.apps}</span>
                </button>
              ))}
            </div>
            <div className="seg" role="group" aria-label="Status" style={{ marginLeft: "auto" }}>
              <button aria-pressed={!problemsOnly} onClick={() => setProblemsOnly(false)}>Any status</button>
              <button aria-pressed={problemsOnly} onClick={() => setProblemsOnly(true)}>Problems</button>
            </div>
          </div>

          {selected && selected.phase !== "ready" && (
            <div className={`banner ${selected.phase === "failed" ? "bad" : "info"}`}>
              <Icon name="alert" />
              <span>Project <b>{selected.name}</b>: {selected.message || "being set up…"}</span>
            </div>
          )}

          {apps.isPending ? (
            <p className="loading">Loading apps…</p>
          ) : shown.length > 0 ? (
            <div className="card scroll-x">
              <table className="t">
                <thead>
                  <tr><th>App</th><th>Image</th><th className="num">Replicas</th><th>Status</th><th>Domain</th><th>Updated</th></tr>
                </thead>
                <tbody>
                  {shown.map((a) => (
                    <Row key={`${a.project}/${a.name}`} app={a}
                      onOpen={() => navigate({ to: "/apps/$project/$name", params: { project: a.project, name: a.name } })} />
                  ))}
                </tbody>
              </table>
            </div>
          ) : all.length === 0 || (project && selected?.apps === 0) ? (
            <div className="empty">
              <h2>No apps {project ? `in ${project}` : "yet"}</h2>
              <p>Deploy a container image from any registry. Building from a Git repository on every push follows in Phase 2.</p>
              {can.deploy && deployButton}
              {project && selected && selected.apps === 0 && can.manageProjects && (
                <button className="btn ghost danger" onClick={() => setDeleting(selected)}>Delete project {project}</button>
              )}
            </div>
          ) : (
            <div className="empty">
              <h2>No apps match</h2>
              <p>Nothing matches these filters.</p>
              <button className="btn" onClick={() => { setQ(""); setProblemsOnly(false); setProject(undefined); }}>Clear filters</button>
            </div>
          )}
        </>
      )}

      {creating && <CreateProjectDialog onClose={() => setCreating(false)} onCreated={(p) => setProject(p.name)} />}
      {deleting && <DeleteProjectDialog project={deleting} onClose={() => setDeleting(undefined)} onDeleted={() => setProject(undefined)} />}
    </section>
  );
}

// A problem: failed, or still not fully rolled out ten minutes after the last change.
function isProblem(a: AppSummary) {
  if (a.phase === "failed") return true;
  return a.phase === "deploying" && Date.now() - new Date(a.updated).getTime() > 10 * 60 * 1000;
}

function Row({ app: a, onOpen }: { app: AppSummary; onOpen: () => void }) {
  const source = a.source.type === "git" ? `git · ${a.source.repository?.replace(/^https?:\/\//, "").replace(/\.git$/, "")}@${a.source.branch || "main"}` : a.image;
  return (
    <tr className="click" onClick={onOpen}>
      <td>
        <span className="nm">
          <Link to="/apps/$project/$name" params={{ project: a.project, name: a.name }} onClick={(e) => e.stopPropagation()}>{a.name}</Link>
        </span>
        <span className="sub">{a.project}{a.stateful ? " · stateful" : ""}</span>
      </td>
      <td className="mono ell" title={source}>{source || "—"}</td>
      <td className="num">{a.readyReplicas}/{a.replicas}</td>
      <td><AppStatus phase={a.phase} reason={a.reason} message={a.message} /></td>
      <td>{a.urls.length > 0 ? hostOf(a.urls[0]!) + (a.urls.length > 1 ? ` +${a.urls.length - 1}` : "") : <span className="dim">internal</span>}</td>
      <td className="dim" title={new Date(a.updated).toLocaleString()}>{ago(a.updated)}</td>
    </tr>
  );
}

function DeleteProjectDialog({ project, onClose, onDeleted }: { project: Project; onClose: () => void; onDeleted: () => void }) {
  const queryClient = useQueryClient();
  const del = useMutation({
    mutationFn: () => workloads.deleteProject(project.name),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["projects"] });
      onDeleted();
      onClose();
    },
  });
  return (
    <Dialog title={`Delete project ${project.name}?`} onClose={onClose} onSubmit={() => del.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Cancel</button>
        <button className="btn pri danger" disabled={del.isPending}>{del.isPending ? "Deleting…" : "Delete project"}</button>
      </>}>
      <p style={{ margin: 0 }}>This removes the namespace <code>{project.name}</code> and everything in it. It cannot be undone.</p>
      {del.isError && <p className="form-error" role="alert">{errorText(del.error)}</p>}
    </Dialog>
  );
}

export function errorText(e: unknown) {
  if (e instanceof ApiError) return e.message;
  return "The console could not be reached. Check your connection and try again.";
}
