import { describe, expect, it } from "vitest";
import {
  durationText, enablesAutoPatch, fleetLine, fleetSteps, inline, needsTypedConfirmation, noticeText, notesBlocks, parseSSE, phasePill, progressLines,
  summaryLine, updateNotices, windowText,
  type Upgrade,
} from "./updates";
import { conditions, describeCondition } from "./alerts";

const base: Upgrade = {
  name: "kwerft-0.6.0-x7k2p", cluster: "local", component: "Kwerft", version: "0.6.0", requestedBy: "alice@example.com", auto: false,
  phase: "Pending", from: { kwerft: "0.5.0", kubernetes: "v1.37.1+k3s1" }, preflight: [], steps: [], nodes: [],
  cancellable: true, finished: false, createdAt: "2026-10-05T10:00:00Z",
};
const up = (o: Partial<Upgrade>): Upgrade => ({ ...base, ...o });
const marks = (u: Upgrade) => progressLines(u).map((l) => `${l.mark} ${l.label}`);

describe("progress", () => {
  it("waits, checks and backs up before the installer", () => {
    expect(marks(up({ phase: "Queued", message: "Waiting for kubernetes-x to finish." }))).toEqual(["wait Checks", "wait Backup"]);
    expect(progressLines(up({ phase: "Queued", message: "Waiting for kubernetes-x to finish." }))[0]!.detail).toBe("Waiting for kubernetes-x to finish.");
    expect(marks(up({ phase: "Preflight" }))).toEqual(["run Checks", "wait Backup"]);
    const checks = [{ check: "Target", ok: true }, { check: "AgentSkew", ok: false, warning: true, message: "skipped" }, { check: "NodesReady", ok: true }];
    const backup = up({ phase: "Backup", preflight: checks, backup: { database: "/var/lib/kwerft/backups/pre-x.db" } });
    expect(marks(backup)).toEqual(["ok Checks", "run Backup"]);
    expect(progressLines(backup)[0]!.detail).toBe("2 of 3 passed · 1 warning");
  });

  it("shows each installer stage like the installer prints it", () => {
    const u = up({
      phase: "Running", preflight: [{ check: "Target", ok: true }],
      backup: { etcdSnapshot: "pre-kwerft-0.6.0-x7k2p", helmRevisions: { "kube-system/cilium": 3, "kwerft-system/kwerft": 12 } },
      steps: [
        { id: "preflight", label: "Preflight", state: "Done", detail: "Ubuntu 24.04 · x86_64" },
        { id: "kubernetes", label: "Kubernetes", state: "Skipped", detail: "k3s v1.37.1+k3s1 · installed" },
        { id: "network", label: "Network", state: "Running" },
      ],
    });
    expect(marks(u)).toEqual(["ok Checks", "ok Backup", "ok Preflight", "skip Kubernetes", "run Network"]);
    expect(progressLines(u)[1]!.detail).toBe("etcd snapshot pre-kwerft-0.6.0-x7k2p · 2 Helm releases recorded");
  });

  it("ends with verification, or the rollback", () => {
    const steps = [{ id: "kwerft", label: "Kwerft", state: "Done" }];
    expect(marks(up({ phase: "Verifying", steps, message: "Waiting for the console" }))).toEqual(["ok Checks", "ok Backup", "ok Kwerft", "run Verify"]);
    expect(marks(up({ phase: "Succeeded", steps }))).toEqual(["ok Checks", "ok Backup", "ok Kwerft", "ok Verify"]);
    const failedStep = [{ id: "kwerft", label: "Kwerft", state: "Failed", detail: "Kwerft installation failed" }];
    expect(marks(up({ phase: "RollingBack", steps: failedStep }))).toEqual(["ok Checks", "ok Backup", "fail Kwerft", "run Rollback"]);
    const rolled = up({ phase: "RolledBack", steps: failedStep, reason: "Kwerft" });
    expect(marks(rolled)).toEqual(["ok Checks", "ok Backup", "fail Kwerft", "ok Rollback"]);
    expect(progressLines(rolled).at(-1)!.detail).toBe("back on 0.5.0");
    expect(marks(up({ phase: "RolledBack", steps, reason: "Verify", message: "the console answers 0.5.0" })))
      .toEqual(["ok Checks", "ok Backup", "ok Kwerft", "fail Verify", "ok Rollback"]);
  });

  it("names what failed before anything changed", () => {
    const pre = up({ phase: "Failed", reason: "Preflight", preflight: [{ check: "NodesReady", ok: false, message: "Not ready: kwerft-2." }] });
    expect(marks(pre)).toEqual(["fail Checks", "skip Backup"]);
    expect(progressLines(pre)[0]!.detail).toBe("Not ready: kwerft-2.");
    expect(marks(up({ phase: "Failed", reason: "Backup", preflight: [{ check: "Target", ok: true }], message: "etcd snapshot failed" })))
      .toEqual(["ok Checks", "fail Backup"]);
    expect(marks(up({ phase: "Cancelled", message: "Cancelled by alice before anything changed." }))).toEqual(["skip Checks", "skip Backup"]);
  });

  it("goes node by node for Kubernetes", () => {
    const u = up({
      component: "Kubernetes", version: "v1.38.1+k3s1", phase: "Running", preflight: [{ check: "Target", ok: true }], backup: { etcdSnapshot: "pre-k" },
      nodes: [
        { name: "kwerft-1", version: "v1.38.1+k3s1", state: "Done" },
        { name: "worker-1", version: "v1.37.1+k3s1", state: "Draining" },
        { name: "worker-2", version: "v1.37.1+k3s1", state: "Waiting" },
      ],
    });
    expect(marks(u)).toEqual(["ok Checks", "ok Backup", "ok kwerft-1", "run worker-1", "wait worker-2"]);
    expect(progressLines(u)[3]!.detail).toBe("v1.37.1+k3s1 · Draining");
  });

  it("closes with one line", () => {
    expect(summaryLine(up({ phase: "Succeeded", startedAt: "2026-10-05T10:00:00Z", finishedAt: "2026-10-05T10:04:12Z" })))
      .toEqual({ tone: "ok", text: "Done in 4m 12s. Kwerft runs 0.6.0." });
    expect(summaryLine(up({ phase: "Failed", reason: "Timeout", message: "The installer ran for 90 minutes." }))!.text).toBe("Failed (Timeout). The installer ran for 90 minutes.");
    expect(summaryLine(up({ phase: "RolledBack", message: "Back on 0.5.0." }))!.tone).toBe("warn");
    expect(summaryLine(up({ phase: "Running" }))).toBeUndefined();
  });

  it.each<[number, string]>([[0, "0s"], [52_000, "52s"], [221_000, "3m 41s"], [3_840_000, "1h 4m"]])("durationText(%j) = %j", (ms, want) => {
    expect(durationText(ms)).toBe(want);
  });

  it("names phases", () => {
    expect(phasePill("Pending").label).toBe("Queued");
    expect(phasePill("RollingBack")).toEqual({ pill: "warn", label: "Rolling back" });
    expect(phasePill("Failed").pill).toBe("bad");
    expect(phasePill("Succeeded").pill).toBe("ok");
  });
});

