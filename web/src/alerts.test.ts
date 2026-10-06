import { describe, expect, it } from "vitest";
import {
  alertLogsLink, alertNodesHref, alertingUnavailable, attentionHeadline, attentionItems, channelInput, channelForm, channelProblem, channelTarget,
  describeChannels, describeCondition, describeScope, durationProblem, goDuration, humanDuration, parseDuration, shortDuration,
  sortAlerts, thresholdProblem, type Alert, type Channel,
} from "./alerts";
import { ApiError } from "./api";
import type { Build } from "./builds";
import type { Domain, ScheduleSummary } from "./jobs";
import type { AppSummary } from "./workloads";

describe("durations", () => {
  it.each<[string, number | undefined]>([
    ["15m0s", 900], ["1h0m0s", 3600], ["168h0m0s", 604800], ["7d", 604800], ["90s", 90], ["1h30m", 5400], ["2 h", 7200], ["1w", 604800],
    ["", undefined], ["15", undefined], ["abc", undefined], ["-5m", undefined],
  ])("parseDuration(%j) = %j", (s, want) => {
    expect(parseDuration(s)).toBe(want);
  });

  it.each<[number, string]>([[900, "15m"], [604800, "168h"], [5400, "1h30m"], [90, "1m30s"], [45, "45s"], [0, "0s"]])("goDuration(%j) = %j", (s, want) => {
    expect(goDuration(s)).toBe(want);
  });

  it.each<[number, string]>([
    [60, "1 minute"], [900, "15 minutes"], [3600, "1 hour"], [7200, "2 hours"], [86400, "1 day"], [1209600, "14 days"], [30, "30 seconds"], [5400, "90 minutes"], [3690, "1 h 1 min"],
  ])("humanDuration(%j) = %j", (s, want) => {
    expect(humanDuration(s)).toBe(want);
  });

  it.each<[string | undefined, string]>([["15m0s", "15m"], ["168h0m0s", "7d"], ["1h0m0s", "1h"], ["1h30m0s", "90m"], ["90s", "1m30s"], [undefined, ""]])("shortDuration(%j) = %j", (s, want) => {
    expect(shortDuration(s)).toBe(want);
  });

  it("explains a bad duration", () => {
    expect(durationProblem("")).toBe("");
    expect(durationProblem("10m")).toBe("");
    expect(durationProblem("ten minutes")).toMatch(/30s, 15m/);
  });
});

describe("describeCondition", () => {
  it.each<[Parameters<typeof describeCondition>[0], string]>([
    [{ condition: "CrashLooping" }, "Crash looping"],
    [{ condition: "Restarts" }, "More than 5 restarts in 15 minutes"],
    [{ condition: "Restarts", threshold: 3, window: "30m0s" }, "More than 3 restarts in 30 minutes"],
    [{ condition: "MemoryHigh" }, "Memory above 90 % of the limit for 10 minutes"],
    [{ condition: "CPUHigh", threshold: 80, for: "5m0s" }, "CPU above 80 % of the limit for 5 minutes"],
    [{ condition: "VolumeFillingUp" }, "Volume more than 85 % full, or full within 7 days"],
    [{ condition: "NodeMemoryPressure" }, "Less than 10 % of a node's memory available for 10 minutes"],
    [{ condition: "CertificateExpiring" }, "Certificate expires within 14 days"],
    [{ condition: "ScheduleFailing" }, "A schedule's last run failed"],
    [{ condition: "ScheduleFailing", window: "24h0m0s" }, "A schedule's last run failed, or no success within 1 day"],
    [{ condition: "BuildFailing" }, "An app's latest build failed"],
    [{ condition: "HTTPErrorRate" }, "More than 5 % of requests fail (5xx) over 5 minutes"],
    [{ condition: "HTTPLatency", threshold: 800 }, "95th percentile response time above 800 ms over 5 minutes"],
    [{ condition: "Custom" }, "Custom expression"],
  ])("%j", (r, want) => {
    expect(describeCondition(r)).toBe(want);
  });

  it("names the scope", () => {
    expect(describeScope({ condition: "Restarts", scope: { projects: [], apps: [] } })).toBe("All apps");
    expect(describeScope({ condition: "Restarts", scope: { projects: ["shop"], apps: ["web/api"] } })).toBe("shop, web/api");
    expect(describeScope({ condition: "NodeDiskPressure", scope: { projects: ["shop"], apps: [] } })).toBe("All nodes");
  });

  it("checks thresholds", () => {
    expect(thresholdProblem("MemoryHigh", "")).toBe("");
    expect(thresholdProblem("MemoryHigh", "95")).toBe("");
    expect(thresholdProblem("MemoryHigh", "120")).toMatch(/between 1 and 100/);
    expect(thresholdProblem("Restarts", "2.5")).toMatch(/whole number/);
    expect(thresholdProblem("HTTPLatency", "0")).toMatch(/above 0/);
  });
});

