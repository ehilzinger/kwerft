// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";
import {
  backupPill, bytes, describeEtcdUpload, describeRestore, etcdFetchCommand, etcdUploadPill, groupKey, recoveryKeyFile, recoveryKeyFileName,
  recoveryKeyProblem, restorePill, retentionLabel, suggestedTarget, targetPill,
} from "./backups";
import { conditions, describeCondition } from "./alerts";

const key = "ABCD-EFGH-IJKL-MNOP-QRST-UVWX-YZ23-4567-ABCD-EFGH-IJKL-MNOP-QRST";

describe("recovery key", () => {
  it("is shown in groups of 4, however it was typed", () => {
    expect(groupKey(key)).toHaveLength(13);
    expect(groupKey(key.toLowerCase().replace(/-/g, " "))).toEqual(groupKey(key));
  });

  it.each<[string, string | undefined]>([
    [key, undefined],
    [key.toLowerCase().replace(/-/g, ""), undefined],
    ["", undefined],
    ["ABCD-EFGH", "A recovery key has 52 letters and digits; this has 8."],
    [key.replace("A", "1"), "A recovery key has only the letters A–Z and the digits 2–7."],
  ])("recoveryKeyProblem(%j)", (k, want) => {
    expect(recoveryKeyProblem(k)).toBe(want);
  });

  it("downloads as a file that says what it is for", () => {
    const file = recoveryKeyFile(key, "ops.example.com", new Date("2026-10-05T12:00:00Z"));
    expect(file).toContain(key);
    expect(file).toContain("Console: ops.example.com");
    expect(file).toContain("install.sh --restore");
    expect(recoveryKeyFileName("ops.example.com")).toBe("kwerft-recovery-key-ops.example.com.txt");
  });
});

describe("presentation", () => {
  it.each<[number, string]>([[0, "—"], [512, "512 B"], [5 << 20, "5.0 MiB"], [1536 * 1024 * 1024, "1.5 GiB"], [300 << 20, "300 MiB"]])("bytes(%j) = %j", (n, want) => {
    expect(bytes(n)).toBe(want);
  });

  it.each<[string, string]>([["14d", "14 days"], ["1d", "1 day"], ["72h", "72 hours"], ["1d12h", "1d12h"]])("retentionLabel(%j) = %j", (r, want) => {
    expect(retentionLabel(r)).toBe(want);
  });

  it("names phases", () => {
    expect(backupPill("Completed")).toEqual({ pill: "ok", label: "Completed" });
    expect(backupPill("PartiallyFailed").pill).toBe("warn");
    expect(backupPill("Failed").pill).toBe("bad");
    expect(backupPill("InProgress").label).toBe("Running");
    expect(restorePill("Completed").label).toBe("Restored");
    expect(restorePill("InProgress").label).toBe("Restoring");
    expect(targetPill({ state: "NotConfigured", configured: false }).label).toBe("Not set up");
    expect(targetPill({ state: "NotConfigured", configured: true }).label).toBe("Incomplete");
    expect(targetPill({ state: "Error", configured: true }).pill).toBe("bad");
  });
});

describe("etcd snapshot uploads", () => {
  const now = Date.parse("2026-10-05T14:00:00Z");
  it("say per node what is in the bucket", () => {
    const ok = { node: "server-1", name: "etcd-snapshot-server-1-1759665600.zip", uploadedAt: "2026-10-05T12:00:00Z", stored: 4 };
    expect(etcdUploadPill(ok)).toEqual({ pill: "ok", label: "Uploaded" });
    expect(describeEtcdUpload(ok, now)).toBe("etcd-snapshot-server-1-1759665600.zip, uploaded 2 h ago · 4 snapshots in the bucket");
    expect(describeEtcdUpload({ ...ok, stored: 1, uploadedAt: undefined }, now)).toBe("etcd-snapshot-server-1-1759665600.zip · 1 snapshot in the bucket");
    const failing = { node: "server-2", stored: 0, message: "cannot list the bucket: AccessDenied" };
    expect(etcdUploadPill(failing).pill).toBe("bad");
    expect(describeEtcdUpload(failing, now)).toBe("cannot list the bucket: AccessDenied");
    expect(etcdUploadPill({ node: "server-3", stored: 0 }).label).toBe("None yet");
  });

  it("are read back with kwerft etcd-snapshot", () => {
    expect(etcdFetchCommand()).toBe("kwerft etcd-snapshot fetch --config kwerft.yaml --name latest");
    expect(etcdFetchCommand("server-2")).toBe("kwerft etcd-snapshot fetch --config kwerft.yaml --node server-2 --name latest");
  });

  it("are encrypted with the recovery key, as the key file says", () => {
    expect(recoveryKeyFile(key, "ops.example.com", new Date())).toContain("etcd snapshots");
  });
});

describe("restores", () => {
  it("say what they do", () => {
    expect(describeRestore({ backup: "b", project: "shop" })).toMatch(/^Restores the project shop in place: only what no longer exists comes back/);
    expect(describeRestore({ backup: "b", project: "shop", targetProject: "shop-restored" }))
      .toBe("Restores the project shop into the new project shop-restored, next to the original.");
    expect(describeRestore({ backup: "b", project: "shop", apps: ["web"], targetProject: "shop" })).toMatch(/^Restores the app web of shop in place/);
    expect(describeRestore({ backup: "b", project: "shop", apps: ["api", "web"] })).toMatch(/^Restores the apps api, web of shop/);
  });

  it("suggest a new project's name that is free", () => {
    expect(suggestedTarget("shop", [])).toBe("shop-restored");
    expect(suggestedTarget("shop", ["shop-restored"])).toBe("shop-restored-2");
    expect(suggestedTarget("shop", ["shop-restored", "shop-restored-2"])).toBe("shop-restored-3");
    expect(suggestedTarget("a".repeat(40), []).length).toBeLessThanOrEqual(40);
  });
});

describe("backup alerts", () => {
  it("are platform conditions", () => {
    expect(conditions.BackupFailing.scope).toBe("none");
    expect(describeCondition({ condition: "BackupMissing" })).toBe("A backup plan has not completed a backup within twice its interval");
    expect(describeCondition({ condition: "BackupMissing", window: "72h0m0s" })).toBe("A backup plan has not completed a backup within 3 days");
  });
});
