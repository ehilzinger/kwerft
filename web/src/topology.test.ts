import { describe, expect, it } from "vitest";
import {
  attentionTarget, between, buildModel, fit, focusOf, layout, peerId, problemsOf, sideId, volumeName,
  type Layers, type TopoApp, type Topology, type View,
} from "./topology";

const allLayers: Layers = { jobs: true, volumes: true, domains: true, rules: true, firewall: true };
const traffic = (over: Partial<View> = {}): View => ({ lens: "traffic", layers: allLayers, collapsed: new Set(), ...over });

function app(project: string, name: string, over: Partial<TopoApp> = {}): TopoApp {
  return {
    name, project, cluster: "local", source: { type: "image", image: "nginx" }, image: "nginx", readyReplicas: 1, replicas: 1, stateful: false,
    phase: "running", urls: [], revision: 3, updated: "2026-10-05T10:00:00Z", ports: [], allowFrom: [], egress: "https", mounts: [],
    pods: [{ name: `${name}-0`, node: "n1", status: "Running", tone: "ok", ready: true, restarts: 0 }], ...over,
  } as TopoApp;
}

function shop(): Topology {
  return {
    cluster: "local",
    projects: [{ name: "shop", isolated: true, placed: true }, { name: "tools", isolated: true, placed: true }],
    apps: [
      app("shop", "web", { pods: [{ name: "web-0", node: "n1", status: "Running", tone: "ok", ready: true, restarts: 0 }, { name: "web-1", node: "n2", status: "Running", tone: "ok", ready: true, restarts: 0 }], replicas: 2, readyReplicas: 2 }),
      app("shop", "api"),
      app("shop", "db", { mounts: [{ volume: "db/data-0", path: "/var/lib/postgresql/data", readOnly: false }] }),
      app("shop", "worker", { readyReplicas: 0, pods: [{ name: "worker-0", node: "n2", status: "CrashLoopBackOff", tone: "bad", ready: false, restarts: 7 }] }),
      app("tools", "n8n"),
    ],
    schedules: [{ name: "nightly", project: "shop", schedule: "0 2 * * *", suspend: false, concurrency: "Forbid", fromApp: "api", restart: [], active: [], phase: "scheduled", created: "", node: "n2" }],
    tasks: [],
    volumes: [{ name: "db/data-0", project: "shop", size: "10Gi", class: "local-nvme", phase: "bound", usedBy: ["db"], created: "", own: true, app: "db", node: "n1", usedBytes: 9 * 2 ** 30, capacityBytes: 10 * 2 ** 30 }],
    domains: [{ name: "web", project: "shop", hostname: "web.example.com", app: "web", certificate: "valid", created: "" }],
    rules: [
      { project: "shop", name: "web-to-api", from: [{ app: "web" }], to: [{ app: "api" }], ports: [{ port: 8080 }], disabled: false, phase: "ready", policies: [], generation: 1, created: "", counts: { allowed: 4100, dropped: 0 } },
      { project: "shop", name: "api-to-db", from: [{ app: "api" }], to: [{ app: "db" }], ports: [{ port: 5432 }], disabled: false, phase: "ready", policies: [], generation: 1, created: "" },
      { project: "tools", name: "flows", from: [{ app: "n8n" }], to: [{ app: "shop/api" }, { internet: true }], ports: [{ port: 8080 }], disabled: false, phase: "ready", policies: [], generation: 1, created: "" },
      { project: "shop", name: "off", from: [{ app: "web" }], to: [{ app: "db" }], ports: [], disabled: true, phase: "disabled", policies: [], generation: 1, created: "" },
    ],
    drops: [{ from: { kind: "pod", namespace: "shop", app: "worker" }, to: { kind: "pod", namespace: "shop", app: "api" }, port: 8080, protocol: "TCP", egress: false, count: 312, first: "", last: "" }],
    hubble: { state: "ok", window: "1h" },
    nodes: [
      { name: "n1", roles: ["control-plane"], ready: true, status: "Ready", unschedulable: false },
      { name: "n2", roles: ["worker"], ready: true, status: "Ready", unschedulable: false },
    ],
    nodesPartial: false,
    firewall: [
      { name: "ssh", port: 22, protocol: "TCP", sources: ["198.51.100.0/24"], nodes: "all", description: "SSH", disabled: false, required: true, editable: "sources", ready: true },
      { name: "https", port: 443, protocol: "TCP", sources: [], nodes: "all", description: "HTTPS", disabled: false, required: true, editable: "none", ready: true },
    ],
    metrics: true,
  };
}