const alert = (a: Partial<Alert>): Alert => ({
  fingerprint: "f", rule: "crash-looping", severity: "critical", state: "firing", summary: "", description: "", labels: {}, startsAt: "2026-10-04T10:00:00Z", ...a,
});

describe("sortAlerts", () => {
  it("puts critical first, newest first within a severity", () => {
    const got = sortAlerts([
      alert({ fingerprint: "a", severity: "info" }),
      alert({ fingerprint: "b", severity: "warning", startsAt: "2026-10-04T09:00:00Z" }),
      alert({ fingerprint: "c", severity: "critical" }),
      alert({ fingerprint: "d", severity: "warning", startsAt: "2026-10-04T11:00:00Z" }),
    ]);
    expect(got.map((a) => a.fingerprint)).toEqual(["c", "d", "b", "a"]);
  });
});

describe("alertLogsLink", () => {
  const origin = "https://ops.example.com";
  it("keeps links into this console in the router", () => {
    expect(alertLogsLink({ consoleURL: "https://ops.example.com/apps/shop/api?tab=logs" }, origin)).toEqual({ href: "/apps/shop/api?tab=logs", external: false });
    expect(alertLogsLink({ consoleURL: "https://old.example.com/apps/shop/api?tab=logs" }, origin)).toEqual({ href: "/apps/shop/api?tab=logs", external: false });
  });
  it("falls back to the app's Logs tab", () => {
    expect(alertLogsLink({ project: "shop", app: "api" }, origin)).toEqual({ href: "/apps/shop/api?tab=logs", external: false });
    expect(alertLogsLink({ consoleURL: "javascript:alert(1)", project: "shop", app: "api" }, origin)).toEqual({ href: "/apps/shop/api?tab=logs", external: false });
    expect(alertLogsLink({}, origin)).toBeUndefined();
  });
  it("leaves other sites external", () => {
    expect(alertLogsLink({ consoleURL: "https://grafana.example.com/d/x" }, origin)).toEqual({ href: "https://grafana.example.com/d/x", external: true });
  });
});

describe("alertingUnavailable", () => {
  it("is true for missing endpoints and an unreachable stack only", () => {
    expect(alertingUnavailable(new ApiError(404, "not found"))).toBe(true);
    expect(alertingUnavailable(new ApiError(503, "down"))).toBe(true);
    expect(alertingUnavailable(new ApiError(403, "forbidden"))).toBe(false);
    expect(alertingUnavailable(new Error("network"))).toBe(false);
  });
});

