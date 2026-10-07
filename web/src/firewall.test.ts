// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";
import { countdown, describePorts, describeSources, parseSourcesText, ruleProblem, sourceContains, sshCovers } from "./firewall";

describe("firewall", () => {
  it("describes ports", () => {
    expect(describePorts({ port: 22, protocol: "TCP" })).toBe("22");
    expect(describePorts({ port: 8000, endPort: 8100, protocol: "TCP" })).toBe("8000–8100");
    expect(describePorts({ port: 1, endPort: 65535, protocol: "TCP" })).toBe("all");
    expect(describePorts({ port: 1, protocol: "ICMP" })).toBe("—");
    expect(describeSources([])).toBe("any");
  });

  it("splits typed sources", () => {
    expect(parseSourcesText(" 203.0.113.7, 198.51.100.0/24\n2001:db8::/32 ;")).toEqual(["203.0.113.7", "198.51.100.0/24", "2001:db8::/32"]);
  });

  it.each<[string, string, boolean]>([
    ["203.0.113.0/24", "203.0.113.7", true],
    ["203.0.113.7", "203.0.113.7", true],
    ["203.0.113.0/25", "203.0.113.200", false],
    ["0.0.0.0/0", "198.51.100.1", true],
    ["2001:db8::/32", "2001:db8:1::5", true],
    ["2001:db8::/32", "2001:db9::5", false],
    ["2001:db8::/32", "203.0.113.7", false],
    ["nonsense", "203.0.113.7", false],
  ])("sourceContains(%s, %s) = %s", (source, ip, want) => {
    expect(sourceContains(source, ip)).toBe(want);
  });

  it("checks SSH coverage", () => {
    expect(sshCovers([], "203.0.113.7")).toBe(true);
    expect(sshCovers(["198.51.100.0/24", "203.0.113.0/24"], "203.0.113.7")).toBe(true);
    expect(sshCovers(["198.51.100.0/24"], "203.0.113.7")).toBe(false);
  });

  it("pre-checks the form", () => {
    expect(ruleProblem({ name: "game", port: "27015", endPort: "" }, true)).toBeUndefined();
    expect(ruleProblem({ name: "Game", port: "27015", endPort: "" }, true)?.field).toBe("name");
    expect(ruleProblem({ name: "ssh", port: "27015", endPort: "" }, true)?.field).toBe("name");
    expect(ruleProblem({ name: "game", port: "0", endPort: "" }, true)?.field).toBe("port");
    expect(ruleProblem({ name: "game", port: "9000", endPort: "8000" }, true)?.field).toBe("endPort");
    expect(ruleProblem({ name: "", port: "9000", endPort: "" }, false)).toBeUndefined();
  });

  it("counts down", () => {
    expect(countdown(59.6)).toBe("1:00");
    expect(countdown(42)).toBe("0:42");
    expect(countdown(-3)).toBe("0:00");
  });
});
