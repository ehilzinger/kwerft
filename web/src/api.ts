// Thin client for the Kwerft REST API. Every call goes through here so auth,
// error shapes and status handling live in one place. Session cookies are
// HttpOnly; the browser sends them, this code never sees them.

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
export type User = { id: string; email: string; name: string; role: "owner" | "admin" | "developer" | "viewer" };
export type SetupStatus = { complete: boolean };

export const api = {
  version: () => request<VersionInfo>("/version"),

  setupStatus: () => request<SetupStatus>("/setup"),
  setupVerify: (token: string) => request<void>("/setup/verify", { method: "POST", json: { token } }),
  setupOwner: (owner: { name: string; email: string; password: string }) =>
    request<User>("/setup/owner", { method: "POST", json: owner }),

  session: () => request<User>("/session"),
  login: (email: string, password: string) => request<User>("/session", { method: "POST", json: { email, password } }),
  logout: () => request<void>("/session", { method: "DELETE" }),
};

export const isUnauthorized = (e: unknown) => e instanceof ApiError && e.status === 401;
