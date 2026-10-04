import { describe, expect, it } from "vitest";
import {
  branchProblem, buildLook, buildResult, commitURL, connectionProblem, durationText, normalizeRepository, repoHost, repoLabel,
  repoPathProblem, repositoryProblem, stripAnsi, suggestConnection, type Build, type Connection, type ConnectionInput,
} from "./builds";

describe("normalizeRepository", () => {
  it.each<[string, string]>([
    ["github.com/acme/api", "https://github.com/acme/api"],
    ["  github.com/acme/api.git  ", "https://github.com/acme/api.git"],
    ["https://github.com/acme/api/", "https://github.com/acme/api"],
    ["HTTPS://gitlab.com/acme/group/api", "https://gitlab.com/acme/group/api"],
    ["git@github.com:acme/api.git", "git@github.com:acme/api.git"],
    ["ssh://git@git.example.com:2222/acme/api.git", "ssh://git@git.example.com:2222/acme/api.git"],
    ["git.example.com:8443/acme/api", "https://git.example.com:8443/acme/api"],
    ["api", "api"],
    ["", ""],
  ])("%j → %j", (input, want) => {
    expect(normalizeRepository(input)).toBe(want);
  });
});

describe("repositoryProblem", () => {
  it.each([
    "https://github.com/acme/api.git",
    "https://gitlab.example.com:8443/acme/group/api",
    "git@github.com:acme/api.git",
    "ssh://git@git.example.com:2222/acme/api.git",
  ])("accepts %j", (repo) => {
    expect(repositoryProblem(repo)).toBe("");
  });

  it.each<[string, RegExp]>([
    ["", /Enter the repository/],
    ["http://github.com/acme/api", /plain HTTP/],
    ["https://user:token@github.com/acme/api", /credentials/],
    ["https://github.com/acme/../api", /\.\./],
    ["github.com/acme/api", /https:\/\/host/],
    ["https://github.com", /https:\/\/host/],
  ])("rejects %j", (repo, msg) => {
    expect(repositoryProblem(repo)).toMatch(msg);
  });
});

describe("branchProblem", () => {
  it.each(["", "main", "release/2.x", "feature/ABC-12_fix", "v1.2.3"])("accepts %j", (b) => {
    expect(branchProblem(b)).toBe("");
  });
  it.each(["-main", "/main", "main/", "a..b", "a//b", "has space", "x.lock", "a@{1}", ".hidden"])("rejects %j", (b) => {
    expect(branchProblem(b)).not.toBe("");
  });
});

describe("repoPathProblem", () => {
  it.each(["", "/", "services/api", "/services/api/", "docker/Dockerfile.prod"])("accepts %j", (p) => {
    expect(repoPathProblem(p)).toBe("");
  });
  it.each(["../x", "a/../../b", "..", "a\\b", "a\nb"])("rejects %j", (p) => {
    expect(repoPathProblem(p)).not.toBe("");
  });
});

describe("repository labels", () => {
  it.each<[string, string, string]>([
    ["https://github.com/acme/api.git", "github.com", "github.com/acme/api"],
    ["git@gitlab.com:acme/group/api.git", "gitlab.com", "gitlab.com/acme/group/api"],
    ["ssh://git@Git.Example.com:2222/acme/api", "git.example.com", "git.example.com/acme/api"],
    ["https://git.example.com:8443/acme/api/", "git.example.com", "git.example.com/acme/api"],
  ])("%j", (repo, host, label) => {
    expect(repoHost(repo)).toBe(host);
    expect(repoLabel(repo)).toBe(label);
  });
});

describe("suggestConnection", () => {
  const conn = (name: string, url: string, owner?: string, projects: string[] = []): Connection =>
    ({ name, provider: "github", url, auth: "token", owner, projects, ready: true, webhookAutomatic: true });
  const conns = [
    conn("gitlab", "https://gitlab.com"),
    conn("github-all", "https://github.com"),
    conn("github-acme", "https://github.com", "Acme"),
    conn("github-shop", "https://github.com", "shop", ["storefront"]),
  ];
  it("prefers the connection for the repository's owner", () => {
    expect(suggestConnection("https://github.com/acme/api", conns)).toBe("github-acme");
    expect(suggestConnection("git@github.com:acme/api.git", conns)).toBe("github-acme");
  });
  it("falls back to one without an owner on the same host", () => {
    expect(suggestConnection("https://github.com/other/api", conns)).toBe("github-all");
    expect(suggestConnection("https://gitlab.com/acme/api", conns)).toBe("gitlab");
  });
  it("leaves out connections of other projects and unknown hosts", () => {
    expect(suggestConnection("https://github.com/shop/web", conns, "internal")).toBe("github-all");
    expect(suggestConnection("https://github.com/shop/web", conns, "storefront")).toBe("github-shop");
    expect(suggestConnection("https://codeberg.org/acme/api", conns)).toBe("");
    expect(suggestConnection("", conns)).toBe("");
  });
});

