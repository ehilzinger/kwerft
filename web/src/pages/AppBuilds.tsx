// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { useState } from "react";
import { Link, useNavigate } from "@tanstack/react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError } from "../api";
import {
  buildLogsPath, buildResult, buildsApi, commitURL, durationText, finished, notOffered, repoLabel, shortSha, triggerText, type Build,
} from "../builds";
import { BuildStatus } from "../components/BuildStatus";
import { Dialog } from "../components/Dialog";
import { Icon } from "../components/Icon";
import { LogViewer } from "../components/LogViewer";
import { ago, type App } from "../workloads";
import { errorText } from "./Apps";
import "../styles/builds.css";

export const buildsKey = (project: string, app: string) => ["builds", project, app];

/** An App's builds, polled quickly while one runs. Only for Git apps. */
export function useBuilds(app: App) {
  const project = app.metadata.namespace;
  const name = app.metadata.name;
  return useQuery({
    queryKey: buildsKey(project, name),
    queryFn: () => buildsApi.list(project, name),
    enabled: !!app.spec.source.git,
    refetchInterval: (q) => (q.state.data?.some((b) => !finished(b)) ? 3000 : 15000),
  });
}

/** "#118", or the short commit while the build has no number yet. */
export const buildName = (b: Pick<Build, "number" | "commit">) => (b.number ? `#${b.number}` : shortSha(b.commit));

