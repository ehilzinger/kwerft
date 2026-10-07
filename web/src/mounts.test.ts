// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

import { describe, expect, it } from "vitest";
import { checkMounts, describeMount, mountsOf, ownDisks, volumesOf, type Mount } from "./mounts";

const spec = [
  { path: "/cache", size: "1Gi", class: "local-nvme" },
  { path: "/media", volume: "media", readOnly: true },
  { path: "/home/app/.ssh", secret: "deploy-key" },
  { path: "/etc/tls", secret: "tls", mode: 0o400 },
];

describe("mounts", () => {
  it("edits shared Volumes and Secrets and keeps disks per replica aside", () => {
    const mounts = mountsOf(spec);
    expect(mounts.map((m) => m.path)).toEqual(["/media", "/home/app/.ssh", "/etc/tls"]);
    expect(ownDisks(spec)).toEqual([spec[0]]);
    // Unchanged, the spec round-trips: no mode appears, none is lost.
    expect([...ownDisks(spec), ...volumesOf(mounts)]).toEqual(spec);
  });

  it("trims what people type", () => {
    expect(volumesOf([{ path: " /keys ", volume: "", readOnly: true, secret: " ssh-key " }])).toEqual([{ path: "/keys", secret: "ssh-key" }]);
  });

  it.each<[string, Mount[], [number, string] | undefined]>([
    ["fine", mountsOf(spec), undefined],
    ["relative path", [{ path: "keys", volume: "", readOnly: true, secret: "a" }], [0, "A mount path is absolute, like /data."]],
    ["no secret name", [{ path: "/keys", volume: "", readOnly: true, secret: " " }], [0, "Enter the name of a Secret in the project."]],
    ["bad secret name", [{ path: "/keys", volume: "", readOnly: true, secret: "SSH_Key" }], [0, `"SSH_Key" is not a valid Secret name. Use lowercase letters, digits, - and ., like ssh-key.`]],
    ["no volume", [{ path: "/data", volume: "", readOnly: false }], [0, "Choose a volume."]],
    ["path twice", [{ path: "/d", volume: "v", readOnly: false }, { path: "/d", volume: "", readOnly: true, secret: "s" }], [1, "/d is mounted twice."]],
  ])("checks: %s", (_, mounts, want) => {
    expect(checkMounts(mounts)).toEqual(want);
  });

  it("describes each kind", () => {
    expect(spec.map(describeMount)).toEqual([
      "/cache (1Gi, local-nvme)",
      "/media ← volume media (read-only)",
      "/home/app/.ssh ← secret deploy-key",
      "/etc/tls ← secret tls (0400)",
    ]);
  });
});
