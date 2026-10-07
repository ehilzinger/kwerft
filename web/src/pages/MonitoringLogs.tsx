// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { useQuery } from "@tanstack/react-query";
import { getRouteApi, useNavigate } from "@tanstack/react-router";
import { api } from "../api";
import { LogSearch, type SearchState } from "../components/LogSearch";
import { isLevel, isRange } from "../logsearch";
import { MonitoringLayout } from "./Monitoring";

const route = getRouteApi("/authed/monitoring/logs");

/** The URL's search params for Monitoring › Logs (router.tsx validates them). */
export type LogsSearch = { query?: string; project?: string; app?: string; level?: string; range?: string; platform?: boolean; cluster?: string };

export function logsSearch(s: Record<string, unknown>): LogsSearch {
  const str = (k: string) => (typeof s[k] === "string" && s[k] ? { [k]: s[k] as string } : {});
  return {
    ...str("query"), ...str("project"), ...str("app"), ...str("cluster"),
    ...(isLevel(s.level) && s.level ? { level: s.level } : {}),
    ...(isRange(s.range) ? { range: s.range } : {}),
    ...(s.platform === true || s.platform === "1" || s.platform === 1 ? { platform: true } : {}),
  };
}

// Monitoring › Logs (Phase 3, W2): search the log history of every project
// the user may read — also pods that are gone — and follow it live. Owners
// and admins may include the platform's namespaces. The filter lives in the
// URL, so a search can be shared.
export function MonitoringLogs() {
  const search = route.useSearch();
  const navigate = useNavigate();
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const canPlatform = session.data?.role === "owner" || session.data?.role === "admin";

  const initial: Partial<SearchState> = {
    query: search.query ?? "", project: search.project ?? "", app: search.app ?? "",
    level: isLevel(search.level) ? search.level : "", range: isRange(search.range) ? search.range : "1h", platform: !!search.platform,
    cluster: search.cluster ?? "",
  };
  const onChange = (s: SearchState) => {
    void navigate({
      to: "/monitoring/logs",
      search: logsSearch({ query: s.query, project: s.project, app: s.app, level: s.level, range: s.range === "1h" ? undefined : s.range, platform: s.platform,
        cluster: s.project ? undefined : s.cluster }),
      replace: true,
    });
  };

  return (
    <MonitoringLayout current="logs">
      <LogSearch initial={initial} onChange={onChange} canPlatform={canPlatform} height="min(66vh, 720px)" />
    </MonitoringLayout>
  );
}