describe("buildModel", () => {
  const m = buildModel(shop());

  it("links rules, domains, mounts, drops and jobs, skipping disabled rules", () => {
    const kinds = (k: string) => m.links.filter((l) => l.kind === k).map((l) => `${l.from}>${l.to}`);
    expect(kinds("rule")).toEqual(["app:shop/web>app:shop/api", "app:shop/api>app:shop/db", "app:tools/n8n>app:shop/api", "app:tools/n8n>out"]);
    expect(kinds("route")).toEqual(["gateway>dom:shop/web", "dom:shop/web>app:shop/web"]);
    expect(kinds("mount")).toEqual(["app:shop/db>vol:shop/db/data-0"]);
    expect(kinds("drop")).toEqual(["app:shop/worker>app:shop/api"]);
    expect(kinds("runs")).toEqual(["job:shop/nightly>app:shop/api"]);
  });

  it("puts callers left, called-only apps right, the rest in the middle", () => {
    const col = (id: string) => m.byId.get(id)!.col;
    expect(col("app:shop/web")).toBe(0);
    expect(col("app:shop/worker")).toBe(0);
    expect(col("job:shop/nightly")).toBe(0);
    expect(col("app:shop/api")).toBe(1);
    expect(col("app:shop/db")).toBe(2);
  });

  it("reads status from pods, volumes and certificates", () => {
    expect(m.byId.get("app:shop/worker")!.status).toBe("bad");
    expect(m.byId.get("app:shop/worker")!.sub).toBe("CrashLoopBackOff · 0/1");
    expect(m.byId.get("vol:shop/db/data-0")!.status).toBe("bad"); // 90% full
    expect(m.byId.get("vol:shop/db/data-0")!.name).toBe("db disk");
    expect(m.byId.get("dom:shop/web")!.status).toBe("ok");
  });
});

describe("peers and sides", () => {
  it("resolves rule peers relative to the rule's project", () => {
    expect(peerId({ app: "api" }, "shop", true)).toBe("app:shop/api");
    expect(peerId({ app: "tools/n8n" }, "shop", true)).toBe("app:tools/n8n");
    expect(peerId({ project: "tools" }, "shop", false)).toBe("prj:tools");
    expect(peerId({ internet: true }, "shop", true)).toBe("gateway");
    expect(peerId({ internet: true }, "shop", false)).toBe("out");
    expect(peerId({ cidr: "10.0.0.0/16" }, "shop", false)).toBe("out");
  });

  it("draws drops only between things on the map", () => {
    const apps = new Set(["app:shop/api"]);
    expect(sideId({ kind: "pod", namespace: "shop", app: "api" }, true, apps)).toBe("app:shop/api");
    expect(sideId({ kind: "pod", namespace: "other", app: "x" }, true, apps)).toBeUndefined();
    expect(sideId({ kind: "world" }, false, apps)).toBe("out");
    expect(sideId({ kind: "kube-apiserver" }, false, apps)).toBeUndefined();
  });

  it("names own disks after their app", () => {
    expect(volumeName({ name: "db/data-1", own: true, app: "db" } as never)).toBe("db disk 1");
    expect(volumeName({ name: "uploads", own: false } as never)).toBe("uploads");
  });
});