describe("commitURL", () => {
  const sha = "4f2c1ab".padEnd(40, "0");
  it.each<[string, string | undefined]>([
    ["https://github.com/acme/api.git", `https://github.com/acme/api/commit/${sha}`],
    ["git@gitlab.com:acme/group/api.git", `https://gitlab.com/acme/group/api/-/commit/${sha}`],
    ["https://codeberg.org/acme/api", `https://codeberg.org/acme/api/commit/${sha}`],
    ["ssh://git@git.example.com:2222/acme/api.git", undefined],
  ])("%j", (repo, want) => {
    expect(commitURL(repo, sha)).toBe(want);
  });
});

describe("durationText", () => {
  it.each<[number | undefined, string]>([
    [undefined, "—"],
    [0, "0 s"],
    [59, "59 s"],
    [91, "1m 31s"],
    [3725, "1 h 02m"],
  ])("%j → %j", (s, want) => {
    expect(durationText(s)).toBe(want);
  });
});

describe("build status", () => {
  const build = (b: Partial<Build>): Build => ({
    name: "api-4f2c1ab-x", project: "shop", number: 118, app: "api", source: { repository: "https://github.com/acme/api", builder: "dockerfile" },
    commit: "4f2c1ab".padEnd(40, "0"), trigger: "push", deploy: true, phase: "running", created: "2026-10-04T09:00:00Z", ...b,
  });
  it.each<[Partial<Build>, string, string]>([
    [{ phase: "pending", statusMessage: "Queued behind 1 build" }, "Queued", "Queued behind 1 build"],
    [{ phase: "running" }, "Building", "—"],
    [{ phase: "running", cancelRequested: true }, "Cancelling", "stopping…"],
    [{ phase: "succeeded", deployedRevision: 42, current: true }, "Deployed", "revision 42"],
    [{ phase: "succeeded", deployedRevision: 41 }, "Superseded", "revision 41"],
    [{ phase: "succeeded" }, "Built", "not deployed yet"],
    [{ phase: "succeeded", deploy: false, trigger: "pull-request", pullRequest: 12 }, "Passed", "check for PR #12"],
    [{ phase: "failed", statusMessage: "tests failed in step 5" }, "Failed", "tests failed in step 5"],
    [{ phase: "cancelled" }, "Cancelled", "—"],
  ])("%j", (b, label, result) => {
    expect(buildLook(build(b)).label).toBe(label);
    expect(buildResult(build(b))).toBe(result);
  });
});

describe("stripAnsi", () => {
  it("removes colours and cursor codes, keeps the text", () => {
    expect(stripAnsi("\x1b[1;32m#5 DONE\x1b[0m 0.4s")).toBe("#5 DONE 0.4s");
    expect(stripAnsi("\x1b[2K\x1b[1Gprogress")).toBe("progress");
    expect(stripAnsi("\x1b]0;title\x07plain")).toBe("plain");
    expect(stripAnsi("no codes [1m here")).toBe("no codes [1m here");
  });
});

describe("connectionProblem", () => {
  const input = (c: Partial<ConnectionInput>): ConnectionInput => ({ name: "github-acme", provider: "github", url: "https://github.com", auth: "token", token: "ghp_x", ...c });
  it("accepts a complete connection", () => {
    expect(connectionProblem(input({}), true)).toBeUndefined();
    expect(connectionProblem(input({ auth: "githubApp", token: undefined, githubApp: { appID: 1, installationID: 2, privateKey: "-----BEGIN RSA PRIVATE KEY-----" } }), true)).toBeUndefined();
  });
  it.each<[Partial<ConnectionInput>, boolean, string]>([
    [{ name: "GitHub" }, true, "name"],
    [{ url: "http://github.com" }, true, "url"],
    [{ provider: "gitlab", auth: "githubApp" }, true, "auth"],
    [{ token: "" }, true, "token"],
    [{ auth: "sshKey", sshPrivateKey: "" }, true, "sshPrivateKey"],
    [{ auth: "sshKey", sshPrivateKey: "ssh-ed25519 AAAA… public" }, false, "sshPrivateKey"],
    [{ auth: "githubApp", githubApp: { appID: 0, installationID: 2 } }, false, "githubApp.appID"],
    [{ auth: "githubApp", githubApp: { appID: 1, installationID: 2 } }, true, "githubApp.privateKey"],
  ])("%j (creating: %s) → %s", (c, creating, field) => {
    expect(connectionProblem(input(c), creating)?.field).toBe(field);
  });
  it("lets an edit keep the stored secrets", () => {
    expect(connectionProblem(input({ token: "" }), false)).toBeUndefined();
  });
});
