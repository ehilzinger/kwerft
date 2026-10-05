import { describe, expect, it } from "vitest";
import { countWarnings, createdApps, paramProblem, parseDotEnv, planLines, templateValues, warningsByService, type ImportPlan, type Template } from "./imports";

describe(".env values", () => {
  it("reads KEY=value lines with comments, export and quotes", () => {
    expect(parseDotEnv('# db\nDB_PASSWORD=s3cret\nexport TAG=1.2\nGREETING="hello # world"\nNAME=x # note\nEMPTY=\n')).toEqual({
      env: { DB_PASSWORD: "s3cret", TAG: "1.2", GREETING: "hello # world", NAME: "x", EMPTY: "" },
    });
  });
  it("names the first line it cannot read", () => {
    expect(parseDotEnv("A=1\nnot a line\n").bad).toBe(2);
  });
});

const plan: Pick<ImportPlan, "apps" | "volumes" | "secretSets"> = {
  volumes: [{ name: "db-data", spec: { size: "5Gi", class: "local-nvme" } }],
  secretSets: [
    { name: "web-env", app: "web", keys: ["STRIPE_KEY"], missing: ["SESSION_SECRET"] },
    { name: "pg", generate: ["PASSWORD"], derived: [{ key: "DATABASE_URL", template: "postgres://app:${PASSWORD}@pg/app" }] },
  ],
  apps: [
    { name: "web", spec: { source: { image: { ref: "acme/web:1" } }, replicas: 2, size: "medium", ports: [{ container: 3000, public: "web.apps.example.com" }] } },
    { name: "db", spec: { source: { image: { ref: "postgres:17" } }, ports: [{ container: 5432 }], volumes: [{ path: "/var/lib/postgresql/data", volume: "db-data" }] } },
  ],
};

describe("the plan", () => {
  it("lists objects in creation order: volumes, shared sets, apps, the apps' own sets", () => {
    const lines = planLines(plan);
    expect(lines.map((l) => `${l.kind}/${l.name}`)).toEqual(["Volume/db-data", "SecretSet/pg", "App/web", "App/db", "SecretSet/web-env"]);
    expect(lines[1]!.detail).toBe("PASSWORD generated · DATABASE_URL derived");
    expect(lines[2]).toMatchObject({ detail: "acme/web:1 · medium · 2 replicas · port 3000", public: ["web.apps.example.com"] });
    expect(lines[3]!.detail).toBe("postgres:17 · small · port 5432 · db-data at /var/lib/postgresql/data");
    expect(lines[4]!.detail).toBe("STRIPE_KEY from the file · SESSION_SECRET to set; deleted with web");
  });

  it("groups warnings by service, file-wide ones first", () => {
    const groups = warningsByService([
      { level: "warning", service: "web", message: "a" },
      { level: "info", message: "b" },
      { level: "warning", service: "db", message: "c" },
      { level: "info", service: "web", message: "d" },
    ]);
    expect(groups.map(([s, ws]) => [s, ws.map((w) => w.message).join("")])).toEqual([["", "b"], ["web", "ad"], ["db", "c"]]);
    expect(countWarnings(groups.flatMap(([, ws]) => ws))).toBe(2);
  });

  it("links the created apps", () => {
    expect(createdApps(["Volume/x", "App/web", "SecretSet/web-env", "App/db"])).toEqual(["web", "db"]);
  });
});

const n8n: Template = {
  id: "n8n", title: "n8n", category: "Automation", description: "", images: [], pinned: "2026-10-05",
  parameters: [
    { key: "name", label: "App name", type: "name", default: "n8n", maxLength: 50 },
    { key: "hostname", label: "Hostname", type: "hostname", required: true },
    { key: "console", label: "Console", type: "hostname", suffix: "-console" },
    { key: "disk", label: "Disk", type: "size", default: "5Gi" },
  ],
};

describe("template parameters", () => {
  it("suggests hostnames under the apps domain that follow the name until edited", () => {
    expect(templateValues(n8n, "apps.example.com")).toEqual({ name: "n8n", hostname: "n8n.apps.example.com", console: "n8n-console.apps.example.com", disk: "5Gi" });
    const renamed = templateValues(n8n, "apps.example.com", { name: "flows", hostname: "x" }, new Set(["name"]));
    expect(renamed.hostname).toBe("flows.apps.example.com");
    const edited = templateValues(n8n, "apps.example.com", { name: "flows", hostname: "n8n.acme.org" }, new Set(["name", "hostname"]));
    expect(edited.hostname).toBe("n8n.acme.org");
    expect(templateValues(n8n, undefined).hostname).toBe("");
  });

  it("checks values like the server", () => {
    const [name, host, console, disk] = n8n.parameters as [Template["parameters"][number], Template["parameters"][number], Template["parameters"][number], Template["parameters"][number]];
    expect(paramProblem(name, "flows")).toBeUndefined();
    expect(paramProblem(name, "Flows")).toMatch(/lowercase/);
    expect(paramProblem(name, "a".repeat(51))).toMatch(/at most 50/);
    expect(paramProblem(host, "")).toMatch(/hostname/);
    expect(paramProblem(console, "")).toBeUndefined();
    expect(paramProblem(host, "not a host")).toMatch(/hostname/);
    expect(paramProblem(disk, "10Gi")).toBeUndefined();
    expect(paramProblem(disk, "lots")).toMatch(/size/);
    expect(paramProblem({ key: "a", label: "Apps", type: "apps" }, "api, shop/web")).toBeUndefined();
    expect(paramProblem({ key: "a", label: "Apps", type: "apps" }, "a/b/c")).toBeDefined();
  });
});
