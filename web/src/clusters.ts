// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Clusters as the console's lists see them (Phase 5, docs/phase5.md): every
// list item carries the cluster its project lives in, and GET
// /cluster-status says which clusters exist and which can be reached now.
// The Clusters pages (W3) manage clusters; this is only what every page
// needs to show and filter by them. With only "local", nothing shows.
import { useCallback, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api, request } from "./api";

export const LOCAL = "local";

export type ClusterStatus = { name: string; connected: boolean };

export const clustersApi = {
  status: () => request<{ clusters: ClusterStatus[] }>("/cluster-status"),
};

/** "?cluster=edge" for API paths that take one; "" for the local cluster. */
export function clusterQuery(cluster?: string, sep: "?" | "&" = "?") {
  return cluster && cluster !== LOCAL ? `${sep}cluster=${encodeURIComponent(cluster)}` : "";
}

/** The clusters and whether there is more than the local one. */
export function useClusters() {
  const q = useQuery({ queryKey: ["cluster-status"], queryFn: clustersApi.status, refetchInterval: 15000, retry: false });
  const clusters = q.data?.clusters ?? [{ name: LOCAL, connected: true }];
  return {
    clusters,
    /** More than the local cluster: show cluster columns, filters and pickers. */
    multi: clusters.length > 1,
    unreachable: clusters.filter((c) => !c.connected).map((c) => c.name),
  };
}

// ---- the cluster filter, remembered per user in this browser ------------------

export const filterKey = (userId: string) => `kwerft:cluster-filter:${userId}`;

type Store = Pick<Storage, "getItem" | "setItem" | "removeItem">;

/** The remembered filter, if that cluster still exists. */
export function readFilter(store: Store | undefined, userId: string | undefined, known: string[]): string | undefined {
  if (!store || !userId) return undefined;
  try {
    const v = store.getItem(filterKey(userId)) ?? undefined;
    return v && known.includes(v) ? v : undefined;
  } catch {
    return undefined; // storage blocked
  }
}

export function writeFilter(store: Store | undefined, userId: string | undefined, cluster: string | undefined) {
  if (!store || !userId) return;
  try {
    if (cluster) store.setItem(filterKey(userId), cluster);
    else store.removeItem(filterKey(userId));
  } catch {
    /* storage blocked: the filter lasts for this page only */
  }
}

const storage = () => (typeof window !== "undefined" ? window.localStorage : undefined);

/** The cluster filter of list pages: undefined = all clusters. */
export function useClusterFilter(): [string | undefined, (c?: string) => void] {
  const session = useQuery({ queryKey: ["session"], queryFn: api.session });
  const { clusters, multi } = useClusters();
  const userId = session.data?.id;
  const [chosen, setChosen] = useState<string | null>(null); // null: not chosen on this page yet
  const remembered = readFilter(storage(), userId, clusters.map((c) => c.name));
  const set = useCallback((c?: string) => {
    setChosen(c ?? "");
    writeFilter(storage(), userId, c);
  }, [userId]);
  if (!multi) return [undefined, set];
  const current = chosen === null ? remembered : chosen || undefined;
  return [current, set];
}

/** Items of the filtered cluster (all without a filter). Items from before Phase 5 have no cluster: local. */
export function inCluster<T extends { cluster?: string }>(items: T[], cluster?: string): T[] {
  return cluster ? items.filter((i) => (i.cluster || LOCAL) === cluster) : items;
}
