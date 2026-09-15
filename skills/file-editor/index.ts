import * as fs from "fs";
import * as path from "path";

const WORKSPACE_ROOT = process.env.FANTAZM_WORKSPACE_ROOT ?? process.cwd();

function resolveAndValidate(inputPath: string): string {
  const resolved = path.resolve(WORKSPACE_ROOT, inputPath);
  const rootReal = path.resolve(WORKSPACE_ROOT);
  if (!resolved.startsWith(rootReal)) {
    throw new Error(`Access denied: path "${inputPath}" is outside workspace`);
  }
  return resolved;
}

export function readFile(params: { path: string }): string {
  const filePath = resolveAndValidate(params.path);
  return fs.readFileSync(filePath, "utf-8");
}

export function writeFile(params: { path: string; content: string }): void {
  const filePath = resolveAndValidate(params.path);
  fs.mkdirSync(path.dirname(filePath), { recursive: true });
  fs.writeFileSync(filePath, params.content, "utf-8");
}

export function listFiles(params: { dir: string; pattern?: string }): string[] {
  const baseDir = resolveAndValidate(params.dir);
  const { pattern } = params;

  function walk(dir: string): string[] {
    const entries = fs.readdirSync(dir, { withFileTypes: true });
    const result: string[] = [];
    for (const ent of entries) {
      const fullPath = path.join(dir, ent.name);
      const relPath = path.relative(WORKSPACE_ROOT, fullPath);
      if (ent.isDirectory()) {
        result.push(...walk(fullPath));
      } else {
        const matches = !pattern || new RegExp(pattern).test(ent.name);
        if (matches) result.push(relPath);
      }
    }
    return result;
  }

  return walk(baseDir);
}

export const tools = [
  {
    name: "read_file",
    description: "Read the contents of a file",
    parameters: {
      type: "object",
      properties: { path: { type: "string", description: "File path" } },
      required: ["path"],
    },
  },
  {
    name: "write_file",
    description: "Write content to a file",
    parameters: {
      type: "object",
      properties: {
        path: { type: "string", description: "File path" },
        content: { type: "string", description: "Content to write" },
      },
      required: ["path", "content"],
    },
  },
  {
    name: "list_files",
    description: "List files in a directory recursively",
    parameters: {
      type: "object",
      properties: {
        dir: { type: "string", description: "Directory path" },
        pattern: { type: "string", description: "Optional regex pattern to filter filenames" },
      },
      required: ["dir"],
    },
  },
];