describe("channels", () => {
  const ch = (c: Partial<Channel>): Channel => ({ name: "ops", type: "slack", sendResolved: true, secretSet: true, ready: true, ...c });

  it("summarises the target without secrets", () => {
    expect(channelTarget(ch({ slack: { channel: "#ops" } }))).toBe("#ops");
    expect(channelTarget(ch({ slack: {} }))).toBe("the webhook's default channel");
    expect(channelTarget(ch({ type: "email", email: { to: ["a@x.dev", "b@x.dev"], from: "k@x.dev", smtpHost: "smtp:587" } }))).toBe("a@x.dev, b@x.dev");
    expect(channelTarget(ch({ type: "ntfy", ntfy: { server: "https://ntfy.sh/", topic: "acme" } }))).toBe("ntfy.sh/acme");
    expect(channelTarget(ch({ type: "webhook" }))).toBe("URL stored");
  });

  it("names the channels of a rule", () => {
    const list = [ch({ name: "ops-slack", slack: { channel: "#ops-alerts" } }), ch({ name: "phone", type: "ntfy", ntfy: { server: "https://ntfy.sh", topic: "t" } })];
    expect(describeChannels([], list)).toBe("Console only");
    expect(describeChannels(["ops-slack", "phone", "gone"], list)).toBe("Slack #ops-alerts · ntfy phone · gone");
  });

  it("sends only the type's settings and typed secrets", () => {
    const f = { ...channelForm(), name: "ops", type: "email" as const, to: "a@x.dev, b@x.dev", from: "k@x.dev", smtpHost: "smtp.x.dev:587", url: "https://ignored" };
    expect(channelInput(f)).toEqual({
      name: "ops", type: "email", sendResolved: true,
      email: { to: ["a@x.dev", "b@x.dev"], from: "k@x.dev", smtpHost: "smtp.x.dev:587", username: undefined }, password: undefined,
    });
    expect(channelInput({ ...channelForm(), name: "hook", type: "webhook", url: " https://h.example.com/x " })).toEqual({
      name: "hook", type: "webhook", sendResolved: true, webhook: {}, url: "https://h.example.com/x",
    });
    expect(channelInput({ ...channelForm(), name: "n", type: "ntfy", server: "", topic: "alerts" }).ntfy).toEqual({ server: "https://ntfy.sh", topic: "alerts" });
  });

  it("finds problems before sending", () => {
    const slack = (url: string, stored = false) => channelProblem(channelInput({ ...channelForm(), name: "ops", url }), stored);
    expect(slack("")).toEqual({ field: "url", message: "Enter the incoming webhook URL." });
    expect(slack("", true)).toBeUndefined();
    expect(slack("http://hooks.slack.com/x")?.message).toMatch(/https/);
    expect(slack("https://hooks.slack.com/services/x")).toBeUndefined();
    expect(channelProblem(channelInput({ ...channelForm(), name: "Ops" }), true)?.field).toBe("name");
    const email = channelInput({ ...channelForm(), name: "mail", type: "email", to: "ops@x.dev", from: "k@x.dev", smtpHost: "smtp.x.dev" });
    expect(channelProblem(email, false)?.field).toBe("email.smtpHost");
  });
});

