// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";
import { clusterQuery, filterKey, inCluster, readFilter, writeFilter } from "./clusters";

function memory() {
  const m = new Map<string, string>();
  return {
    getItem: (k: string) => m.get(k) ?? null,
    setItem: (k: string, v: string) => void m.set(k, v),
    removeItem: (k: string) => void m.delete(k),
    m,
  };
}

describe("cluster filter", () => {
  it("is remembered per user", () => {
    const s = memory();
    writeFilter(s, "u1", "edge");
    expect(s.m.get(filterKey("u1"))).toBe("edge");
    expect(readFilter(s, "u1", ["local", "edge"])).toBe("edge");
    expect(readFilter(s, "u2", ["local", "edge"])).toBeUndefined();
  });
  it("forgets a cluster that no longer exists", () => {
    const s = memory();
    writeFilter(s, "u1", "edge");
    expect(readFilter(s, "u1", ["local"])).toBeUndefined();
  });
  it("clears", () => {
    const s = memory();
    writeFilter(s, "u1", "edge");
    writeFilter(s, "u1", undefined);
    expect(s.m.size).toBe(0);
  });
  it("survives blocked storage", () => {
    const blocked = { getItem: () => { throw new Error("blocked"); }, setItem: () => { throw new Error("blocked"); }, removeItem: () => { throw new Error("blocked"); } };
    expect(readFilter(blocked, "u1", ["local"])).toBeUndefined();
    expect(() => writeFilter(blocked, "u1", "edge")).not.toThrow();
  });
});

describe("inCluster", () => {
  const items = [{ name: "a", cluster: "local" }, { name: "b", cluster: "edge" }, { name: "c" }];
  it("keeps everything without a filter", () => expect(inCluster(items).length).toBe(3));
  it("counts items without a cluster as local", () => expect(inCluster(items, "local").map((i) => i.name)).toEqual(["a", "c"]));
  it("filters", () => expect(inCluster(items, "edge").map((i) => i.name)).toEqual(["b"]));
});

describe("clusterQuery", () => {
  it("leaves the local cluster out", () => {
    expect(clusterQuery("local")).toBe("");
    expect(clusterQuery(undefined)).toBe("");
    expect(clusterQuery("edge")).toBe("?cluster=edge");
    expect(clusterQuery("edge", "&")).toBe("&cluster=edge");
  });
});
