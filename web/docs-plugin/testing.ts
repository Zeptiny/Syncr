// Helpers for the plugin's tests: an in-memory repository and options.

import path from "node:path";
import { fileURLToPath } from "node:url";

import { compareCodeUnits, type DocsFs, type DocsOptions } from "./model.ts";

/** The mini repository in testdata/repo (docs/ plus a few files outside). */
export const TESTDATA_ROOT = fileURLToPath(new URL("./testdata/repo", import.meta.url));

export function testOptions(repoRoot: string, extra: Partial<DocsOptions> = {}): DocsOptions {
  return {
    docsDir: path.join(repoRoot, "docs"),
    repoRoot,
    githubRepo: "https://github.com/o/r",
    githubOnly: ["development.md", "adr/**"],
    genFile: "src/docs.gen.ts",
    repoLinks: "check",
    ...extra,
  };
}

/** A repository in memory: repository-relative POSIX path -> content. */
export function memFs(files: Record<string, string | Buffer>): DocsFs {
  const dirs = new Set<string>();
  for (const f of Object.keys(files)) {
    for (let d = path.posix.dirname(f); d !== "."; d = path.posix.dirname(d)) dirs.add(d);
  }
  return {
    files: (dir) =>
      Object.keys(files)
        .filter((f) => f.startsWith(`${dir}/`))
        .sort(compareCodeUnits),
    kind: (rel) => (rel in files ? "file" : dirs.has(rel) ? "dir" : undefined),
    read: (rel) => {
      const content = files[rel];
      if (content === undefined) throw new Error(`no file ${rel}`);
      return Buffer.from(content);
    },
  };
}
