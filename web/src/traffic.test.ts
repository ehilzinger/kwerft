import { describe, expect, it } from "vitest";
import { count, fromRow, parsePorts, peerLabel, portsLabel, portsText, sendsOut, sideLabel, toRow } from "./traffic";

describe("labels", () => {
  it("names peers relative to the rule's project", () => {
    expect(peerLabel({ app: "api" }, "shop")).toBe("shop/api");
    expect(peerLabel({ app: "internal/cron" }, "shop")).toBe("internal/cron");
    expect(peerLabel({ project: "internal" }, "shop")).toBe("internal/*");
    expect(peerLabel({ internet: true }, "shop")).toBe("internet");
    expect(peerLabel({ cidr: "10.0.0.0/16" }, "shop")).toBe("10.0.0.0/16");
  });
  it("groups ports by protocol", () => {
    expect(portsLabel([])).toBe("any port");
    expect(portsLabel([{ port: 5432 }, { port: 6379, protocol: "TCP" }, { port: 9000, endPort: 9010, protocol: "UDP" }]))
      .toBe("TCP 5432, 6379 · UDP 9000–9010");
  });
  it("describes drop sides", () => {
    expect(sideLabel({ kind: "pod", namespace: "shop", app: "api" })).toBe("shop/api");
    expect(sideLabel({ kind: "pod", namespace: "shop" })).toBe("shop (not an app)");
    expect(sideLabel({ kind: "world", ip: "93.184.216.34", name: "example.com" })).toBe("internet 93.184.216.34 (example.com)");
    expect(sideLabel({ kind: "remote-node" })).toBe("a node (ingress)");
  });
  it("shortens counts", () => {
    expect([count(undefined), count(0), count(999), count(1234), count(96400), count(2_500_000)]).toEqual(["—", "0", "999", "1.2k", "96k", "2.5M"]);
  });
});

describe("editor", () => {
  it("round-trips peers through rows", () => {
    for (const p of [{ app: "api" }, { app: "x/y" }, { project: "shop" }, { internet: true }, { cidr: "10.0.0.0/8" }]) {
      expect(fromRow(toRow(p))).toEqual(p);
    }
  });
  it("parses ports and ranges", () => {
    expect(parsePorts("8080, 5432 9000-9010", "TCP")).toEqual([
      { port: 8080, protocol: "TCP" }, { port: 5432, protocol: "TCP" }, { port: 9000, endPort: 9010, protocol: "TCP" },
    ]);
    expect(parsePorts("", "UDP")).toEqual([]);
    expect(parsePorts("http", "TCP")).toMatch(/not a port/);
    expect(parsePorts("70000", "TCP")).toMatch(/outside/);
    expect(parsePorts("10-5", "TCP")).toMatch(/outside/);
    expect(portsText([{ port: 80 }, { port: 9000, endPort: 9010 }])).toBe("80, 9000-9010");
  });
  it("knows when a rule sends out of its project", () => {
    expect(sendsOut([{ kind: "app", value: "api" }], "shop")).toBe(false);
    expect(sendsOut([{ kind: "app", value: "shop/api" }], "shop")).toBe(false);
    expect(sendsOut([{ kind: "project", value: "shop" }], "shop")).toBe(false);
    expect(sendsOut([{ kind: "app", value: "billing/ledger" }], "shop")).toBe(true);
    expect(sendsOut([{ kind: "internet", value: "" }], "shop")).toBe(true);
    expect(sendsOut([{ kind: "cidr", value: "10.0.0.0/16" }], "shop")).toBe(true);
  });
});