describe("confirmation and notices", () => {
  it("asks to type the version for a Kubernetes minor only", () => {
    expect(needsTypedConfirmation("Kubernetes", "Minor")).toBe(true);
    expect(needsTypedConfirmation("Kubernetes", undefined)).toBe(true);
    expect(needsTypedConfirmation("Kubernetes", "Patch")).toBe(false);
    expect(needsTypedConfirmation("Kwerft", "Minor")).toBe(false);
  });

  it("announces the newest allowed release per component, unless updates are off", () => {
    const available = [
      { component: "Kwerft" as const, version: "0.7.0", kind: "Minor" as const, allowed: false, reason: "upgrade Kubernetes first" },
      { component: "Kwerft" as const, version: "0.6.1", kind: "Minor" as const, allowed: true },
      { component: "Kubernetes" as const, version: "v1.37.2+k3s1", kind: "Patch" as const, allowed: true },
    ];
    const policy = { policy: "Notify" as const, channel: "stable" as const, kubernetesPatches: false };
    expect(updateNotices({ policy, available }).map((a) => a.version)).toEqual(["0.6.1", "v1.37.2+k3s1"]);
    expect(updateNotices({ policy: { ...policy, policy: "Off" }, available })).toEqual([]);
    expect(updateNotices(undefined)).toEqual([]);
    expect(noticeText(available[1]!)).toEqual({ title: "Kwerft 0.6.1", detail: "Minor release" });
  });

  it("describes the window", () => {
    expect(windowText({ days: ["Sun"], start: "03:00", duration: "2h", timeZone: "Europe/Berlin" })).toBe("Sun 03:00 for 2h (Europe/Berlin)");
    expect(windowText({ days: [], start: "22:30", duration: "1h30m" })).toBe("every day 22:30 for 1h30m (UTC)");
    expect(windowText(undefined)).toBe("no window");
  });

  it("knows the UpgradeFailed alert", () => {
    expect(conditions.UpgradeFailed.scope).toBe("none");
    expect(describeCondition({ condition: "UpgradeFailed" })).toBe("The latest upgrade failed or was rolled back (fires for 1 day)");
  });
});

