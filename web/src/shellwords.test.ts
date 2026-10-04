import { describe, expect, it } from "vitest";
import { CommandSyntaxError, formatCommand, splitCommand } from "./shellwords";

describe("splitCommand", () => {
  it.each<[string, string[]]>([
    ["echo hi", ["echo", "hi"]],
    ["  echo   hi  ", ["echo", "hi"]],
    [`echo "hi there"`, ["echo", "hi there"]],
    [`sh -c 'echo $HOME && date'`, ["sh", "-c", "echo $HOME && date"]],
    [`printf "%s\\n" a`, ["printf", "%s\\n", "a"]],
    [`echo "say \\"hi\\""`, ["echo", 'say "hi"']],
    [`echo it\\'s`, ["echo", "it's"]],
    [`a""b ''`, ["ab", ""]],
    ["", []],
  ])("%j", (line, want) => {
    expect(splitCommand(line)).toEqual(want);
  });

  it.each([`echo "hi`, "echo 'hi", "echo \\"])("rejects %j", (line) => {
    expect(() => splitCommand(line)).toThrow(CommandSyntaxError);
  });
});

describe("formatCommand", () => {
  it.each<string[][]>([
    [["echo", "hi"]],
    [["sh", "-c", "echo $HOME && date"]],
    [["echo", "it's"]],
    [["echo", ""]],
    [["/usr/bin/env", "FOO=1", "./run.sh", "--flag=a,b"]],
  ])("round-trips %j", (args) => {
    expect(splitCommand(formatCommand(args))).toEqual(args);
  });

  it("leaves plain words unquoted", () => {
    expect(formatCommand(["echo", "hi"])).toBe("echo hi");
    expect(formatCommand(["sh", "-c", "echo hi"])).toBe("sh -c 'echo hi'");
  });
});
