// Builds from Git (docs/phase2.md): an App's builds, their logs, and the Git
// connections that give Kwerft access to private repositories. The server
// acts as the signed-in user, so a 403 is Kubernetes RBAC talking.
import { ApiError, request } from "./api";
import { PROJECT_RE } from "./workloads";

// ---- builds ------------------------------------------------------------------

export type BuildPhase = "pending" | "running" | "succeeded" | "failed" | "cancelled";
export type Builder = "dockerfile" | "railpack";

export type Build = {
  name: string;
  project: string;
  /** #1, #2, … per app; 0 until the build controller numbers it. */
  number: number;
  app: string;
  source: { repository: string; path?: string; builder: Builder; dockerfile?: string; connection?: string };
  commit: string;
  branch?: string;
  /** The commit's subject line. */
  message?: string;
  author?: string;
  trigger: "push" | "manual" | "pull-request";
  requestedBy?: string;
  pullRequest?: number;
  deploy: boolean;
  phase: BuildPhase;
  /** Why it failed or waits ("Queued behind 1 build"). */
  statusMessage?: string;
  cancelRequested?: boolean;
  image?: string;
  digest?: string;
  created: string;
  started?: string;
  finished?: string;
  durationSeconds?: number;
  /** The newest app revision that ran this build; current: it runs now. */
  deployedRevision?: number;
  current?: boolean;
};

const enc = encodeURIComponent;
const appPath = (project: string, app: string) => `/projects/${enc(project)}/apps/${enc(app)}`;
const buildPath = (project: string, name: string) => `/projects/${enc(project)}/builds/${enc(name)}`;

export const buildsApi = {
  list: (project: string, app: string) => request<Build[]>(`${appPath(project, app)}/builds`),
  get: (project: string, name: string) => request<Build>(buildPath(project, name)),
  cancel: (project: string, name: string) => request<Build>(`${buildPath(project, name)}/cancel`, { method: "POST" }),
  /** Build the branch head (or one commit) now. */
  buildNow: (project: string, app: string, commit?: string) =>
    request<Build>(`${appPath(project, app)}/builds`, { method: "POST", json: commit ? { commit } : {} }),
};

/** Log stream path for LogViewer. */
export const buildLogsPath = (project: string, name: string) => `${buildPath(project, name)}/logs`;

export const finished = (b: Pick<Build, "phase">) => b.phase === "succeeded" || b.phase === "failed" || b.phase === "cancelled";

// ---- Git connections ---------------------------------------------------------

export type GitProvider = "github" | "gitlab" | "gitea" | "generic";
export type GitAuth = "none" | "token" | "sshKey" | "githubApp";

export type Connection = {
  name: string;
  provider: GitProvider;
  url: string;
  auth: GitAuth;
  owner?: string;
  /** Projects that may build with it; empty means all. */
  projects: string[];
  /** Who the credentials act as. */
  account?: string;
  ready: boolean;
  message?: string;
  webhookURL?: string;
  /** Kwerft registered the webhook at the host itself. */
  webhookAutomatic: boolean;
  lastDelivery?: string;
  githubApp?: { appID: number; installationID: number; slug?: string };
};

export type ConnectionInput = {
  name: string;
  provider: GitProvider;
  url: string;
  auth: GitAuth;
  owner?: string;
  projects?: string[];
  token?: string;
  sshPrivateKey?: string;
  knownHosts?: string;
  githubApp?: { appID: number; installationID: number; privateKey?: string };
};

export type Commit = { sha: string; message: string; author: string; time?: string };

export type CheckQuery = { repository: string; branch?: string; connection?: string; path?: string; dockerfile?: string };
export type CheckResult = {
  ok: boolean;
  message: string;
  defaultBranch?: string;
  head?: Commit;
  /** Whether the Dockerfile exists, when asked about one. */
  dockerfile?: boolean;
  /** The connection the check used (picked by the server when none was named). */
  connection?: string;
};