describe("attentionItems", () => {
  const now = Date.parse("2026-10-04T12:00:00Z");
  const app = (a: Partial<AppSummary>): AppSummary => ({
    name: "api", project: "shop", source: { type: "image" }, image: "x", readyReplicas: 1, replicas: 1, stateful: false, phase: "running",
    urls: [], revision: 1, updated: "2026-10-04T08:00:00Z", ...a,
  });

  it("combines alerts and what the console knows, worst first", () => {
    const items = attentionItems({
      now, origin: "https://ops.example.com",
      alerts: [alert({ fingerprint: "a1", project: "shop", app: "worker", rule: "crash-looping", summary: "worker restarts", consoleURL: "https://ops.example.com/apps/shop/worker?tab=logs" })],
      apps: [
        app({ name: "worker", phase: "deploying", readyReplicas: 0 }), // covered by the alert
        app({ name: "web", phase: "deploying", readyReplicas: 0, replicas: 2 }),
        app({ name: "fresh", phase: "deploying", readyReplicas: 0, updated: "2026-10-04T11:58:00Z" }), // just deploying
        app({ name: "ok" }),
        app({ name: "site", source: { type: "git" } }),
      ],
      builds: { "shop/site": { name: "site-7", number: 7, phase: "failed", created: "2026-10-04T11:00:00Z", statusMessage: "exit 1" } as Build },
      schedules: [
        { name: "backup", project: "shop", suspend: false, phase: "scheduled", lastRun: { name: "backup-1", project: "shop", phase: "failed", exitCode: 2, created: "2026-10-04T03:00:00Z", finished: "2026-10-04T03:01:00Z" } } as ScheduleSummary,
        { name: "old", project: "shop", suspend: true, phase: "suspended", lastRun: { name: "old-1", project: "shop", phase: "failed", created: "2026-10-01T00:00:00Z" } } as ScheduleSummary,
      ],
      domains: [
        { name: "d1", project: "shop", hostname: "shop.example.com", certificate: "failed", created: "2026-10-04T11:50:00Z", message: "DNS points elsewhere" } as Domain,
        { name: "d2", project: "shop", hostname: "new.example.com", certificate: "issuing", created: "2026-10-04T11:50:00Z" } as Domain,
        { name: "d3", project: "shop", hostname: "soon.example.com", certificate: "valid", created: "2026-07-01T00:00:00Z", notAfter: "2026-10-10T12:00:00Z" } as Domain,
      ],
    });
    expect(items.map((i) => i.key)).toEqual([
      "domain:shop/d1", "build:shop/site", "alert:a1", "app:shop/web", "schedule:shop/backup", "domain:shop/d3",
    ]);
    const byKey = Object.fromEntries(items.map((i) => [i.key, i]));
    expect(byKey["alert:a1"]!.action).toEqual({ label: "Open logs", href: "/apps/shop/worker?tab=logs" });
    expect(byKey["build:shop/site"]!.action.href).toBe("/apps/shop/site?build=site-7");
    expect(byKey["schedule:shop/backup"]!.action.href).toBe("/jobs/shop/tasks/backup-1");
    expect(byKey["schedule:shop/backup"]!.detail).toBe("Exit code 2.");
    expect(byKey["domain:shop/d3"]!.detail).toMatch(/in 6 days/);
  });

  it("works without alerts", () => {
    expect(attentionItems({ now, origin: "https://x", apps: [app({ phase: "failed", reason: "HostnameInUse", message: "taken" })] })).toEqual([
      expect.objectContaining({ key: "app:shop/api", what: "Hostname in use", detail: "taken" }),
    ]);
    expect(attentionItems({ now, origin: "https://x" })).toEqual([]);
  });

  it("sends alerts without an app to Monitoring", () => {
    const [item] = attentionItems({ now, origin: "https://x", alerts: [alert({ rule: "node-memory", labels: { node: "fsn1-1" } })] });
    expect(item!.title).toBe("fsn1-1");
    expect(item!.action).toEqual({ label: "Review", href: "/monitoring" });
  });

  it("sends disk alerts to their cluster's nodes", () => {
    const disk = alert({ rule: "disk-failing", cluster: "kwerft-dedi-1", consoleURL: "https://x/clusters/local/nodes",
      labels: { node: "kwerft-dedi-1", device: "nvme0", kwerft_kind: "disk" } });
    const [item] = attentionItems({ now, origin: "https://x", alerts: [disk] });
    expect(item!.title).toBe("kwerft-dedi-1");
    expect(item!.action).toEqual({ label: "Open nodes", href: "/clusters/kwerft-dedi-1/nodes" });
    expect(alertNodesHref({ labels: { kwerft_kind: "node", node: "n" } })).toBeUndefined();
  });

  it("writes a headline", () => {
    const items = attentionItems({
      now, origin: "https://x",
      apps: [app({ name: "a", phase: "failed", reason: "CrashLooping" }), app({ name: "b", phase: "failed", reason: "VolumeNotFound" }), app({ name: "c", phase: "failed" })],
    });
    expect(attentionHeadline(items).title).toBe("3 issues need attention.");
    expect(attentionHeadline(items.slice(0, 1)).title).toBe("1 issue needs attention.");
    expect(attentionHeadline(items).detail).toMatch(/and 1 more\.$/);
    expect(attentionHeadline([])).toEqual({ title: "", detail: "" });
  });
});
