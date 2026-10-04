import { describe, expect, it } from "vitest";
import { clusterNameProblem, clusterState, nodesText } from "./clusters";

describe("clusters", () => {
  it.each<[string, boolean]>([
    ["edge", true],
    ["edge-1", true],
    ["a", true],
    ["Edge", false],
    ["1edge", false],
    ["edge-", false],
    ["local", false],
    ["connect", false],
    ["", false],
    ["a".repeat(40), true],
    ["a".repeat(41), false],
  ])("name %s ok=%s", (name, ok) => {
    expect(clusterNameProblem(name) === undefined).toBe(ok);
  });

  it("phrases the state", () => {
    expect(clusterState({ phase: "Connected", connected: true, provider: "local" })).toEqual({ label: "Running", tone: "ok" });
    expect(clusterState({ phase: "Pending", connected: true, provider: "adopted" })).toEqual({ label: "Connected", tone: "ok" });
    expect(clusterState({ phase: "Disconnected", connected: false, provider: "adopted" }).tone).toBe("bad");
    expect(clusterState({ phase: "Provisioning", connected: false, provider: "hetzner-cloud" }).label).toBe("Creating");
    expect(clusterState({ phase: "Pending", connected: false, provider: "adopted" }).label).toBe("Waiting for agent");
    expect(clusterState({ phase: "Connected", connected: true, provider: "adopted", deleting: true }).label).toBe("Deleting");
  });

  it("counts nodes", () => {
    expect(nodesText({ nodes: 0, readyNodes: 0, connected: false })).toBe("—");
    expect(nodesText({ nodes: 3, readyNodes: 3, connected: true })).toBe("3");
    expect(nodesText({ nodes: 3, readyNodes: 2, connected: false, lastSeen: "2026-10-05T00:00:00Z" })).toBe("2 / 3 ready");
  });
});