export const gitApi = {
  connections: () => request<Connection[]>("/git/connections"),
  create: (c: ConnectionInput) => request<{ connection: Connection; webhookSecret: string }>("/git/connections", { method: "POST", json: c }),
  update: (name: string, c: ConnectionInput) => request<Connection>(`/git/connections/${enc(name)}`, { method: "PUT", json: c }),
  remove: (name: string) => request<void>(`/git/connections/${enc(name)}`, { method: "DELETE" }),
  /** A new webhook secret; shown once. */
  rotateSecret: (name: string) => request<{ webhookSecret: string }>(`/git/connections/${enc(name)}/webhook-secret`, { method: "POST" }),
  check: (q: CheckQuery) => request<CheckResult>("/git/check", { method: "POST", json: q }),
};

/** The console does not offer this endpoint (yet): an older server. */
export const notOffered = (e: unknown) => e instanceof ApiError && (e.status === 404 || e.status === 405 || e.status === 501);

export const providers: { id: GitProvider; label: string; url: string }[] = [
  { id: "github", label: "GitHub", url: "https://github.com" },
  { id: "gitlab", label: "GitLab", url: "https://gitlab.com" },
  { id: "gitea", label: "Gitea / Forgejo", url: "" },
  { id: "generic", label: "Other Git host", url: "" },
];

export const authLabels: Record<GitAuth, string> = {
  githubApp: "GitHub App",
  token: "Access token",
  sshKey: "SSH deploy key",
  none: "No credentials",
};

/** Authentication a provider supports, most capable first. */
export function authOptions(p: GitProvider): GitAuth[] {
  return p === "github" ? ["githubApp", "token", "sshKey", "none"] : ["token", "sshKey", "none"];
}

/** What a connection can do besides cloning, for the add dialog. */
export function authNote(a: GitAuth): string {
  switch (a) {
    case "githubApp": return "Clones, registers the push webhook itself and reports build status on commits.";
    case "token": return "Clones over HTTPS, registers the push webhook and reports build status on commits.";
    case "sshKey": return "Clones over SSH with a deploy key. Add the push webhook at the host yourself; no commit status.";
    case "none": return "Public repositories only. Add the push webhook at the host yourself; no commit status.";
  }
}

export function webhookHint(c: Pick<Connection, "webhookAutomatic" | "webhookURL" | "provider">): string {
  if (!c.webhookURL) return "No webhook URL yet: the console needs its public hostname.";
  if (c.webhookAutomatic) return "Kwerft registers this webhook on the repositories it builds.";
  const where = c.provider === "github" || c.provider === "gitlab" ? "Settings → Webhooks" : "the webhook settings";
  return `Add it in ${where} of each repository (push events; pull requests too for checks), with the webhook secret.`;
}

/** Client-side checks for the connection form; the server has the last word. */
export function connectionProblem(c: ConnectionInput, creating: boolean): { field: string; message: string } | undefined {
  if (creating && (!PROJECT_RE.test(c.name) || c.name.length > 63)) {
    return { field: "name", message: "Use lowercase letters, digits and dashes, like github-acme." };
  }
  if (!/^https:\/\/[^/\s]+(\/\S*)?$/.test(c.url)) return { field: "url", message: "Enter the host's address, like https://gitlab.example.com." };
  if (c.auth === "githubApp" && c.provider !== "github") return { field: "auth", message: "GitHub Apps work with GitHub only." };
  if (creating && c.auth === "token" && !c.token?.trim()) return { field: "token", message: "Paste the access token." };
  if (creating && c.auth === "sshKey" && !c.sshPrivateKey?.trim()) return { field: "sshPrivateKey", message: "Paste the deploy key's private key." };
  if (c.sshPrivateKey?.trim() && !/-----BEGIN [A-Z ]*PRIVATE KEY-----/.test(c.sshPrivateKey)) {
    return { field: "sshPrivateKey", message: "That is not a private key. It starts with -----BEGIN OPENSSH PRIVATE KEY-----." };
  }
  if (c.auth === "githubApp") {
    const app = c.githubApp;
    if (!app || !Number.isInteger(app.appID) || app.appID <= 0) return { field: "githubApp.appID", message: "Enter the App ID, a number from the app's settings page." };
    if (!Number.isInteger(app.installationID) || app.installationID <= 0) return { field: "githubApp.installationID", message: "Enter the installation ID, the number at the end of the installation's URL." };
    if (creating && !app.privateKey?.trim()) return { field: "githubApp.privateKey", message: "Paste the app's private key (.pem)." };
  }
  return undefined;
}

