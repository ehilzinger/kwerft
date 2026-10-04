import { describe, expect, it } from "vitest";
import type { SecondFactor } from "./api";
import type { Role } from "./access";
import {
  daysProblem, describeProjects, issuerHost, parseDomains, parseNames, parseSecondFactors, readLoginQuery, ssoErrorMessage, tokenRoles,
} from "./identity";

describe("parseSecondFactors", () => {
  it.each<[string | null, SecondFactor[] | undefined]>([
    ["passkey,totp,recovery", ["passkey", "totp", "recovery"]],
    ["totp", ["totp"]],
    ["recovery, totp", ["recovery", "totp"]],
    ["totp,totp,sms", ["totp"]],
    ["sms", undefined],
    ["", undefined],
    [null, undefined],
  ])("%j", (v, want) => {
    expect(parseSecondFactors(v)).toEqual(want);
  });
});

describe("readLoginQuery", () => {
  it("opens the second-factor step and drops the parameter", () => {
    expect(readLoginQuery("?second-factor=passkey,totp")).toEqual({ methods: ["passkey", "totp"], ssoError: undefined, search: "", changed: true });
  });

  it("explains a failed sign-in and keeps other parameters", () => {
    const q = readLoginQuery("?next=%2Fapps&sso=not_member");
    expect(q.ssoError).toBe("There is no account for your email address. Ask an owner or admin for an invite.");
    expect(q.methods).toBeUndefined();
    expect(new URLSearchParams(q.search).get("next")).toBe("/apps");
    expect(q.changed).toBe(true);
  });

  it("leaves a plain sign-in alone", () => {
    expect(readLoginQuery("?next=%2Fapps")).toEqual({ methods: undefined, ssoError: undefined, search: "?next=%2Fapps", changed: false });
    expect(readLoginQuery("").changed).toBe(false);
  });

  it("drops an empty second-factor list", () => {
    expect(readLoginQuery("?second-factor=")).toMatchObject({ methods: undefined, search: "", changed: true });
  });
});

describe("ssoErrorMessage", () => {
  it("knows the server's codes", () => {
    expect(ssoErrorMessage("limited")).toBe("Too many sign-in attempts. Wait 15 minutes and try again.");
    expect(ssoErrorMessage("domain")).toBe("Your email address is not in a domain this console admits.");
  });

  it.each(["failed", "nonsense", "", "toString", "__proto__"])("reads %j as a plain failure", (code) => {
    expect(ssoErrorMessage(code)).toBe("Single sign-on failed. Try again, or sign in with your password.");
  });
});

describe("parseDomains", () => {
  it.each<[string, string[]]>([
    ["example.com", ["example.com"]],
    ["example.com, Example.org  other.net", ["example.com", "example.org", "other.net"]],
    ["@example.com;example.com.\nshop.example", ["example.com", "shop.example"]],
    [" , ", []],
    ["", []],
  ])("%j", (s, want) => {
    expect(parseDomains(s)).toEqual(want);
  });
});

describe("issuerHost", () => {
  it.each<[string, string]>([
    ["https://accounts.google.com", "accounts.google.com"],
    ["https://login.microsoftonline.com/0000/v2.0", "login.microsoftonline.com"],
    ["https://sso.example.com:8443/realms/acme", "sso.example.com:8443"],
    ["not a url", "not a url"],
  ])("%j", (issuer, want) => {
    expect(issuerHost(issuer)).toBe(want);
  });
});

describe("tokenRoles", () => {
  it.each<[Role, boolean, Role[]]>([
    ["owner", false, ["owner", "admin", "developer", "viewer"]],
    ["admin", false, ["admin", "developer", "viewer"]],
    ["developer", false, ["developer", "viewer"]],
    ["viewer", false, ["viewer"]],
    ["owner", true, ["developer", "viewer"]],
    ["admin", true, ["developer", "viewer"]],
    ["viewer", true, ["viewer"]],
  ])("%s, limited %s", (mine, limited, want) => {
    expect(tokenRoles(mine, limited)).toEqual(want);
  });
});

describe("token form helpers", () => {
  it("splits typed project names", () => {
    expect(parseNames("shop, billing shop;ops")).toEqual(["shop", "billing", "ops"]);
    expect(parseNames("  ")).toEqual([]);
  });

  it.each<[string, boolean]>([["90", true], ["1", true], ["365", true], [" 30 ", true], ["0", false], ["366", false], ["1.5", false], ["", false], ["abc", false]])(
    "days %j ok: %s", (s, ok) => {
      expect(daysProblem(s, 365) === undefined).toBe(ok);
    },
  );

  it("describes the projects", () => {
    expect(describeProjects([])).toBe("All projects");
    expect(describeProjects(["shop", "ops"])).toBe("shop, ops");
  });
});
