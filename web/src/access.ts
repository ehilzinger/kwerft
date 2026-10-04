// Members, invites, roles and the audit log: the API behind the Access page
// and the public invite page.

import { request, type User } from "./api";

export type Role = User["role"];
export const roles: Role[] = ["owner", "admin", "developer", "viewer"];
export const roleLabel: Record<Role, string> = { owner: "Owner", admin: "Admin", developer: "Developer", viewer: "Viewer" };
export const roleNote: Record<Role, string> = {
  owner: "Everything, including managing owners",
  admin: "Everything except managing owners",
  developer: "Deploy and run apps and jobs, open shells",
  viewer: "Read only",
};

export type Member = {
  id: string;
  name: string;
  email: string;
  role: Role;
  createdAt: string;
  lastActive: string | null;
  secondFactor: ("passkey" | "totp")[];
  you: boolean;
  /** Whether the signed-in user may change this member (admins: not owners). */
  manageable: boolean;
};

export type InviteState = "open" | "expired" | "accepted" | "revoked";
export type Invite = {
  id: string;
  email: string;
  role: Role;
  invitedBy: string;
  invitedByEmail: string;
  createdAt: string;
  expiresAt: string;
  state: InviteState;
  /** Delivered by email; false while links are passed on by hand. */
  sent: boolean;
  manageable?: boolean;
};
/** The link is in the response only this once. */
export type IssuedInvite = { invite: Invite; url: string };

export type InviteInfo = { email: string; role: Role; invitedBy: string; expiresAt: string };

export type Level = "yes" | "no" | "partial";
export type Permission = {
  id: string;
  label: string;
  enforcedBy: "kubernetes" | "console";
  grants: Record<Role, { level: Level; note?: string }>;
};
export type RoleMatrix = { roles: { role: Role; group: string; clusterRole: string }[]; permissions: Permission[] };

export type AuditEntry = { id: number; at: string; actor: string; action: string; target: string; ip: string; detail: string };
export type AuditPage = { entries: AuditEntry[]; next: number | null };
export type AuditFilter = { actor?: string; action?: string };

const id = encodeURIComponent;

export const accessApi = {
  members: () => request<Member[]>("/members"),
  setRole: (memberId: string, role: Role) => request<User>(`/members/${id(memberId)}`, { method: "PATCH", json: { role } }),
  remove: (memberId: string) => request<void>(`/members/${id(memberId)}`, { method: "DELETE" }),

  invites: () => request<Invite[]>("/invites"),
  invite: (email: string, role: Role) => request<IssuedInvite>("/invites", { method: "POST", json: { email, role } }),
  reissue: (inviteId: string) => request<IssuedInvite>(`/invites/${id(inviteId)}/reissue`, { method: "POST", json: {} }),
  revoke: (inviteId: string) => request<void>(`/invites/${id(inviteId)}`, { method: "DELETE" }),

  // Public: the invite page. The token goes in the body, not the URL.
  lookupInvite: (token: string) => request<InviteInfo>("/invite/lookup", { method: "POST", json: { token } }),
  acceptInvite: (token: string, name: string, password: string) =>
    request<User>("/invite/accept", { method: "POST", json: { token, name, password } }),

  roles: () => request<RoleMatrix>("/roles"),

  audit: (f: AuditFilter, before?: number, limit = 50) => {
    const q = new URLSearchParams({ limit: String(limit) });
    if (f.actor) q.set("actor", f.actor);
    if (f.action) q.set("action", f.action);
    if (before) q.set("before", String(before));
    return request<AuditPage>(`/audit?${q}`);
  },
  auditFacets: () => request<{ actors: string[]; actions: string[] }>("/audit/facets"),
};

/** Who may manage members: owners fully, admins all but owners. */
export const canManageMembers = (role?: Role) => role === "owner" || role === "admin";

/** "now", "12 min ago", "5 h ago", "3 d ago". */
export function ago(iso: string, now = Date.now()) {
  const s = Math.max(0, Math.floor((now - new Date(iso).getTime()) / 1000));
  if (s < 120) return "now";
  if (s < 3600) return `${Math.floor(s / 60)} min ago`;
  if (s < 86400) return `${Math.floor(s / 3600)} h ago`;
  return `${Math.floor(s / 86400)} d ago`;
}