// ---- repositories -------------------------------------------------------------

const SCP = /^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[A-Za-z0-9._~%+-]+(\/[A-Za-z0-9._~%+-]+)*\/?$/;
const HTTPS = /^https:\/\/[A-Za-z0-9.-]+(:\d{1,5})?(\/[A-Za-z0-9._~%+-]+)+\/?$/;
const SSH = /^ssh:\/\/([A-Za-z0-9._-]+@)?[A-Za-z0-9.-]+(:\d{1,5})?(\/[A-Za-z0-9._~%+-]+)+\/?$/;
const DOTS = /(^|\/)\.\.(\/|$)/;

/**
 * What people paste, as Kwerft stores it: "github.com/acme/api" becomes
 * https://github.com/acme/api; SSH forms stay as they are.
 */
export function normalizeRepository(input: string): string {
  let s = input.trim();
  if (!s) return s;
  if (/^[a-z][a-z0-9+.-]*:\/\//i.test(s)) s = s.replace(/^[a-z]+/i, (m) => m.toLowerCase());
  else if (!SCP.test(s) && /^[A-Za-z0-9-]+(\.[A-Za-z0-9-]+)+(:\d+)?\//.test(s)) s = "https://" + s;
  return s.replace(/\/+$/, "");
}

/** What is wrong with a repository URL, or "". Mirrors the server (api_builds.go). */
export function repositoryProblem(repo: string): string {
  if (!repo) return "Enter the repository's URL, like https://github.com/acme/api.git or git@github.com:acme/api.git.";
  if (repo.startsWith("http://")) return "Use https:// or SSH (git@host:owner/repo.git); Kwerft does not clone over plain HTTP.";
  if (repo.startsWith("https://") && repo.slice(8).split("/")[0]!.includes("@")) return "Leave credentials out of the URL. Add a Git connection in Settings instead.";
  if (DOTS.test(repo)) return 'The URL must not contain "..".';
  if (HTTPS.test(repo) || SSH.test(repo) || SCP.test(repo)) return "";
  return "Use https://host/owner/repo.git or git@host:owner/repo.git.";
}

/** Follows git check-ref-format for the names people use; "" when fine. */
export function branchProblem(branch: string): string {
  if (!branch) return "";
  const bad = branch.length > 255 || !/^[A-Za-z0-9._/+@-]+$/.test(branch) || branch.includes("..") || branch.includes("//") ||
    /^[-/.]/.test(branch) || /[/.]$/.test(branch) || branch.endsWith(".lock") || branch.includes("@{");
  return bad ? `"${branch}" is not a valid branch name.` : "";
}

/** A path inside the repository (build context or Dockerfile); "" when fine. */
export function repoPathProblem(path: string): string {
  if (path.length > 255 || /[\x00-\x1f\x7f\\]/.test(path)) return "Use a path inside the repository, like / or services/api.";
  if (DOTS.test(path)) return 'The path must stay inside the repository: no "..".';
  return "";
}

/** Host of a repository URL in any of its forms ("github.com"). */
export function repoHost(repo: string): string {
  const scp = /^[^@/]+@([^:/]+):/.exec(repo);
  if (scp) return scp[1]!.toLowerCase();
  const url = /^[a-z]+:\/\/(?:[^@/]+@)?([^:/]+)/i.exec(repo);
  return url ? url[1]!.toLowerCase() : "";
}

/** "https://github.com/acme/api.git" or "git@github.com:acme/api.git" → "github.com/acme/api". */
export function repoLabel(repo: string): string {
  const host = repoHost(repo);
  if (!host) return repo;
  const path = repo.replace(/^[^@/]+@[^:/]+:/, "").replace(/^[a-z]+:\/\/(?:[^@/]+@)?[^/]+\/?/i, "").replace(/\.git$/, "").replace(/\/+$/, "");
  return `${host}/${path}`;
}

/** The connection that most likely grants access to repo: same host, and owner if it has one. */
export function suggestConnection(repo: string, conns: Connection[], project?: string): string {
  const host = repoHost(repo);
  if (!host) return "";
  const owner = repoLabel(repo).split("/")[1]?.toLowerCase();
  const usable = conns.filter((c) => !project || c.projects.length === 0 || c.projects.includes(project));
  const sameHost = usable.filter((c) => repoHost(c.url) === host);
  const exact = sameHost.find((c) => c.owner && c.owner.toLowerCase() === owner);
  return (exact ?? sameHost.find((c) => !c.owner))?.name ?? "";
}

// ---- presentation ---------------------------------------------------------------

export const shortSha = (sha?: string) => (sha ? sha.slice(0, 7) : "—");

/** 59 → "59 s", 91 → "1m 31s", 3725 → "1 h 02m". */
export function durationText(seconds?: number): string {
  if (seconds === undefined || seconds < 0) return "—";
  const s = Math.round(seconds);
  if (s < 60) return `${s} s`;
  if (s < 3600) return `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, "0")}s`;
  return `${Math.floor(s / 3600)} h ${String(Math.floor((s % 3600) / 60)).padStart(2, "0")}m`;
}

/** The status pill of a build. */
export function buildLook(b: Build): { cls: "ok" | "info" | "warn" | "bad" | "mute"; label: string } {
  if (b.cancelRequested) return { cls: "mute", label: "Cancelling" };
  switch (b.phase) {
    case "pending": return { cls: "info", label: "Queued" };
    case "running": return { cls: "info", label: "Building" };
    case "failed": return { cls: "bad", label: "Failed" };
    case "cancelled": return { cls: "mute", label: "Cancelled" };
    case "succeeded":
      if (b.current) return { cls: "ok", label: "Deployed" };
      if (b.deployedRevision) return { cls: "mute", label: "Superseded" };
      return { cls: "ok", label: b.deploy ? "Built" : "Passed" };
  }
  return { cls: "info", label: "Queued" };
}

/** The "Result" column: what came of the build. */
export function buildResult(b: Build): string {
  if (b.deployedRevision) return `revision ${b.deployedRevision}`;
  if (b.phase === "failed" || b.phase === "cancelled") return b.statusMessage || "—";
  if (b.phase === "succeeded" && !b.deploy) return b.pullRequest ? `check for PR #${b.pullRequest}` : "not deployed";
  if (b.phase === "succeeded") return "not deployed yet";
  if (b.phase === "pending") return b.statusMessage || "waiting for a builder";
  return b.cancelRequested ? "stopping…" : "—";
}

export function triggerText(b: Build): string {
  switch (b.trigger) {
    case "manual": return b.requestedBy ? `Build now · ${b.requestedBy}` : "Build now";
    case "pull-request": return b.pullRequest ? `PR #${b.pullRequest}` : "pull request";
    default: return b.requestedBy ? `push · ${b.requestedBy}` : "push";
  }
}

/** The commit's page at well-known hosts (GitHub, GitLab, Gitea, Forgejo, Codeberg); undefined elsewhere. */
export function commitURL(repo: string, sha: string): string | undefined {
  const host = repoHost(repo);
  if (!host || !/^[0-9a-f]{7,40}$/.test(sha)) return undefined;
  const base = `https://${repoLabel(repo)}`;
  if (host === "gitlab.com" || host.startsWith("gitlab.")) return `${base}/-/commit/${sha}`;
  if (host === "github.com" || host === "codeberg.org" || /^(gitea|forgejo)\./.test(host)) return `${base}/commit/${sha}`;
  return undefined;
}

/** Removes terminal colour and cursor codes, which logs carry from tools that think they talk to a terminal. */
export function stripAnsi(text: string): string {
  return text.includes("\x1b") ? text.replace(/\x1b(\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(\x07|\x1b\\)|[@-Z\\-_])/g, "") : text;
}
