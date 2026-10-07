// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";
import {
  controlPlaneCountProblem, diskHealthText, diskTone, labelsText, nodesWithDisks, parseLabels, percentText, phaseTone, poolSummary, priceText,
  raidText, smartText, type ClusterNode, type Disk, type Pool,
} from "./nodes";

const pool = (p: Partial<Pool>): Pool => ({
  name: "prod-web", pool: "web", role: "worker", serverType: "cx23", location: "fsn1", count: 3, desired: 3, ready: 2,
  labels: {}, deleting: false, state: "progressing", servers: [], ...p,
});

describe("nodes", () => {
  it("summarises pools", () => {
    expect(poolSummary(pool({}))).toBe("2 of 3 ready");
    expect(poolSummary(pool({ role: "builds", desired: 0, ready: 0, count: 2 }))).toBe("Idle · up to 2");
    expect(poolSummary(pool({ role: "builds", desired: 1, ready: 1, count: 2, servers: [{ name: "x", phase: "Ready" }] }))).toBe("1 of 1 ready · up to 2");
    expect(poolSummary(pool({ deleting: true }))).toBe("Removing its servers");
  });

  it("checks control-plane counts", () => {
    expect(controlPlaneCountProblem(0, 3)).toBeUndefined();
    expect(controlPlaneCountProblem(1, 2)).toBeUndefined();
    expect(controlPlaneCountProblem(1, 1)).toMatch(/2 control-plane nodes/);
    expect(controlPlaneCountProblem(0, 0)).toMatch(/at least one/);
  });

  it("parses labels", () => {
    expect(parseLabels("tier=web\n disk = nvme ,")).toEqual({ labels: { tier: "web", disk: "nvme" } });
    expect(parseLabels("oops").error).toMatch(/not key=value/);
    expect(labelsText({ a: "1", b: "2" })).toBe("a=1\nb=2");
  });

  it("phrases phases and prices", () => {
    expect(phaseTone("Ready")).toBe("ok");
    expect(phaseTone("Joining")).toBe("warn");
    expect(phaseTone("Failed")).toBe("bad");
    expect(priceText({ name: "cx23", description: "", cores: 2, memory: 4, disk: 40, cpuType: "shared", architecture: "x86", locations: ["fsn1"], prices: { fsn1: "4.7000" } }, "fsn1")).toBe("€4.70/month");
    expect(priceText(undefined, "fsn1")).toBe("");
  });

  it("phrases disk health", () => {
    expect(diskTone("bad")).toBe("bad");
    expect(diskTone("unknown")).toBe("warn");
    expect(diskHealthText("unknown")).toBe("No readings");
    expect(raidText({ device: "md1", state: "active", active: 2, required: 2, failed: 0, spare: 0, health: "ok" })).toBe("2 of 2 disks active");
    expect(raidText({ device: "md1", state: "recovering", active: 1, required: 2, failed: 1, spare: 1, health: "bad" }))
      .toBe("1 of 2 disks active · 1 failed · 1 spare · recovering");
    expect(percentText(114.6)).toBe("115 %");
    expect(percentText(undefined)).toBe("—");
    const disk = (d: Partial<Disk>): Disk => ({ device: "nvme0", health: "ok", problems: [], ...d });
    expect(smartText(disk({ smartPassed: false }))).toBe("FAILED");
    expect(smartText(disk({ smartPassed: true, unreadable: true }))).toBe("Unreadable");
    expect(smartText(disk({}))).toBe("—");
    const node = (n: Partial<ClusterNode>): ClusterNode => ({ name: "n", roles: [], ready: true, status: "Ready", unschedulable: false, ...n });
    const none = { health: "ok" as const, summary: "", smart: false, arrays: [], disks: [] };
    expect(nodesWithDisks([
      node({ name: "cloud" }),
      node({ name: "cloud-raid", platform: "cloud", diskHealth: { ...none, arrays: [{ device: "md0", state: "active", active: 2, required: 2, failed: 0, spare: 0, health: "ok" }] } }),
      node({ name: "dedi", platform: "dedicated", diskHealth: { ...none, health: "unknown" } }),
    ]).map((n) => n.name)).toEqual(["cloud-raid", "dedi"]);
  });
});
