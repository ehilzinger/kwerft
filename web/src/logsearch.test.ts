// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";
import { entryKey, logParams, mergeEntries, parseSSE, sourceOf, type LogEntry } from "./logsearch";

const e = (time: string, line: string, more: Partial<LogEntry> = {}): LogEntry => ({
  time, line, namespace: "shop", pod: "web-1", container: "app", ...more,
});

describe("logParams and clusters", () => {
  it("names a cluster only without a project, and never the local one", () => {
    expect(logParams({ cluster: "edge" })).toBe("cluster=edge");
    expect(logParams({ cluster: "local" })).toBe("");
    expect(logParams({ cluster: "edge", project: "shop" })).toBe("project=shop");
  });
});

describe("logParams", () => {
  it("leaves out empty values", () => {
    expect(logParams({})).toBe("");
    expect(logParams({ query: "  ", level: "", platform: false })).toBe("");
  });
  it("encodes a full filter", () => {
    const q = new URLSearchParams(logParams({ query: 'status:500 "timed out"', project: "shop", app: "web", level: "error", since: "1h", limit: 200 }));
    expect(Object.fromEntries(q)).toEqual({ query: 'status:500 "timed out"', project: "shop", app: "web", level: "error", since: "1h", limit: "200" });
  });
  it("sends an app only with its project, and platform only without one", () => {
    expect(logParams({ app: "web" })).toBe("");
    expect(logParams({ platform: true })).toBe("platform=1");
    expect(logParams({ platform: true, project: "shop" })).toBe("project=shop");
  });
});

describe("mergeEntries", () => {
  it("sorts by time and drops repeats from page boundaries and the tail", () => {
    const older = [e("2026-10-04T09:00:00Z", "a"), e("2026-10-04T09:00:01Z", "b")];
    const page = [e("2026-10-04T09:00:01Z", "b"), e("2026-10-04T09:00:02Z", "c")];
    expect(mergeEntries(page, older).map((x) => x.line)).toEqual(["a", "b", "c"]);
  });
  it("keeps identical lines from different pods, and the newest max", () => {
    const lines = [e("2026-10-04T09:00:00Z", "x"), e("2026-10-04T09:00:00Z", "x", { pod: "web-2" }), e("2026-10-04T09:00:03Z", "y")];
    expect(mergeEntries(lines, []).length).toBe(3);
    expect(mergeEntries(lines, [], 2).map((x) => x.line)).toEqual(["x", "y"]);
    expect(entryKey(lines[0]!)).not.toBe(entryKey(lines[1]!));
  });
});

describe("sourceOf", () => {
  it("names the app, build, task or namespace", () => {
    expect(sourceOf(e("t", "l", { project: "shop", app: "web" }))).toBe("shop/web");
    expect(sourceOf(e("t", "l", { namespace: "kwerft-builds", project: "shop", build: "web-4" }))).toBe("shop · build web-4");
    expect(sourceOf(e("t", "l", { project: "shop", task: "nightly-x" }))).toBe("shop · task nightly-x");
    expect(sourceOf(e("t", "l", { namespace: "kube-system" }))).toBe("kube-system");
  });
});

describe("parseSSE", () => {
  it("returns complete events and keeps the rest", () => {
    const { events, rest } = parseSSE('event: start\ndata: {}\n\n: ping\n\nevent: line\ndata: {"line":"a"}\n\nevent: li');
    expect(events).toEqual([{ name: "start", data: "{}" }, { name: "line", data: '{"line":"a"}' }]);
    expect(rest).toBe("event: li");
  });
});
