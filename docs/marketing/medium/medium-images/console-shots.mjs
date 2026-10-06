// Console screenshots for the Medium posts: crops of the cards a post talks
// about, from the real console with the homepage's demo data (a fictional
// web shop, nothing from a real server).
//
//   node console-shots.mjs --dist <built console> [--homepage <kwerft-homepage>] [--only a,b]
//
// Build the console from a release tag, never from the working tree:
//   git archive v0.6.0-rc.3 web | tar -x -C <tmp> && (cd <tmp>/web && npm ci)
//   <tmp>/web/node_modules/.bin/vite build --outDir <tmp>/dist --emptyOutDir
// The mock API and Playwright come from ../kwerft-homepage/tools/screenshots
// (npm install there once). Writes <name>.png next to this file.
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const here = path.dirname(fileURLToPath(import.meta.url));
const args = process.argv.slice(2);
const arg = (k, d) => { const i = args.indexOf(k); return i >= 0 ? args[i + 1] : d; };
const homepage = path.resolve(arg("--homepage", path.join(here, "../../../../../kwerft-homepage")));
const tools = path.join(homepage, "tools/screenshots");
const dist = path.resolve(arg("--dist", path.join(tools, ".cache/console")));
const only = arg("--only")?.split(",");
const port = 8098;
const base = `http://127.0.0.1:${port}`;

const { chromium } = await import(pathToFileURL(path.join(tools, "node_modules/playwright/index.mjs")).href);
const { createServer } = await import(pathToFileURL(path.join(tools, "mock/server.mjs")).href);

async function settle(page, extra = 400) {
  await page.waitForLoadState("networkidle").catch(() => {});
  await page.waitForFunction(() => {
    const busy = [...document.querySelectorAll(".loading")].some((e) => e.offsetParent !== null);
    return !busy && !/Loading |Checking…|Updating…/.test(document.body.innerText);
  }, null, { timeout: 30000 });
  await page.evaluate(() => document.fonts.ready);
  await page.waitForTimeout(extra);
}
const card = (page, heading) => page.locator(".card", { has: page.locator("h3", { hasText: heading }) }).first();

// A Kwerft upgrade that failed its verification and rolled back, for the
// upgrades post. The wording is the runner's (internal/upgrades/runner.go,
// kube.go AppsBehind); the times are relative to now.
function rolledBack(now) {
  const t = (min) => new Date(now - min * 60000).toISOString();
  const step = (id, label, state, at, detail) => ({ id, label, state, at: t(at), ...(detail ? { detail } : {}) });
  return {
    name: "kwerft-0-6-1-r8t2v", cluster: "local", component: "Kwerft", version: "0.6.1", from: { kwerft: "0.6.0", kubernetes: "v1.37.1+k3s1" },
    requestedBy: "mara@example.com", auto: false, phase: "RolledBack", preflight: [], nodes: [], cancellable: false, finished: true,
    reason: "Verify",
    message: "Verification failed: Apps with fewer ready replicas than before: shop/storefront (1 of 2 ready). Rolled back to 0.6.0.",
    backup: { etcdSnapshot: "pre-kwerft-0-6-1-r8t2v", database: "backups/pre-kwerft-0-6-1-r8t2v.db" },
    steps: [
      step("preflight", "Preflight", "Done", 47), step("system", "System", "Done", 46), step("firewall", "Firewall", "Done", 46),
      step("kubernetes", "Kubernetes", "Skipped", 46, "k3s v1.37.1+k3s1 already installed"), step("registry", "Registry mirror", "Done", 46),
      step("helm", "Helm", "Done", 45), step("upgrades", "Upgrades", "Done", 45), step("network", "Network", "Done", 43, "Cilium 1.20.2"),
      step("ingress", "Ingress & TLS", "Done", 41), step("observability", "Observability", "Done", 39), step("backups", "Backups", "Done", 38),
      step("kwerft", "Kwerft", "Done", 36), step("handoff", "Handoff", "Done", 36),
    ],
    createdAt: t(48), startedAt: t(48), finishedAt: t(22),
  };
}

const shots = [
  { name: "drain-shot-health", path: "/apps/shop/storefront", act: async (p) => { await p.getByRole("tab", { name: "Settings" }).click(); await settle(p); }, crop: (p) => card(p, "Health & draining") },
  { name: "migration-shot-schedules", path: "/jobs", width: 1560, crop: (p) => p.locator(".card", { hasText: "Schedules" }).first() },
  { name: "rbac-shot-roles", path: "/access/roles", crop: (p) => card(p, "How roles reach Kubernetes") },
  { name: "rbac-shot-secrets", path: "/secrets?project=shop", width: 1440, crop: (p) => p.locator(".main > :not(.topbar)").first() },
  {
    name: "upgrade-shot-rolledback", path: "/settings/updates", upgrade: true,
    act: async (p) => { await p.getByRole("button", { name: "Kwerft 0.6.1" }).click(); await settle(p, 800); },
    crop: (p) => p.locator(".card.upgrade-progress"),
  },
  { name: "upgrade-shot-versions", path: "/settings/updates", crop: (p) => card(p, "Versions") },
  { name: "backup-shot-target", path: "/settings", crop: (p) => p.locator("#backups") },
  { name: "backup-shot-plans", path: "/backups", crop: (p) => card(p, "Plans") },
];

const { server, misses } = createServer({ dist });
await new Promise((ok, fail) => server.once("error", fail).listen(port, "127.0.0.1", ok));
const browser = await chromium.launch({ channel: "chrome", headless: true });
try {
  for (const s of shots) {
    if (only && !only.includes(s.name)) continue;
    const context = await browser.newContext({ viewport: { width: s.width ?? 1280, height: 1000 }, deviceScaleFactor: 2, colorScheme: "light", locale: "en-GB", timezoneId: "Europe/Berlin" });
    const page = await context.newPage();
    if (s.upgrade) {
      const u = rolledBack(Date.now());
      await page.route(/\/api\/v1\/upgrades(\?.*)?$/, async (route) => {
        const r = await route.fetch();
        const list = await r.json();
        await route.fulfill({ response: r, json: new URL(route.request().url()).searchParams.get("cluster") === "hel1-staging" ? list : [u, ...list] });
      });
      await page.route(/\/api\/v1\/upgrades\/kwerft-0-6-1-r8t2v(\?.*)?$/, (route) => route.fulfill({ json: u }));
      await page.route(/\/api\/v1\/upgrades\/kwerft-0-6-1-r8t2v\/events/, (route) => route.fulfill({
        contentType: "text/event-stream",
        body: `event: upgrade\ndata: ${JSON.stringify(u)}\n\nevent: end\ndata: {"phase":"RolledBack","reason":"Verify"}\n\n`,
      }));
      await page.route(/\/api\/v1\/upgrades\/kwerft-0-6-1-r8t2v\/log/, (route) => route.fulfill({ json: { log: "", truncated: false } }));
    }
    await page.goto(base + s.path);
    await settle(page);
    if (s.act) await s.act(page);
    await page.mouse.move(0, 0);
    const file = path.join(here, `${s.name}.png`);
    await s.crop(page).screenshot({ path: file });
    console.log(path.basename(file));
    await context.close();
  }
} finally {
  await browser.close();
  server.close();
  server.closeAllConnections?.();
}
if (misses.length) console.warn(`The mock had no fixture for:\n  ${[...new Set(misses)].join("\n  ")}`);
