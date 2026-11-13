import { describe, expect, it } from "vitest";

import { commonParent } from "@/components/app/file-browser/selection";

import golden from "./rclone-filters.golden.json";
import {
  escapeFilterName,
  filterPattern,
  MAX_SELECTION_ITEMS,
  relativePath,
  selectionFilters,
  type FilterItem,
} from "./rclone-filters";

describe("rclone filters golden cases (checked with real rclone)", () => {
  it("has cases and names the rclone version", () => {
    expect(golden.cases.length).toBeGreaterThan(5);
    expect(golden.rclone).toMatch(/^v1\.\d+\.\d+$/);
  });

  for (const c of golden.cases) {
    it(c.name, () => {
      const parent = commonParent(
        c.selection.map((s) => s.path),
        "remote",
      );
      expect(parent).toBe(c.source);
      expect(selectionFilters(c.selection, parent)).toEqual({
        source: c.source,
        filters: c.filters,
      });
      // Every selected item is among what rclone matched.
      for (const s of c.selection) {
        const rel = relativePath(s.path, c.source) ?? "";
        expect(c.matched).toContain(s.dir ? `${rel}/` : rel);
      }
    });
  }

  it("holds no absolute paths", () => {
    const text = JSON.stringify(golden);
    expect(text).not.toMatch(/\/(dev|tmp|root|home)\//);
  });
});

describe("escapeFilterName", () => {
  it("escapes rclone's glob characters and nothing else", () => {
    expect(escapeFilterName("a*b?c[d]e{f,g}h\\i")).toBe("a\\*b\\?c\\[d\\]e\\{f,g\\}h\\\\i");
    expect(escapeFilterName("-rf +x #y ;z !w ünï ✓")).toBe("-rf +x #y ;z !w ünï ✓");
  });
});

describe("filterPattern", () => {
  it("anchors files and folders", () => {
    expect(filterPattern("a/b.txt", false)).toBe("/a/b.txt");
    expect(filterPattern("a/b", true)).toBe("/a/b/**");
    expect(filterPattern("x*/y?", true)).toBe("/x\\*/y\\?/**");
  });

  it("keeps whitespace at the end of a file name as a class", () => {
    expect(filterPattern("trail ", false)).toBe("/trail[ ]");
    expect(filterPattern("two\t ", false)).toBe("/two[\t][ ]");
    expect(filterPattern("dir /f", false)).toBe("/dir /f");
    expect(filterPattern("dir ", true)).toBe("/dir /**");
  });
});

describe("relativePath", () => {
  it("handles a remote's paths", () => {
    expect(relativePath("a/b", "")).toBe("a/b");
    expect(relativePath("a/b/c", "a")).toBe("b/c");
    expect(relativePath("ab/c", "a")).toBeUndefined();
    expect(relativePath("a", "a")).toBeUndefined();
  });

  it("handles POSIX worker paths, where a backslash is part of a name", () => {
    expect(relativePath("/srv", "/")).toBe("srv");
    expect(relativePath("/srv/docs/x\\y", "/srv")).toBe("docs/x\\y");
    expect(relativePath("/srvx/a", "/srv")).toBeUndefined();
  });

  it("handles Windows worker paths without case and with either separator", () => {
    expect(relativePath("C:\\Users\\ada\\Documents\\x.txt", "C:\\Users\\ada")).toBe(
      "Documents/x.txt",
    );
    expect(relativePath("d:\\shared\\Projects", "D:\\")).toBe("shared/Projects");
    expect(relativePath("C:\\Users\\adam", "C:\\Users\\ada")).toBeUndefined();
  });
});

describe("selectionFilters", () => {
  const f = (path: string): FilterItem => ({ path, dir: false });
  const d = (path: string): FilterItem => ({ path, dir: true });

  it("builds the transfer of a worker's items", () => {
    expect(selectionFilters([d("/srv/photos/2026"), f("/srv/docs/notes.md")], "/srv")).toEqual({
      source: "/srv",
      filters: ["+ /photos/2026/**", "+ /docs/notes.md", "- **"],
    });
    expect(
      selectionFilters([f("C:\\Users\\ada\\Documents\\budget.xlsx")], "C:\\Users\\ada\\Documents"),
    ).toEqual({
      source: "C:\\Users\\ada\\Documents",
      filters: ["+ /budget.xlsx", "- **"],
    });
  });

  it("refuses what filters cannot name", () => {
    expect(selectionFilters([], "")).toEqual({ error: "Select at least one item." });
    expect(selectionFilters([f("C:\\a"), f("D:\\b")], undefined)).toHaveProperty(
      "error",
      expect.stringContaining("different drives"),
    );
    expect(selectionFilters([f("a/line\nbreak")], "a")).toHaveProperty(
      "error",
      expect.stringContaining("line break"),
    );
    expect(selectionFilters([f("b/x")], "a")).toHaveProperty("error", "b/x is not inside a.");
    expect(selectionFilters([f(`a/${"x".repeat(4100)}`)], "a")).toHaveProperty(
      "error",
      expect.stringContaining("too long"),
    );
    const many = Array.from({ length: MAX_SELECTION_ITEMS + 1 }, (_, i) => f(`a/${i}`));
    expect(selectionFilters(many, "a")).toHaveProperty(
      "error",
      expect.stringContaining("at most 999"),
    );
    expect(selectionFilters(many.slice(1), "a")).toHaveProperty("filters");
  });
});