describe("layout", () => {
  const m = buildModel(shop());

  it("traffic: chips in their project's columns, firewall web ports to the gateway", () => {
    const l = layout(m, traffic());
    expect(l.items["app:shop/web"].x).toBeLessThan(l.items["app:shop/api"].x);
    expect(l.items["app:shop/api"].x).toBeLessThan(l.items["app:shop/db"].x);
    expect(l.items["vol:shop/db/data-0"].y).toBe(l.items["app:shop/db"].y);
    expect(l.items["dom:shop/web"].y).toBe(l.items["app:shop/web"].y);
    expect(l.items["fw:https"]).toBeDefined();
    expect(l.edges.some((e) => e.a === "fw:https" && e.b === "gateway")).toBe(true);
    expect(l.edges.some((e) => e.a === "internet" && e.b === "fw:ssh")).toBe(false); // SSH only in placement
    expect(l.items.out).toBeDefined();
    expect(Object.values(l.items).some((i) => i.kind === "server")).toBe(false);
  });

  it("traffic: a collapsed project draws its links to one chip", () => {
    const l = layout(m, traffic({ collapsed: new Set(["shop"]) }));
    expect(l.items["app:shop/api"]).toBeUndefined();
    expect(l.items["prj:shop"].kind).toBe("pchip");
    expect(l.edges.some((e) => e.a === "app:tools/n8n" && e.b === "prj:shop")).toBe(true);
    expect(l.edges.some((e) => e.a === "dom:shop/web" && e.b === "prj:shop")).toBe(true);
    expect(l.edges.some((e) => e.a === e.b)).toBe(false);
    expect(l.items["vol:shop/db/data-0"]).toBeUndefined();
  });

  it("traffic: layers and the project filter leave things out", () => {
    const l = layout(m, traffic({ layers: { ...allLayers, jobs: false, domains: false, rules: false, firewall: false }, project: "shop" }));
    expect(l.items["job:shop/nightly"]).toBeUndefined();
    expect(l.items["dom:shop/web"]).toBeUndefined();
    expect(l.items["app:tools/n8n"]).toBeUndefined();
    expect(l.items.fwband).toBeUndefined();
    expect(l.edges.filter((e) => e.kind === "rule" || e.kind === "drop")).toEqual([]);
    // Without domains the gateway routes straight to the app.
    expect(l.edges.some((e) => e.a === "gateway" && e.b === "app:shop/web")).toBe(true);
  });

  it("placement: one chip per pod on its server, mounts from the pod on the volume's server", () => {
    const l = layout(m, traffic({ lens: "placement" }));
    expect(l.items["app:shop/web"].node).toBe("n1");
    expect(l.items["app:shop/web~1"].node).toBe("n2");
    expect(l.items["app:shop/web~1"].pod).toMatchObject({ i: 1, n: 2 });
    expect(l.items["job:shop/nightly"]).toMatchObject({ node: "n2", dashed: true });
    expect(l.items["vol:shop/db/data-0"].node).toBe("n1");
    expect(l.items["srv:n1"].kind).toBe("server");
    expect(l.edges.some((e) => e.kind === "ssh" && e.b === "srv:n2")).toBe(true);
    expect(l.edges.find((e) => e.kind === "mount")).toMatchObject({ a: "app:shop/db", b: "vol:shop/db/data-0" });
    expect(l.edges.some((e) => e.kind === "rule")).toBe(false);
  });

  it("placement: apps without placed pods sit apart", () => {
    const t = shop();
    t.apps[1] = app("shop", "api", { pods: [] });
    const l = layout(buildModel(t), traffic({ lens: "placement" }));
    expect(l.items["srv:"]).toMatchObject({ kind: "server", sel: null });
    expect(l.items["app:shop/api"].node).toBeUndefined();
  });

  it("without the firewall (not an owner or admin) the internet goes straight to the gateway", () => {
    const t = shop();
    t.firewall = null;
    const l = layout(buildModel(t), traffic());
    expect(l.items.fwband).toBeUndefined();
    expect(l.edges.some((e) => e.id === "pub:direct")).toBe(true);
  });
});

describe("focus, tween and fit", () => {
  const m = buildModel(shop());
  const l = layout(m, traffic());

  it("lights what a selection touches", () => {
    const f = focusOf(l, "app:shop/api");
    expect([...f.keys].sort()).toEqual(expect.arrayContaining(["app:shop/api", "app:shop/web", "app:shop/db", "app:tools/n8n", "app:shop/worker", "job:shop/nightly"]));
    expect(f.keys.has("dom:shop/web")).toBe(false);
    const r = focusOf(l, "rule:shop/web-to-api");
    expect([...r.keys].sort()).toEqual(["app:shop/api", "app:shop/web"]);
  });

  it("problems are failing things and dropped connections", () => {
    const f = problemsOf(l);
    expect(f.keys.has("app:shop/worker")).toBe(true);
    expect(f.keys.has("vol:shop/db/data-0")).toBe(true);
    expect(f.keys.has("app:shop/web")).toBe(false);
  });

  it("between moves shared items and fades the rest", () => {
    const p = layout(m, traffic({ lens: "placement" }));
    const mid = between(l, p, 0.5);
    expect(mid.items["app:shop/api"].x).toBeCloseTo((l.items["app:shop/api"].x + p.items["app:shop/api"].x) / 2);
    expect(mid.items["app:shop/web~1"].op).toBeCloseTo(0.5); // a pod splits out of its app
    expect(mid.items["srv:n1"].op).toBeCloseTo(0.5);
    expect(between(l, p, 1).items["dom:shop/web"].op).toBe(0);
  });

  it("fits everything, at least 1000 wide", () => {
    const vb = fit(l);
    expect(vb.w).toBeGreaterThanOrEqual(1000);
    for (const i of Object.values(l.items)) expect(i.x - i.w / 2).toBeGreaterThanOrEqual(vb.x);
  });
});

describe("attentionTarget", () => {
  it("maps Needs-attention items to map objects", () => {
    expect(attentionTarget("app:shop/worker")).toBe("app:shop/worker");
    expect(attentionTarget("build:shop/web")).toBe("app:shop/web");
    expect(attentionTarget("schedule:shop/nightly")).toBe("job:shop/nightly");
    expect(attentionTarget("domain:shop/web")).toBe("dom:shop/web");
    expect(attentionTarget("alert:abc", "shop", "api")).toBe("app:shop/api");
    expect(attentionTarget("alert:abc")).toBeUndefined();
    expect(attentionTarget("update:kwerft")).toBeUndefined();
  });
});
