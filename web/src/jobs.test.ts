import { describe, expect, it } from "vitest";
import { outOfMemory, runEnd } from "./jobs";

describe("runEnd", () => {
  it.each<[Parameters<typeof runEnd>[0], string | undefined, string | undefined]>([
    [{}, undefined, undefined],
    [{ exitCode: 3, terminationReason: "Error" }, "exit code 3", "exit 3"],
    [{ exitCode: 0, terminationReason: "Completed" }, "exit code 0", "exit 0"],
    // The kernel's OOM kill is exit code 137; the reason says what it was.
    [{ exitCode: 137, terminationReason: "OOMKilled", memoryLimit: "256Mi" }, "out of memory (limit 256Mi)", "out of memory"],
    [{ exitCode: 137, terminationReason: "OOMKilled" }, "out of memory", "out of memory"],
    // 137 without the reason is any SIGKILL, not necessarily memory.
    [{ exitCode: 137, terminationReason: "Error" }, "exit code 137", "exit 137"],
  ])("%j → %j / %j", (r, long, short) => {
    expect(runEnd(r)).toBe(long);
    expect(runEnd(r, true)).toBe(short);
    expect(outOfMemory(r)).toBe(r.terminationReason === "OOMKilled");
  });
});
