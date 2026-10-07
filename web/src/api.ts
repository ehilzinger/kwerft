// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Thin client for the Kwerft REST API. Every call goes through here so auth,
// error shapes and status handling live in one place. Session cookies are
// HttpOnly; the browser sends them, this code never sees them.

import type { PasskeyOptions } from "./webauthn";

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
    /** Form field the error is about, when the server names one. */
    public field?: string,
  ) {
    super(message);
  }
}

export async function request<T>(path: string, init?: RequestInit & { json?: unknown }): Promise<T> {
  const { json, ...rest } = init ?? {};
  const res = await fetch(`/api/v1${path}`, {
    ...rest,
    body: json === undefined ? rest.body : JSON.stringify(json),
    headers: {
      Accept: "application/json",
      ...(json === undefined ? {} : { "Content-Type": "application/json" }),
      ...rest.headers,
    },
    credentials: "same-origin",
  });
  if (!res.ok) {
    let message = `${res.status} ${res.statusText}`;
    let field: string | undefined;
    try {
      const body = (await res.json()) as { error?: string; field?: string };
      message = body.error ?? message;
      field = body.field;
    } catch {
      /* non-JSON error body */
    }
    throw new ApiError(res.status, message, field);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export type VersionInfo = { version: string; commit: string; platform: "cloud" | "dedicated" };
/** mustEnrol: the console requires a second factor this user has not set up; only the Account page works until they do. */
export type User = { id: string; email: string; name: string; role: "owner" | "admin" | "developer" | "viewer"; mustEnrol?: boolean };
/** consoleDomain: where the console is served (Settings can move it). */
export type SetupStatus = { complete: boolean; consoleDomain?: string };
/** A right password for an account with a second factor: these methods can finish the sign-in. */
export type SecondFactorNeeded = { secondFactor: SecondFactor[] };
export type SecondFactor = "passkey" | "totp" | "recovery";

export const api = {
  version: () => request<VersionInfo>("/version"),

  setupStatus: () => request<SetupStatus>("/setup"),
  setupVerify: (token: string) => request<void>("/setup/verify", { method: "POST", json: { token } }),
  setupOwner: (owner: { name: string; email: string; password: string }) =>
    request<User>("/setup/owner", { method: "POST", json: owner }),

  session: () => request<User>("/session"),
  login: (email: string, password: string) =>
    request<User | SecondFactorNeeded>("/session", { method: "POST", json: { email, password } }),
  logout: () => request<void>("/session", { method: "DELETE" }),
};

export const isUnauthorized = (e: unknown) => e instanceof ApiError && e.status === 401;

// ---- second factors and the account page ----------------------------------

export type Passkey = { id: string; name: string; createdAt: string; lastUsedAt: string | null };
export type AccountSession = { id: string; current: boolean; createdAt: string; lastSeenAt: string; ip: string; userAgent: string };
/** An account at a single sign-on provider linked to this one. */
export type SSOIdentity = { issuer: string; email: string; linkedAt: string; lastLoginAt: string | null };
export type Account = {
  user: User;
  /** False for accounts that only ever signed in with single sign-on. */
  hasPassword: boolean;
  identities: SSOIdentity[];
  totp: boolean;
  passkeys: Passkey[];
  recoveryCodesLeft: number;
  sessions: AccountSession[];
  available: { totp: boolean; passkeys: boolean };
};
export type TOTPSetup = { secret: string; uri: string; qr: string };
/** Recovery codes come back only when they were (re)generated: the one time they are shown. */
export type MaybeCodes = { recoveryCodes?: string[] };

const post = <T,>(path: string, json?: unknown) => request<T>(path, { method: "POST", json: json ?? {} });

export const mfaApi = {
  totp: (code: string) => post<User>("/session/second-factor/totp", { code }),
  recovery: (code: string) => post<User>("/session/second-factor/recovery", { code }),
  passkeyBegin: () => post<PasskeyOptions>("/session/second-factor/passkey/begin"),
  passkeyFinish: (credential: unknown) => post<User>("/session/second-factor/passkey/finish", credential),
  /** Passwordless sign-in with a discoverable passkey. */
  passwordlessBegin: () => post<PasskeyOptions>("/session/passkey/begin"),
  passwordlessFinish: (credential: unknown) => post<User>("/session/passkey/finish", credential),
};

export const accountApi = {
  get: () => request<Account>("/account"),
  rename: (name: string) => request<User>("/account", { method: "PATCH", json: { name } }),
  /** current is empty when an account without a password sets one (soon after signing in with single sign-on). */
  changePassword: (current: string, next: string) => post<{ signedOut: number }>("/account/password", { current, new: next }),
  totpStart: (password: string) => post<TOTPSetup>("/account/totp", { password }),
  totpConfirm: (code: string) => post<MaybeCodes>("/account/totp/confirm", { code }),
  totpDisable: (password: string) => post<void>("/account/totp/disable", { password }),
  regenerateCodes: (password: string) => post<Required<MaybeCodes>>("/account/recovery-codes", { password }),
  passkeyBegin: (password: string) => post<PasskeyOptions>("/account/passkeys/begin", { password }),
  passkeyFinish: (name: string, credential: unknown) =>
    post<{ passkey: Passkey } & MaybeCodes>("/account/passkeys/finish", { name, credential }),
  renamePasskey: (id: string, name: string) => request<void>(`/account/passkeys/${encodeURIComponent(id)}`, { method: "PATCH", json: { name } }),
  removePasskey: (id: string, password: string) =>
    request<void>(`/account/passkeys/${encodeURIComponent(id)}`, { method: "DELETE", json: { password } }),
  signOutOthers: () => request<{ signedOut: number }>("/account/sessions", { method: "DELETE" }),
  signOutSession: (id: string) => request<void>(`/account/sessions/${encodeURIComponent(id)}`, { method: "DELETE" }),
};
