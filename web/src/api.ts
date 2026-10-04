// Thin client for the Kwerft REST API. Every call goes through here so auth,
// error shapes and (later) CSRF handling live in one place.

export class ApiError extends Error {
  constructor(public status: number, message: string) {
    super(message);
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`/api/v1${path}`, {
    ...init,
    headers: { Accept: "application/json", ...init?.headers },
    credentials: "same-origin",
  });
  if (!res.ok) {
    let message = res.statusText;
    try {
      message = ((await res.json()) as { error?: string }).error ?? message;
    } catch {
      /* non-JSON error body */
    }
    throw new ApiError(res.status, message);
  }
  return (await res.json()) as T;
}

export type VersionInfo = { version: string; commit: string; platform: "cloud" | "dedicated" };

export const api = {
  version: () => request<VersionInfo>("/version"),
};