describe("release notes", () => {
  it("reads headings, paragraphs, lists and code as text", () => {
    const md = "Faster **builds** and a [new page](https://example.com).\nSecond line.\n\n## What's Changed\n* Fix `x` by @someone in https://github.com/a/b/pull/1\n* Two\n  continued\n\n```bash\ncurl x | sh\n```\n<img src=x onerror=alert(1)>";
    expect(notesBlocks(md)).toEqual([
      { type: "p", text: "Faster builds and a new page. Second line." },
      { type: "h", level: 2, text: "What's Changed" },
      { type: "ul", items: ["Fix `x` by @someone in https://github.com/a/b/pull/1", "Two continued"] },
      { type: "code", text: "curl x | sh" },
    ]);
  });

  it("drops emphasis, links and HTML inline", () => {
    expect(inline("*one* __two__ ![img](x.png) [three](y) <b>four</b> snake_case_name")).toBe("one two img three four snake_case_name");
  });
});

describe("upgrade all", () => {
  it("lists the console first, then the agent clusters", () => {
    const plan = { version: "0.6.0", console: true, clusters: ["edge-1", "edge-2"], skipped: [{ cluster: "away", reason: "not connected", warning: true }] };
    expect(fleetSteps(plan, "0.5.0")).toEqual(["The console: Kwerft 0.5.0 → 0.6.0", "edge-1: its agent to 0.6.0", "edge-2: its agent to 0.6.0"]);
    expect(fleetSteps({ ...plan, console: false })).toEqual(["edge-1: its agent to 0.6.0", "edge-2: its agent to 0.6.0"]);
  });

  it("follows the agent clusters on the console's upgrade", () => {
    expect(fleetLine(base)).toBeUndefined();
    const running = up({ phase: "Succeeded", agentClusters: { finished: false, stopped: false, message: "Agent clusters (edge-1: Running, edge-2: waiting)." } });
    expect(fleetLine(running)).toEqual({ mark: "run", label: "Agent clusters", detail: "edge-1: Running, edge-2: waiting." });
    expect(marks(running).at(-1)).toBe("run Agent clusters");
    const stopped = up({ phase: "Succeeded", agentClusters: { finished: true, stopped: true,
      message: "Agent clusters (edge-1: RolledBack, edge-2: Cancelled). Stopped: edge-1 ended RolledBack." } });
    expect(fleetLine(stopped)).toEqual({ mark: "fail", label: "Agent clusters", detail: "edge-1: RolledBack, edge-2: Cancelled. Stopped: edge-1 ended RolledBack." });
    expect(fleetLine(up({ agentClusters: { finished: true, stopped: false, message: "Agent clusters (edge-1: Succeeded)." } }))!.mark).toBe("ok");
  });
});

describe("the update policy", () => {
  it("asks for the password when unattended upgrades are turned on, not off", () => {
    const notify = { policy: "Notify" as const, kubernetesPatches: false };
    const auto = { policy: "AutoPatch" as const, kubernetesPatches: false };
    const autoK8s = { policy: "AutoPatch" as const, kubernetesPatches: true };
    expect(enablesAutoPatch(notify, auto)).toBe(true);
    expect(enablesAutoPatch(auto, autoK8s)).toBe(true);
    expect(enablesAutoPatch(notify, autoK8s)).toBe(true);
    expect(enablesAutoPatch(auto, auto)).toBe(false);
    expect(enablesAutoPatch(autoK8s, auto)).toBe(false);
    expect(enablesAutoPatch(autoK8s, notify)).toBe(false);
    expect(enablesAutoPatch({ policy: "Off", kubernetesPatches: false }, notify)).toBe(false);
  });
});

describe("the event stream", () => {
  it("splits events and keeps the rest", () => {
    const { events, rest } = parseSSE('event: upgrade\ndata: {"a":1}\n\n: ping\n\nevent: end\ndata: {"phase":"Succeeded"}\n\nevent: upg');
    expect(events).toEqual([{ name: "upgrade", data: '{"a":1}' }, { name: "end", data: '{"phase":"Succeeded"}' }]);
    expect(rest).toBe("event: upg");
  });
});
