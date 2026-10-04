import { useMemo, useState, type ReactNode } from "react";
import { Link, useNavigate } from "@tanstack/react-router";
import { useInfiniteQuery, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api";
import {
  accessApi, ago, canManageMembers, roleLabel, roleNote, roles,
  type AuditEntry, type AuditFilter, type Invite, type IssuedInvite, type Member, type Permission, type Role,
} from "../access";
import { shortDate, when } from "../jobs";
import { Dialog } from "../components/Dialog";
import { Field } from "../components/Field";
import { Icon } from "../components/Icon";
import { errorText } from "./Apps";
import { workloads, type Project } from "../workloads";
import "../styles/workloads.css";
import "../styles/jobs.css";
import "../styles/access.css";

// The Access page: Members | Roles | Audit log | Recordings. Each tab is its
// own route (/access, /access/roles, /access/audit, /access/recordings) and
// renders inside AccessLayout, so the header and tab strip stay the same.

export type AccessTab = "members" | "roles" | "audit" | "recordings";

const tabs: { id: AccessTab; to: string; label: string }[] = [
  { id: "members", to: "/access", label: "Members" },
  { id: "roles", to: "/access/roles", label: "Roles" },
  { id: "audit", to: "/access/audit", label: "Audit log" },
  { id: "recordings", to: "/access/recordings", label: "Recordings" },
];

function useMe() {
  return useQuery({ queryKey: ["session"], queryFn: api.session }).data;
}

const membersQuery = { queryKey: ["members"], queryFn: accessApi.members };

export function AccessTabs({ current }: { current: AccessTab }) {
  const me = useMe();
  const members = useQuery({ ...membersQuery, enabled: canManageMembers(me?.role) });
  return (
    <nav className="tabs" aria-label="Access">
      {tabs.map((t) => (
        <Link key={t.id} to={t.to} className={current === t.id ? "on" : undefined} aria-current={current === t.id ? "page" : undefined}>
          {t.label}
          {t.id === "members" && members.data && <span className="n">{members.data.length}</span>}
        </Link>
      ))}
    </nav>
  );
}

/** Header and tab strip shared by every Access tab, Recordings included. */
export function AccessLayout({ current, actions, children }: { current: AccessTab; actions?: ReactNode; children: ReactNode }) {
  return (
    <section className="view">
      <div className="ph">
        <div>
          <h1>Access</h1>
          <p>Members, roles mapped to Kubernetes RBAC, and an append-only audit log of every change and shell session</p>
        </div>
        {actions && <div className="acts">{actions}</div>}
      </div>
      <AccessTabs current={current} />
      <div className="tabpanel">{children}</div>
    </section>
  );
}

function OwnersAndAdminsOnly({ what, role }: { what: string; role?: Role }) {
  return (
    <div className="empty">
      <h2>Only owners and admins see {what}</h2>
      <p>You are {role === "admin" || role === "owner" ? "an" : "a"} {role ? roleLabel[role].toLowerCase() : "member"}. The Roles tab shows what each role may do; ask an owner if you need more.</p>
    </div>
  );
}

// ---- Members ------------------------------------------------------------------

export function AccessMembers() {
  const me = useMe();
  const manage = canManageMembers(me?.role);
  const members = useQuery({ ...membersQuery, enabled: manage });
  const invites = useQuery({ queryKey: ["invites"], queryFn: accessApi.invites, enabled: manage });
  const [inviting, setInviting] = useState(false);
  const [editing, setEditing] = useState<Member>();
  const [issued, setIssued] = useState<IssuedInvite>();
  const projects = useQuery({ queryKey: ["projects"], queryFn: workloads.projects, enabled: manage });

  if (!me) return <AccessLayout current="members"><p className="loading">Loading…</p></AccessLayout>;
  if (!manage) return <AccessLayout current="members"><OwnersAndAdminsOnly what="the member list" role={me.role} /></AccessLayout>;

  return (
    <AccessLayout current="members" actions={
      <button className="btn pri" onClick={() => setInviting(true)}><Icon name="plus" />Invite member</button>
    }>
      {members.isError && <div className="banner bad" role="alert"><Icon name="alert" /><span>{errorText(members.error)}</span></div>}
      {members.isPending ? <p className="loading">Loading members…</p> : members.data && (
        <div className="card scroll-x">
          <table className="t">
            <thead><tr><th>Member</th><th>Role</th><th>Projects</th><th>Second factor</th><th>Last active</th><th>Joined</th><th><span className="sr">Actions</span></th></tr></thead>
            <tbody>
              {members.data.map((m) => (
                <tr key={m.id}>
                  <td><span className="nm">{m.name}</span>{m.you && <span className="you">you</span>}<span className="sub">{m.email}</span></td>
                  <td>{roleLabel[m.role]}</td>
                  <ProjectsCell m={m} projects={projects.data} />
                  <td><SecondFactor m={m} /></td>
                  <td className="dim nowrap" title={m.lastActive ? new Date(m.lastActive).toLocaleString() : "No active session"}>{m.lastActive ? ago(m.lastActive) : "—"}</td>
                  <td className="dim nowrap">{shortDate(m.createdAt)}</td>
                  <td className="nowrap">
                    {m.manageable
                      ? <button className="btn sm" onClick={() => setEditing(m)} aria-label={`Edit ${m.name}`}>Edit</button>
                      : <span className="dim" title="Only an owner can change an owner">—</span>}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <Invites q={invites} onIssued={setIssued} />
      <SignInPolicyCard owner={me.role === "owner"} />
      <p className="dim note">
        Roles apply at once, also to Kubernetes: the console acts as <code>kwerft:&lt;email&gt;</code> in the group <code>kwerft:role:&lt;role&gt;</code>.
        A project limited to its members (Apps, select the project, Access) gives its members the role chosen there instead.
        Removing a member signs them out everywhere.
      </p>
      {inviting && <InviteDialog myRole={me.role} onClose={() => setInviting(false)} onIssued={(i) => { setInviting(false); setIssued(i); }} />}
      {editing && <EditMemberDialog m={editing} myRole={me.role} onClose={() => setEditing(undefined)} />}
      {issued && <LinkDialog issued={issued} onClose={() => setIssued(undefined)} />}
    </AccessLayout>
  );
}

/** The projects a member reaches: owners and admins all; others every Team project and the Members projects listing them. */
function ProjectsCell({ m, projects }: { m: Member; projects?: Project[] }) {
  if (m.role === "owner" || m.role === "admin") return <td title="Owners and admins reach every project">All</td>;
  if (!projects) return <td className="dim">…</td>;
  const listed = (p: Project) => p.members?.find((x) => x.user.toLowerCase() === m.email.toLowerCase());
  const team = projects.filter((p) => p.access !== "Members");
  const limited = projects.filter((p) => p.access === "Members");
  const mine = limited.filter((p) => listed(p));
  const label = limited.length === 0 ? "All"
    : [team.length > 0 ? `${team.length} team ${team.length === 1 ? "project" : "projects"}` : "", ...mine.map((p) => p.name)].filter(Boolean).join(", ") || "None";
  const title = mine.length > 0
    ? mine.map((p) => `${p.name}: ${listed(p)?.role}`).join(", ")
    : limited.length > 0 ? `Not a member of ${limited.map((p) => p.name).join(", ")}` : "Every project is open to the whole team";
  return <td title={title}>{label}</td>;
}

/** Owners can require a second factor of everyone; members without one are sent to set one up after their password. */
function SignInPolicyCard({ owner }: { owner: boolean }) {
  const queryClient = useQueryClient();
  const policy = useQuery({ queryKey: ["sign-in-policy"], queryFn: accessApi.signInPolicy });
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);
  if (!policy.data) return null;
  const on = policy.data.requireTwoFactor;
  async function toggle() {
    setBusy(true);
    setError(undefined);
    try {
      queryClient.setQueryData(["sign-in-policy"], await accessApi.setSignInPolicy(!on));
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <div className="card">
      <div className="ch-h"><h3>Two-factor sign-in</h3></div>
      <div className="bd stack">
        <p className="note">
          {on
            ? "Required: members without a passkey or authenticator app are sent to set one up right after their password, and nobody can remove their last one."
            : "Optional: each member decides. Members who set one up are asked for it at every sign-in."}
        </p>
        {owner ? (
          <div><button className={on ? "btn" : "btn pri"} disabled={busy} onClick={toggle}>
            {busy ? "Saving…" : on ? "Make it optional" : "Require two-factor sign-in"}
          </button></div>
        ) : <p className="dim note">Only owners change this.</p>}
        {error && <p className="form-error" role="alert">{error}</p>}
      </div>
    </div>
  );
}

function SecondFactor({ m }: { m: Member }) {
  if (m.secondFactor.length === 0) return <span className="pill warn">Not set up</span>;
  const names = m.secondFactor.map((f) => (f === "passkey" ? "Passkey" : "Authenticator app"));
  return <span className="pill ok">{names.join(" + ")}</span>;
}

function Invites({ q, onIssued }: { q: { data?: Invite[]; isError: boolean; error: unknown }; onIssued: (i: IssuedInvite) => void }) {
  const queryClient = useQueryClient();
  const [busy, setBusy] = useState<string>();
  const [error, setError] = useState<string>();
  if (q.isError) return <div className="banner bad" role="alert"><Icon name="alert" /><span>{errorText(q.error)}</span></div>;
  if (!q.data || q.data.length === 0) return null;

  async function act(inv: Invite, what: "reissue" | "revoke") {
    setBusy(inv.id + what);
    setError(undefined);
    try {
      if (what === "reissue") onIssued(await accessApi.reissue(inv.id));
      else await accessApi.revoke(inv.id);
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(undefined);
      await queryClient.invalidateQueries({ queryKey: ["invites"] });
    }
  }

  return (
    <div className="card scroll-x">
      <div className="ch-h"><h3>Pending invites <span className="dim small">{q.data.length}</span></h3></div>
      {error && <p className="form-error pad" role="alert">{error}</p>}
      <table className="t">
        <thead><tr><th>Email</th><th>Role</th><th>Invited by</th><th>Link</th><th><span className="sr">Actions</span></th></tr></thead>
        <tbody>
          {q.data.map((inv) => (
            <tr key={inv.id}>
              <td className="nm">{inv.email}</td>
              <td>{roleLabel[inv.role]}</td>
              <td><span title={inv.invitedByEmail}>{inv.invitedBy}</span><span className="sub">{shortDate(inv.createdAt)}</span></td>
              <td className="nowrap">
                {inv.state === "expired"
                  ? <span className="pill warn">Expired</span>
                  : <span className="dim" title={new Date(inv.expiresAt).toLocaleString()}>expires {when(inv.expiresAt)}</span>}
              </td>
              <td className="nowrap">
                {inv.manageable !== false && (
                  <div className="acts">
                    <button className="btn sm" disabled={!!busy} onClick={() => act(inv, "reissue")} title="A new link; the old one stops working">
                      {busy === inv.id + "reissue" ? "Issuing…" : "New link"}
                    </button>
                    <button className="btn sm danger" disabled={!!busy} onClick={() => act(inv, "revoke")}>
                      {busy === inv.id + "revoke" ? "Revoking…" : "Revoke"}
                    </button>
                  </div>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

function RolePick({ value, onChange, myRole, name }: { value: Role; onChange: (r: Role) => void; myRole: Role; name: string }) {
  return (
    <fieldset className="role-pick">
      <legend>Role</legend>
      {roles.map((r) => {
        const locked = r === "owner" && myRole !== "owner";
        return (
          <label key={r} title={locked ? "Only an owner can make someone an owner" : undefined}>
            <input type="radio" name={name} value={r} checked={value === r} disabled={locked} onChange={() => onChange(r)} />
            <b>{roleLabel[r]}</b>
            <small>{roleNote[r]}</small>
          </label>
        );
      })}
    </fieldset>
  );
}

function InviteDialog({ myRole, onClose, onIssued }: { myRole: Role; onClose: () => void; onIssued: (i: IssuedInvite) => void }) {
  const queryClient = useQueryClient();
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<Role>("developer");
  const [error, setError] = useState<{ field?: string; message: string }>();
  const [busy, setBusy] = useState(false);

  async function submit() {
    setBusy(true);
    setError(undefined);
    try {
      const issued = await accessApi.invite(email, role);
      await queryClient.invalidateQueries({ queryKey: ["invites"] });
      onIssued(issued);
    } catch (e) {
      setError({ field: (e as { field?: string }).field, message: errorText(e) });
      setBusy(false);
    }
  }

  return (
    <Dialog title="Invite a member" onClose={onClose} onSubmit={submit} actions={<>
      <button type="button" className="btn" onClick={onClose}>Cancel</button>
      <button className="btn pri" disabled={busy || email.trim() === ""}>{busy ? "Creating link…" : "Create invite link"}</button>
    </>}>
      <Field id="invite-email" label="Email" type="email" value={email} onChange={(e) => setEmail(e.target.value)}
        autoComplete="off" autoFocus required error={error?.field === "email" ? error.message : undefined} />
      <RolePick value={role} onChange={setRole} myRole={myRole} name="invite-role" />
      {error && error.field !== "email" && <p className="form-error" role="alert">{error.message}</p>}
      <p className="dim note">Kwerft does not send email yet: you get a link to pass on. It works once and expires in 7 days.</p>
    </Dialog>
  );
}

function LinkDialog({ issued, onClose }: { issued: IssuedInvite; onClose: () => void }) {
  const [copied, setCopied] = useState(false);
  async function copy() {
    try {
      await navigator.clipboard.writeText(issued.url);
      setCopied(true);
    } catch {
      /* clipboard refused: the link stays selectable */
    }
  }
  const inv = issued.invite;
  return (
    <Dialog title="Invite link" onClose={onClose} onSubmit={onClose} actions={<button className="btn pri" autoFocus>Done</button>}>
      <p className="note">
        Send this link to <b>{inv.email}</b>{inv.sent ? " (it was also emailed)" : ""}. It makes them {inv.role === "admin" || inv.role === "owner" ? "an" : "a"} <b>{roleLabel[inv.role].toLowerCase()}</b>,
        works once and expires {when(inv.expiresAt)}.
      </p>
      <div className="linkbox">
        <code>{issued.url}</code>
        <button type="button" className="btn sm" onClick={copy}>{copied ? "Copied" : "Copy"}</button>
      </div>
      <p className="dim note">This is the only time the link is shown. Lost it? Use “New link” in the list; the old one then stops working.</p>
    </Dialog>
  );
}

function EditMemberDialog({ m, myRole, onClose }: { m: Member; myRole: Role; onClose: () => void }) {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const [role, setRole] = useState<Role>(m.role);
  const [confirmRemove, setConfirmRemove] = useState(false);
  const [confirmReset, setConfirmReset] = useState(false);
  const [resetDone, setResetDone] = useState(false);
  const [error, setError] = useState<string>();
  const [busy, setBusy] = useState(false);

  async function resetFactors() {
    setBusy(true);
    setError(undefined);
    try {
      await accessApi.resetSecondFactor(m.id);
      setResetDone(true);
      setConfirmReset(false);
      await queryClient.invalidateQueries({ queryKey: ["members"] });
    } catch (e) {
      setError(errorText(e));
    } finally {
      setBusy(false);
    }
  }

  async function done() {
    await queryClient.invalidateQueries({ queryKey: ["members"] });
    if (m.you) await queryClient.invalidateQueries({ queryKey: ["session"] });
    onClose();
  }

  async function save() {
    if (role === m.role) return onClose();
    setBusy(true);
    setError(undefined);
    try {
      await accessApi.setRole(m.id, role);
      await done();
    } catch (e) {
      setError(errorText(e));
      setBusy(false);
    }
  }

  async function remove() {
    setBusy(true);
    setError(undefined);
    try {
      await accessApi.remove(m.id);
      if (m.you) {
        queryClient.clear();
        await navigate({ to: "/login", search: { next: "/" } });
        return;
      }
      await done();
    } catch (e) {
      setError(errorText(e));
      setBusy(false);
    }
  }

  return (
    <Dialog title={m.you ? "Your role" : `Edit ${m.name}`} onClose={onClose} onSubmit={save} actions={<>
      <button type="button" className="btn" onClick={onClose}>Cancel</button>
      <button className="btn pri" disabled={busy || role === m.role}>{busy ? "Saving…" : "Save role"}</button>
    </>}>
      <p className="dim note">{m.email} · joined {shortDate(m.createdAt)}</p>
      <RolePick value={role} onChange={setRole} myRole={myRole} name="member-role" />
      {m.you && role !== m.role && <p className="note warn-text">You are changing your own role. It applies at once.</p>}
      {error && <p className="form-error" role="alert">{error}</p>}
      {!m.you && (m.secondFactor.length > 0 || resetDone) && (
        <div className="danger-zone">
          <p>
            Lost phone or security key? Resetting removes {m.name}'s passkeys, authenticator app and recovery codes and signs them out
            everywhere. They sign in with their password and set up a second factor again.
          </p>
          {resetDone ? <p className="note">Done: {m.name} sets up a second factor at the next sign-in.</p> : confirmReset ? (
            <div className="confirm-row">
              <button type="button" className="btn pri danger" disabled={busy} onClick={resetFactors}>{busy ? "Resetting…" : "Yes, reset second factors"}</button>
              <button type="button" className="btn" onClick={() => setConfirmReset(false)}>Keep</button>
            </div>
          ) : (
            <div><button type="button" className="btn danger" onClick={() => setConfirmReset(true)}><Icon name="key" />Reset second factors</button></div>
          )}
        </div>
      )}
      <div className="danger-zone">
        <p>
          {m.you ? "Leaving the team" : "Removing a member"} deletes the account with its passkeys and authenticator app and
          signs {m.you ? "you" : "them"} out everywhere at once. The audit log keeps what {m.you ? "you" : "they"} did.
        </p>
        {confirmRemove ? (
          <div className="confirm-row">
            <button type="button" className="btn pri danger" disabled={busy} onClick={remove}>
              {busy ? "Removing…" : m.you ? "Yes, remove my account" : `Yes, remove ${m.name}`}
            </button>
            <button type="button" className="btn" onClick={() => setConfirmRemove(false)}>Keep</button>
          </div>
        ) : (
          <div><button type="button" className="btn danger" onClick={() => setConfirmRemove(true)}>
            <Icon name="trash" />{m.you ? "Leave the team" : "Remove from team"}
          </button></div>
        )}
      </div>
    </Dialog>
  );
}

// ---- Roles --------------------------------------------------------------------

export function AccessRoles() {
  const matrix = useQuery({ queryKey: ["roles"], queryFn: accessApi.roles, staleTime: Infinity });
  const shell = matrix.data?.permissions.find((p) => p.id === "shell");
  return (
    <AccessLayout current="roles">
      {matrix.isError && <div className="banner bad" role="alert"><Icon name="alert" /><span>{errorText(matrix.error)}</span></div>}
      {matrix.isPending ? <p className="loading">Loading roles…</p> : matrix.data && (
        <>
          <div className="card scroll-x">
            <table className="t matrix">
              <thead><tr><th>Permission</th>{roles.map((r) => <th key={r} className="c">{roleLabel[r]}</th>)}</tr></thead>
              <tbody>
                {matrix.data.permissions.map((p) => (
                  <tr key={p.id}>
                    <td>{p.label}<Enforced p={p} /></td>
                    {roles.map((r) => <Cell key={r} g={p.grants[r]} />)}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div className="card scroll-x">
            <div className="ch-h"><h3>How roles reach Kubernetes</h3></div>
            <table className="t">
              <thead><tr><th>Role</th><th>Kubernetes group</th><th>Cluster role</th><th>In each project namespace it reaches</th></tr></thead>
              <tbody>
                {matrix.data.roles.map((r) => (
                  <tr key={r.role}>
                    <td>{roleLabel[r.role]}</td>
                    <td className="mono">{r.group}</td>
                    <td className="mono">{r.clusterRole}</td>
                    <td className="mono">{r.projectRole && `${r.projectRole}, `}kwerft:pods-read{shell && shell.grants[r.role].level !== "no" && ", kwerft:pods-exec"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="dim note">
            The console reaches Kubernetes as <code>kwerft:&lt;email&gt;</code> in the role's group, so Kubernetes RBAC is the final say for
            everything marked “Kubernetes”; the rest the console checks itself. Owners and admins reach every project. Developers and
            viewers reach the projects open to the whole team with their role, and projects limited to members only when listed, with
            the role given there (the project's RoleBindings name them as <code>kwerft:&lt;email&gt;</code>).
          </p>
        </>
      )}
    </AccessLayout>
  );
}

function Enforced({ p }: { p: Permission }) {
  return p.enforcedBy === "kubernetes"
    ? <span className="tag by" title="Enforced by Kubernetes RBAC on the impersonated identity">Kubernetes</span>
    : <span className="tag by" title="Enforced by the console API">console</span>;
}

function Cell({ g }: { g: Permission["grants"][Role] }) {
  if (g.level === "yes") return <td className="c yes" aria-label="Yes">✓</td>;
  if (g.level === "partial") return <td className="c part">{g.note}</td>;
  return <td className="c no" aria-label="No">—</td>;
}

// ---- Audit log ----------------------------------------------------------------

const pageSize = 50;

export function AccessAudit() {
  const me = useMe();
  const allowed = canManageMembers(me?.role);
  const [filter, setFilter] = useState<AuditFilter>({});
  const facets = useQuery({ queryKey: ["audit-facets"], queryFn: accessApi.auditFacets, enabled: allowed });
  const members = useQuery({ ...membersQuery, enabled: allowed });
  const log = useInfiniteQuery({
    queryKey: ["audit", filter],
    queryFn: ({ pageParam }) => accessApi.audit(filter, pageParam, pageSize),
    initialPageParam: undefined as number | undefined,
    getNextPageParam: (last) => last.next ?? undefined,
    enabled: allowed,
  });
  const names = useMemo(() => new Map((members.data ?? []).map((m) => [m.email.toLowerCase(), m.name])), [members.data]);
  const groups = useMemo(() => {
    const prefixes = new Set((facets.data?.actions ?? []).map((a) => a.split(".")[0] + "."));
    return [...prefixes].sort();
  }, [facets.data]);

  if (!me) return <AccessLayout current="audit"><p className="loading">Loading…</p></AccessLayout>;
  if (!allowed) return <AccessLayout current="audit"><OwnersAndAdminsOnly what="the audit log" role={me.role} /></AccessLayout>;

  const who = (actor: string) => names.get(actor.toLowerCase()) ?? actor;
  const entries = log.data?.pages.flatMap((p) => p.entries) ?? [];
  const filtered = !!(filter.actor || filter.action);

  return (
    <AccessLayout current="audit">
      <div className="audit-tools">
        <label className="sr" htmlFor="audit-actor">Who</label>
        <select id="audit-actor" className="input" value={filter.actor ?? ""} onChange={(e) => setFilter({ ...filter, actor: e.target.value || undefined })}>
          <option value="">Everyone</option>
          {(facets.data?.actors ?? []).map((a) => <option key={a} value={a}>{who(a) === a ? a : `${who(a)} (${a})`}</option>)}
        </select>
        <label className="sr" htmlFor="audit-action">Action</label>
        <select id="audit-action" className="input" value={filter.action ?? ""} onChange={(e) => setFilter({ ...filter, action: e.target.value || undefined })}>
          <option value="">All actions</option>
          <optgroup label="Kinds">
            {groups.map((g) => <option key={g} value={g}>{g}*</option>)}
          </optgroup>
          <optgroup label="Actions">
            {(facets.data?.actions ?? []).map((a) => <option key={a} value={a}>{a}</option>)}
          </optgroup>
        </select>
        {filtered && <button className="btn sm ghost" onClick={() => setFilter({})}>Clear filters</button>}
      </div>
      {log.isError && <div className="banner bad" role="alert"><Icon name="alert" /><span>{errorText(log.error)}</span></div>}
      {log.isPending ? <p className="loading">Loading the audit log…</p> : (
        <div className="card scroll-x">
          <table className="t">
            <thead><tr><th>Time (UTC)</th><th>Who</th><th>Action</th><th>Target</th><th>From</th></tr></thead>
            <tbody>
              {entries.length === 0 && <tr><td colSpan={5} className="dim">{filtered ? "No entries match these filters." : "Nothing recorded yet."}</td></tr>}
              {entries.map((e) => <AuditRow key={e.id} e={e} who={who(e.actor)} />)}
            </tbody>
          </table>
          {log.hasNextPage && (
            <div className="audit-more">
              <button className="btn sm" disabled={log.isFetchingNextPage} onClick={() => log.fetchNextPage()}>
                {log.isFetchingNextPage ? "Loading…" : "Load older entries"}
              </button>
            </div>
          )}
        </div>
      )}
      <p className="dim note">Append-only: entries cannot be edited or deleted from the console. Sign-ins, membership changes, every change to apps and jobs, and shell sessions are recorded.</p>
    </AccessLayout>
  );
}

function AuditRow({ e, who }: { e: AuditEntry; who: string }) {
  return (
    <tr>
      <td className="mono nowrap" title={new Date(e.at).toLocaleString()}>{utcTime(e.at)}</td>
      <td title={e.actor}>{who}</td>
      <td className="mono">{e.action}</td>
      <td className="mono">{e.target}{e.detail && <span className="detail">{e.detail}</span>}</td>
      <td className="mono">{e.ip || "—"}</td>
    </tr>
  );
}

const months = ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"];
const two = (n: number) => String(n).padStart(2, "0");

/** As the blueprint: 09:12:44 today, 03 Oct 16:40 before, with the year when it differs. UTC throughout. */
function utcTime(iso: string, now = new Date()) {
  const t = new Date(iso);
  const sameDay = t.getUTCFullYear() === now.getUTCFullYear() && t.getUTCMonth() === now.getUTCMonth() && t.getUTCDate() === now.getUTCDate();
  const hm = `${two(t.getUTCHours())}:${two(t.getUTCMinutes())}`;
  if (sameDay) return `${hm}:${two(t.getUTCSeconds())}`;
  const year = t.getUTCFullYear() !== now.getUTCFullYear() ? ` ${t.getUTCFullYear()}` : "";
  return `${two(t.getUTCDate())} ${months[t.getUTCMonth()]}${year} ${hm}`;
}
