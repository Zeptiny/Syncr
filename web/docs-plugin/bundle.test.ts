// @vitest-environment node
import { describe, expect, it } from "vitest";

import { eagerModules, type BundleFile } from "./bundle.ts";

const isDocs = (id: string) => id.startsWith("\0virtual:syncr-docs/");

function chunk(
  fileName: string,
  imports: string[],
  moduleIds: string[],
  isEntry = false,
): BundleFile {
  return { type: "chunk", fileName, isEntry, imports, moduleIds };
}

function bundleOf(...files: BundleFile[]): Record<string, BundleFile> {
  return Object.fromEntries(
    files.map((f, i) => [f.type === "chunk" ? f.fileName : `asset-${i}`, f]),
  );
}

describe("eagerModules", () => {
  it("accepts docs that only dynamic imports reach", () => {
    const bundle = bundleOf(
      chunk("assets/index.js", ["assets/react.js"], ["/src/main.tsx"], true),
      chunk("assets/react.js", [], ["/node_modules/react/index.js"]),
      // The lazy chunks: reached through import(), not in imports.
      chunk(
        "assets/manifest.js",
        ["assets/react.js"],
        ["/src/features/docs/manifest.ts", "\0virtual:syncr-docs/manifest"],
      ),
      chunk("assets/doc-restic.js", [], ["\0virtual:syncr-docs/page/restic"]),
      { type: "asset" },
    );
    expect(eagerModules(bundle, isDocs)).toEqual([]);
  });

  it("finds docs in the entry chunk and in chunks it imports statically", () => {
    const bundle = bundleOf(
      chunk(
        "assets/index.js",
        ["assets/shared.js"],
        ["/src/main.tsx", "\0virtual:syncr-docs/manifest"],
        true,
      ),
      chunk("assets/shared.js", ["assets/deeper.js", "assets/index.js"], ["/src/lib/utils.ts"]),
      chunk("assets/deeper.js", [], ["\0virtual:syncr-docs/search"]),
      chunk("assets/lazy.js", [], ["\0virtual:syncr-docs/page/api"]),
    );
    expect(eagerModules(bundle, isDocs)).toEqual([
      "assets/deeper.js: virtual:syncr-docs/search",
      "assets/index.js: virtual:syncr-docs/manifest",
    ]);
  });

  it("does not mistake app code that names a docs anchor for the docs", () => {
    const bundle = bundleOf(
      chunk("assets/index.js", [], ["/src/main.tsx", "/src/features/jobs/backup-editor.tsx"], true),
    );
    expect(eagerModules(bundle, isDocs)).toEqual([]);
  });
});
