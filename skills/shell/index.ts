import { execSync } from "child_process";
import * as path from "path";

// NOTE: this denylist is best-effort defense-in-depth, NOT the security
// boundary. Denylisting freeform shell is inherently leaky — the real gate is
// the host policy engine (shell is deny-by-default) plus subprocess isolation.
// These patterns exist to catch obvious foot-guns, not a determined attacker.
//
// The patterns below were tightened to close the trivial bypasses the
// previous versions had: pipe-to-interpreter that required a trailing space
// (so `curl x|bash` slipped through), and `rm -rf /` at end-of-string (the
// old `\/\s` needed whitespace after the slash). We normalize whitespace
// before matching so spacing tricks don't help.
const BLOCKED_PATTERNS = [
  // rm with a recursive+force flag (in any order/grouping) targeting / or /*
  /\brm\s+(?:-\S*\s+)*-\S*[rf]\S*[rf]?\S*\s+\/(?:\s|$|\*)/i,
  /\brm\s+-[rf]{1,}\s+\/(?:\s|$|\*)/i,
  /\bformat\b/i,
  /\bmkfs[.\b]/i,
  /\bdd\s+if=/i,
  // fork bomb :(){ :|:& };:
  /:\s*\(\s*\)\s*\{/,
  // download-and-pipe-into-an-interpreter, regardless of spacing or sudo
  /\b(?:curl|wget|fetch)\b[^\n|]*\|\s*(?:sudo\s+)?(?:bash|sh|zsh|dash|python[0-9.]*|perl|ruby|node)\b/i,
  // base64/echo decoded straight into a shell
  /\bbase64\s+(?:-d|--decode|-D)\b[^\n|]*\|\s*(?:sudo\s+)?(?:bash|sh|zsh)\b/i,
  /\bchmod\s+-[A-Za-z]*R[A-Za-z]*\s+0?777\s+\//i,
  /\bchown\s+-[A-Za-z]*R[A-Za-z]*\s+\S+\s+\/(?:\s|$)/i,
  // overwrite a raw disk device
  />\s*\/dev\/(?:sd|nvme|disk|hd)/i,
];

function isBlocked(command: string): boolean {
  // Collapse runs of whitespace so `curl  x  |  bash` and `curl x|bash`
  // normalize to the same shape the patterns expect.
  const normalized = command.trim().replace(/\s+/g, " ");
  return BLOCKED_PATTERNS.some((re) => re.test(normalized));
}

export async function execute(params: {
  command: string;
  cwd?: string;
  timeout?: number;
}): Promise<{ stdout: string; stderr: string; exitCode: number }> {
  const { command, cwd = process.cwd(), timeout = 30000 } = params;

  if (isBlocked(command)) {
    throw new Error("Command blocked by security policy");
  }

  try {
    const result = execSync(command, {
      encoding: "utf-8",
      cwd: path.resolve(cwd),
      timeout,
      maxBuffer: 1024 * 1024,
    });
    return {
      stdout: result,
      stderr: "",
      exitCode: 0,
    };
  } catch (err: unknown) {
    const e = err as { stdout?: string; stderr?: string; status?: number };
    return {
      stdout: e.stdout ?? "",
      stderr: e.stderr ?? String(err),
      exitCode: e.status ?? 1,
    };
  }
}

export const tools = [
  {
    name: "shell_exec",
    description: "Execute a shell command",
    parameters: {
      type: "object",
      properties: {
        command: { type: "string", description: "Shell command to run" },
        cwd: { type: "string", description: "Working directory" },
        timeout: { type: "number", description: "Timeout in milliseconds (default 30000)" },
      },
      required: ["command"],
    },
  },
];
