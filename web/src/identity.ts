// Identity: single sign-on (the sign-in button, linked identities and the
// owner's settings), API tokens and kubeconfigs, and the data key that
// encrypts authenticator-app secrets.

import { request, type SecondFactor } from "./api";
import { roles, type Role } from "./access";

// ---- single sign-on ------------------------------------------------------------

export type SSOProvider = "google" | "microsoft" | "keycloak" | "oidc";

/** Public: whether the sign-in page offers single sign-on, and its button label. */
export type SSOStatus = { enabled: false } | { enabled: true; provider: SSOProvider; displayName: string };

export type SSOConfig = {
  enabled: boolean;
  provider: SSOProvider;
  issuer: string;
  tenant?: string;
  clientId: string;
  displayName: string;
  allowedDomains: string[];
  autoJoin: boolean;
  defaultRole: "developer" | "viewer";
};

/** available: false while the console runs without its cluster connection. */
export type SSOSettings = { sso: SSOConfig | null; secretSet: boolean; redirectUrl: string; available: boolean };

/** clientSecret empty: keep the stored one. */
export type SSOInput = Omit<SSOConfig, "tenant"> & { tenant: string; clientSecret: string };

export const providers: SSOProvider[] = ["google", "microsoft", "keycloak", "oidc"];
export const providerLabel: Record<SSOProvider, string> = {
  google: "Google",
  microsoft: "Microsoft Entra ID",
  keycloak: "Keycloak",
  oidc: "Other OpenID Connect provider",
};
/** What the sign-in button says when no label is set. */
export const providerButton: Record<SSOProvider, string> = {
  google: "Google",
  microsoft: "Microsoft",
  keycloak: "Keycloak",
  oidc: "single sign-on",
};
/** Where to register the console at the provider. */
export const providerHint: Record<SSOProvider, string | undefined> = {
  google: "Google Cloud console › APIs & Services › Credentials › OAuth client ID (Web application); add the redirect URL.",
  microsoft: "Entra admin center › App registrations › New registration (single tenant); add the redirect URL as a Web platform redirect URI and create a client secret.",
  keycloak: "Clients › Create client (OpenID Connect, client authentication on, standard flow); add the redirect URL as a valid redirect URI.",
  oidc: undefined,
};

const ssoErrors: Record<string, string> = {
  limited: "Too many sign-in attempts. Wait 15 minutes and try again.",
  unavailable: "Single sign-on is not available right now. Sign in with your password.",
  provider: "The sign-in provider could not be reached. Try again in a moment.",
  expired: "The sign-in took too long or was started in another browser. Try again.",
  denied: "The sign-in was cancelled or refused at the provider.",
  unverified: "The provider did not confirm your email address, so Kwerft cannot match it to an account.",
  domain: "Your email address is not in a domain this console admits.",
  not_member: "There is no account for your email address. Ask an owner or admin for an invite.",
  conflict: "Your account is already linked to a different account at this provider.",
  failed: "Single sign-on failed. Try again, or sign in with your password.",
};

/** The message for /login?sso=<code>; unknown codes read as a plain failure. */
export function ssoErrorMessage(code: string) {
  return Object.hasOwn(ssoErrors, code) ? ssoErrors[code]! : ssoErrors.failed!;
}

const factors: SecondFactor[] = ["passkey", "totp", "recovery"];

/** "passkey,totp" from /login?second-factor=…, known methods only, in the given order. */
export function parseSecondFactors(v: string | null | undefined): SecondFactor[] | undefined {
  const list = (v ?? "").split(",").map((s) => s.trim()).filter((s): s is SecondFactor => (factors as string[]).includes(s));
  const unique = [...new Set(list)];
  return unique.length ? unique : undefined;
}

/**
 * What single sign-on left in the sign-in page's query: the second factors
 * still to ask for, or why it failed. search is the query without them, so
 * a reload does not show them again; changed says whether there were any.
 */
export function readLoginQuery(search: string): { methods?: SecondFactor[]; ssoError?: string; search: string; changed: boolean } {
  const q = new URLSearchParams(search);
  const changed = q.has("second-factor") || q.has("sso");
  const methods = q.has("second-factor") ? parseSecondFactors(q.get("second-factor")) : undefined;
  const code = q.get("sso");
  q.delete("second-factor");
  q.delete("sso");
  const rest = q.toString();
  return { methods, ssoError: code === null ? undefined : ssoErrorMessage(code), search: rest ? `?${rest}` : "", changed };
}