// The Builds tab of a Git app: the blueprint's toolbar and table, and below
// it the selected build (?build=<name>) with its live log.
export function AppBuilds({ app, canDeploy, selected }: { app: App; canDeploy: boolean; selected?: string }) {
  const project = app.metadata.namespace;
  const name = app.metadata.name;
  const git = app.spec.source.git;
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const builds = useBuilds(app);
  const [notice, setNotice] = useState<string>();
  const [cancelling, setCancelling] = useState<Build>();
  const open = (build?: string) => navigate({ to: "/apps/$project/$name", params: { project, name }, search: build ? { build } : {} });

  const buildNow = useMutation({
    mutationFn: () => buildsApi.buildNow(project, name),
    onSuccess: async (b) => {
      setNotice(undefined);
      await queryClient.invalidateQueries({ queryKey: buildsKey(project, name) });
      void open(b.name);
    },
    onError: (e) => setNotice(notOffered(e) ? "This console cannot start builds yet. Push to the branch instead, once webhooks are set up." : errorText(e)),
  });

  if (!git) {
    return (
      <div className="empty">
        <h2>No builds for image apps</h2>
        <p>This app runs a ready-made image. Apps built from a Git repository list their builds here.</p>
      </div>
    );
  }
  const branch = git.branch || "main";
  const builder = git.builder === "railpack" ? "Railpack" : `Dockerfile ${joinPath(git.path, git.dockerfile || "Dockerfile")}`;
  const list = builds.data ?? [];
  const denied = canDeploy ? undefined : "Your role can view builds but not start them.";

  return (
    <>
      <div className="toolbar">
        <span className="tag" title={git.repository}>{repoLabel(git.repository)}</span>
        <span className="tag">{branch}</span>
        <span className="dim small-note">
          {builder} · rootless BuildKit · {git.connection ? <>via {git.connection}</> : "public repository"} · {git.autoDeploy === false ? "pushes are not deployed" : `every push to ${branch} deploys`}
        </span>
        <button className="btn sm push" disabled={!canDeploy || buildNow.isPending} title={denied ?? `Build the head of ${branch} now`} onClick={() => buildNow.mutate()}>
          <Icon name="play" />{buildNow.isPending ? "Starting…" : "Build now"}
        </button>
      </div>

      {notice && (
        <div className="banner bad" role="alert"><Icon name="alert" /><span>{notice}</span><button className="btn sm" onClick={() => setNotice(undefined)}>Dismiss</button></div>
      )}
      {git.pinnedImage && (
        <div className="banner warn" role="status"><Icon name="alert" /><span>Pinned to an earlier image after a rollback: builds run, but none is deployed until the pin is cleared.</span></div>
      )}
      {builds.isError && <div className="banner bad" role="alert"><Icon name="alert" /><span>{errorText(builds.error)}</span></div>}

      {builds.isPending ? (
        <p className="loading">Loading builds…</p>
      ) : list.length === 0 && !builds.isError ? (
        <div className="empty">
          <h2>No builds yet</h2>
          <p>{git.autoDeploy === false
            ? `Press Build now to build the head of ${branch}.`
            : `Push to ${branch}, or press Build now. Each successful build of ${branch} rolls out as a new revision.`}</p>
        </div>
      ) : list.length > 0 && (
        <div className="card scroll-x">
          <table className="t builds">
            <caption className="sr">Builds of {name}, newest first</caption>
            <thead>
              <tr><th>Build</th><th>Commit</th><th>Author</th><th>Trigger</th><th className="num">Duration</th><th>Status</th><th>Result</th><th><span className="sr">Actions</span></th></tr>
            </thead>
            <tbody>
              {list.map((b) => (
                <tr key={b.name} className={`click${b.name === selected ? " sel" : ""}`} onClick={() => open(b.name)} aria-current={b.name === selected ? "true" : undefined}>
                  <td className="nm">
                    <Link to="/apps/$project/$name" params={{ project, name }} search={{ build: b.name }} onClick={(e) => e.stopPropagation()}>{buildName(b)}</Link>
                    <span className="sub">{ago(b.created)}</span>
                  </td>
                  <td className="commit"><span className="mono">{shortSha(b.commit)}</span><span className="sub ell" title={b.message}>{b.message || "—"}</span></td>
                  <td>{b.author || <span className="dim">—</span>}</td>
                  <td className="dim">{triggerText(b)}</td>
                  <td className="num">{durationText(b.durationSeconds)}</td>
                  <td><BuildStatus build={b} /></td>
                  <td className="dim ell" title={buildResult(b)}>{buildResult(b)}</td>
                  <td>
                    {!finished(b) && canDeploy && !b.cancelRequested && (
                      <button className="btn ghost sm danger" onClick={(e) => { e.stopPropagation(); setCancelling(b); }} aria-label={`Cancel build ${buildName(b)}`}>Cancel</button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {selected && <BuildDetail project={project} name={selected} app={app} canDeploy={canDeploy} onClose={() => open()} onCancel={setCancelling} />}
      {cancelling && (
        <CancelBuildDialog build={cancelling} onClose={() => setCancelling(undefined)}
          onDone={(b) => {
            queryClient.setQueryData(["build", project, b.name], b);
            void queryClient.invalidateQueries({ queryKey: buildsKey(project, name) });
          }} />
      )}
    </>
  );
}

const joinPath = (dir: string | undefined, file: string) => {
  const d = (dir ?? "").replace(/^\/+|\/+$/g, "");
  return `./${d ? d + "/" : ""}${file.replace(/^\.?\/+/, "")}`;
};

function BuildDetail({ project, name, app, canDeploy, onClose, onCancel }: {
  project: string; name: string; app: App; canDeploy: boolean; onClose: () => void; onCancel: (b: Build) => void;
}) {
  const q = useQuery({
    queryKey: ["build", project, name],
    queryFn: () => buildsApi.get(project, name),
    refetchInterval: (query) => (query.state.data && finished(query.state.data) ? false : 3000),
  });

  if (q.isPending) return <div className="card"><div className="bd"><p className="loading">Loading build…</p></div></div>;
  if (q.isError) {
    const gone = q.error instanceof ApiError && q.error.status === 404;
    return (
      <div className="banner bad" role="alert">
        <Icon name="alert" />
        <span>{gone ? `Build ${name} not found. Kwerft removes old builds that no revision runs.` : errorText(q.error)}</span>
        <button className="btn sm" onClick={onClose}>Close</button>
      </div>
    );
  }
  const b = q.data;
  const done = finished(b);
  const url = commitURL(b.source.repository, b.commit);
  const otherApp = b.app !== app.metadata.name;

  return (
    <section className="card build-detail" aria-label={`Build ${buildName(b)}`}>
      <div className="ch-h">
        <h3>Build {buildName(b)} <BuildStatus build={b} /></h3>
        <div className="acts">
          {!done && canDeploy && !b.cancelRequested && <button className="btn sm danger" onClick={() => onCancel(b)}>Cancel build</button>}
          <button className="btn sm" onClick={onClose} aria-label="Close build">Close</button>
        </div>
      </div>
      {b.phase === "failed" && b.statusMessage && (
        <div className="bd"><div className="banner bad" role="alert"><Icon name="alert" /><span><b>Failed</b>: {b.statusMessage}</span></div></div>
      )}
      {b.phase === "pending" && (
        <div className="bd"><div className="banner info" role="status"><Icon name="clock" /><span>{b.statusMessage || "Waiting for a builder. Builds run one at a time on small servers."}</span></div></div>
      )}
      <div className="bd">
        <dl className="kv">
          <dt>Commit</dt>
          <dd>
            {url ? <a href={url} target="_blank" rel="noreferrer"><code>{b.commit}</code></a> : <code>{b.commit}</code>}
            {b.message && <span className="sub-line">{b.message}</span>}
          </dd>
          <dt>Branch</dt><dd>{b.branch || <span className="dim">—</span>}{b.pullRequest ? ` · pull request #${b.pullRequest}` : ""}</dd>
          <dt>Author</dt><dd>{b.author || <span className="dim">—</span>}</dd>
          <dt>Trigger</dt><dd>{triggerText(b)}</dd>
          <dt>Built with</dt><dd>{b.source.builder === "railpack" ? "Railpack" : `Dockerfile ${joinPath(b.source.path, b.source.dockerfile || "Dockerfile")}`}{b.source.connection ? ` · via ${b.source.connection}` : ""}</dd>
          <dt>Started</dt><dd>{b.started ? new Date(b.started).toLocaleString() : <span className="dim">not yet</span>}</dd>
          <dt>Duration</dt><dd>{durationText(b.durationSeconds)}{b.started && !done ? " so far" : ""}</dd>
          <dt>Image</dt><dd>{b.image ? <code>{b.image}</code> : <span className="dim">not pushed yet</span>}{b.digest && <span className="sub-line mono">{b.digest}</span>}</dd>
          <dt>Result</dt>
          <dd>
            {b.deployedRevision ? <>revision {b.deployedRevision}{b.current ? " · running now" : ""}</>
              : !b.deploy ? "A check only: pull request builds are not deployed."
              : buildResult(b)}
            {otherApp && <span className="sub-line">Build of app {b.app}.</span>}
          </dd>
        </dl>
      </div>
      <div className="bd sep">
        <LogViewer path={buildLogsPath(project, b.name)} single follow={!done} height="min(56vh, 560px)"
          downloadName={`${b.app}-build-${b.number || shortSha(b.commit)}`}
          waiting={b.phase === "pending" ? "The log starts when the build does." : "Waiting for the first lines…"} />
      </div>
    </section>
  );
}

function CancelBuildDialog({ build, onClose, onDone }: { build: Build; onClose: () => void; onDone: (b: Build) => void }) {
  const cancel = useMutation({
    mutationFn: () => buildsApi.cancel(build.project, build.name),
    onSuccess: (b) => { onDone(b); onClose(); },
  });
  return (
    <Dialog title={`Cancel build ${buildName(build)}?`} onClose={onClose} onSubmit={() => cancel.mutate()}
      actions={<>
        <button type="button" className="btn" onClick={onClose}>Keep building</button>
        <button className="btn pri danger" disabled={cancel.isPending}>{cancel.isPending ? "Cancelling…" : "Cancel build"}</button>
      </>}>
      <p style={{ margin: 0 }}>Stops the build of <code>{shortSha(build.commit)}</code>{build.message ? ` (${build.message})` : ""}. Nothing is deployed; the app keeps running what it runs now.</p>
      {cancel.isError && <p className="form-error" role="alert">{errorText(cancel.error)}</p>}
    </Dialog>
  );
}
