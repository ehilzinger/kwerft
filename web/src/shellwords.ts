// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

// Commands are typed as one line and split like a POSIX shell splits words,
// without running a shell: no variables, globs, pipes or redirection — for
// those, the command is `sh -c '…'`. This is what people expect from
// Dockerfiles and docker run, and it avoids the trap of a single argument
// "echo hi" that names a program that does not exist.

export class CommandSyntaxError extends Error {}

/** Splits a command line into arguments, honouring '…', "…" and \ escapes. */
export function splitCommand(line: string): string[] {
  const args: string[] = [];
  let cur = "";
  let inWord = false;
  for (let i = 0; i < line.length; i++) {
    const c = line[i]!;
    if (c === "'") {
      const end = line.indexOf("'", i + 1);
      if (end < 0) throw new CommandSyntaxError("A ' quote is not closed.");
      cur += line.slice(i + 1, end);
      i = end;
      inWord = true;
    } else if (c === '"') {
      i++;
      for (; i < line.length && line[i] !== '"'; i++) {
        // Inside double quotes a backslash only escapes " \ and $ and `.
        if (line[i] === "\\" && i + 1 < line.length && `"\\$\``.includes(line[i + 1]!)) i++;
        cur += line[i];
      }
      if (i >= line.length) throw new CommandSyntaxError('A " quote is not closed.');
      inWord = true;
    } else if (c === "\\") {
      if (i + 1 >= line.length) throw new CommandSyntaxError("The command ends with a lone \\.");
      cur += line[++i];
      inWord = true;
    } else if (/\s/.test(c)) {
      if (inWord) args.push(cur);
      cur = "";
      inWord = false;
    } else {
      cur += c;
      inWord = true;
    }
  }
  if (inWord) args.push(cur);
  return args;
}

/** Formats arguments as one line that splitCommand reads back unchanged. */
export function formatCommand(args: string[]): string {
  return args.map(quote).join(" ");
}

function quote(arg: string): string {
  if (arg === "") return "''";
  if (/^[A-Za-z0-9_@%+=:,./-]+$/.test(arg)) return arg;
  return "'" + arg.replaceAll("'", `'\\''`) + "'";
}