/** "example.com, @Example.org other.net" → ["example.com", "example.org", "other.net"]. */
export function parseDomains(s: string): string[] {
  const out = s.split(/[\s,;]+/).map((d) => d.trim().toLowerCase().replace(/^@/, "").replace(/\.$/, "")).filter(Boolean);
  return [...new Set(out)];
}

/** The host of an issuer URL, for lists: https://login.microsoftonline.com/<tenant>/v2.0 → login.microsoftonline.com. */
export function issuerHost(issuer: string) {
  try {
    return new URL(issuer).host || issuer;
  } catch {
    return issuer;
  }
}

// ---- API tokens and kubeconfigs ------------------------------------------------

export type APIToken = {
  id: string;
  name: string;
  kind: "api" | "kubeconfig";
  /** The token's start, like kwft_AbCdEf…: enough to recognize it. */
  hint: string;
  role: Role;
  /** Empty: all projects. */
  projects: string[];
  createdAt: string;
  expiresAt: string;
  lastUsedAt: string | null;
  lastUsedIp: string;
  expired: boolean;
};
export type TokenList = { tokens: APIToken[]; maxDays: number; defaultDays: number };
export type TokenInput = { name: string; role: Role; projects: string[]; expiresInDays: number };
/** The token is in the response only this once. */
export type IssuedToken = { token: string; apiToken: APIToken };
export type IssuedKubeconfig = { kubeconfig: string; filename: string; apiToken: APIToken };

/** The roles a token may carry: the user's own and those below it; developer or viewer when limited to projects. */
export function tokenRoles(mine: Role, limited: boolean): Role[] {
  const below = roles.slice(Math.max(0, roles.indexOf(mine)));
  return limited ? below.filter((r) => r === "developer" || r === "viewer") : below;
}

/** "shop, web api" → ["shop", "web", "api"]: projects typed by hand when the list could not load. */
export function parseNames(s: string): string[] {
  return [...new Set(s.split(/[\s,;]+/).map((x) => x.trim()).filter(Boolean))];
}

/** A whole number of days within 1..max, or a message why not. */
export function daysProblem(s: string, max: number): string | undefined {
  const t = s.trim();
  if (!/^\d+$/.test(t) || Number(t) < 1 || Number(t) > max) return `Enter a number of days from 1 to ${max}.`;
  return undefined;
}

/** "All projects", or the projects a token is limited to. */
export const describeProjects = (projects: string[]) => (projects.length ? projects.join(", ") : "All projects");

/** Hands text to the browser as a file download. */
export function downloadText(text: string, filename: string, type: string) {
  const url = URL.createObjectURL(new Blob([text], { type }));
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.style.display = "none";
  document.body.append(a);
  a.click();
  a.remove();
  // Revoked a moment later: some browsers start the download asynchronously.
  window.setTimeout(() => URL.revokeObjectURL(url), 1000);
}

// ---- data key -----------------------------------------------------------------

export type DataKey =
  | { available: false }
  | { available: true; keyId: string; otherKeyIds: string[]; sealed: number; upToDate: number; canRotate: boolean; secret: string };
export type DataKeyRotated = { keyId: string; previousKeyId: string; resealed: number; unreadable: number; previousRetired: boolean };

// ---- client ---------------------------------------------------------------------

const post = <T,>(path: string, json: unknown) => request<T>(path, { method: "POST", json });

export const identityApi = {
  sso: () => request<SSOStatus>("/sso"),
  unlink: (issuer: string, password: string) => post<void>("/account/sso/unlink", { issuer, password }),

  tokens: () => request<TokenList>("/account/tokens"),
  createToken: (t: TokenInput) => post<IssuedToken>("/account/tokens", t),
  revokeToken: (id: string) => request<void>(`/account/tokens/${encodeURIComponent(id)}`, { method: "DELETE" }),
  kubeconfig: (t: TokenInput) => post<IssuedKubeconfig>("/account/kubeconfig", t),

  ssoSettings: () => request<SSOSettings>("/settings/sso"),
  saveSSO: (s: SSOInput) => request<SSOSettings>("/settings/sso", { method: "PUT", json: s }),
  dataKey: () => request<DataKey>("/settings/data-key"),
  rotateDataKey: (password: string) => post<DataKeyRotated>("/settings/data-key/rotate", { password }),
};

/** Shown where an account without a password may leave the password empty. */
export const ssoPasswordHint = "Leave empty if you signed in with single sign-on in the last 10 minutes.";
