import { describe, expect, it } from "vitest";

import { joinPath, normalizePath, parentPath, pathCrumbs } from "./paths";
import { isWindowsTree, isWithinRoots, treeToLocal } from "./restic-paths";

describe("treeToLocal", () => {
  it.each<[string, string, string | undefined]>([
    ["/srv/photos/2026", "linux", "/srv/photos/2026"],
    ["/", "linux", "/"],
    ["/home/ada", "darwin", "/home/ada"],
    ["/C/Users/ada/Documents", "windows", "C:\\Users\\ada\\Documents"],
    ["/c/Users/ada", "Windows", "C:\\Users\\ada"],
    ["/D", "windows", "D:\\"],
    ["/D/", "windows", "D:\\"],
    // No safe answer: the tree root, a UNC-like or unknown first segment,
    // a segment that could not be a Windows name.
    ["/", "windows", undefined],
    ["/server/share/x", "windows", undefined],
    ["/C/a:b", "windows", undefined],
    ["/C/a\\b", "windows", undefined],
    ["relative/path", "linux", undefined],
    ["", "windows", undefined],
  ])("%s on %s", (tree, os, want) => {
    expect(treeToLocal(tree, os)).toBe(want);
  });

  it("treats an unknown OS as not Windows", () => {
    expect(treeToLocal("/C/Users", undefined)).toBe("/C/Users");
  });
});

describe("isWithinRoots", () => {
  it.each<[string, string[], string, boolean]>([
    ["/srv/restore", ["/srv", "/mnt"], "linux", true],
    ["/srv", ["/srv"], "linux", true],
    ["/srv/", ["/srv"], "linux", true],
    ["/srvx/restore", ["/srv"], "linux", false],
    ["/home/ada", ["/srv", "/mnt"], "linux", false],
    ["/srv/../etc", ["/srv"], "linux", false],
    ["/Srv/restore", ["/srv"], "linux", false],
    ["/anything", ["/"], "linux", true],
    ["", ["/srv"], "linux", false],
    ["/srv/x", [], "linux", false],
    ["D:\\Restore\\x", ["D:\\Restore"], "windows", true],
    ["d:\\restore\\X", ["D:\\Restore"], "windows", true],
    ["D:/Restore/x", ["D:\\Restore"], "windows", true],
    ["D:\\RestoreOther", ["D:\\Restore"], "windows", false],
    ["C:\\Users\\ada", ["C:\\"], "windows", true],
    ["D:\\Restore\\..\\Windows", ["D:\\Restore"], "windows", false],
  ])("%s in %j on %s", (path, roots, os, want) => {
    expect(isWithinRoots(path, roots, os)).toBe(want);
  });
});

describe("isWindowsTree", () => {
  it("recognizes snapshots made on Windows by their paths", () => {
    expect(isWindowsTree(["C:\\Users\\ada\\Documents"])).toBe(true);
    expect(isWindowsTree(["\\\\nas\\share"])).toBe(true);
    expect(isWindowsTree(["/srv/photos"])).toBe(false);
    expect(isWindowsTree([])).toBe(false);
  });
});

describe("browser paths", () => {
  it("joins and splits remote, snapshot and worker paths", () => {
    expect(joinPath("", "restic", "remote")).toBe("restic");
    expect(joinPath("restic", "photos", "remote")).toBe("restic/photos");
    expect(joinPath("/", "srv", "snapshot")).toBe("/srv");
    expect(joinPath("/srv", "photos", "worker")).toBe("/srv/photos");
    expect(joinPath("C:\\", "Users", "worker")).toBe("C:\\Users");
    expect(joinPath("C:\\Users", "ada", "worker")).toBe("C:\\Users\\ada");
    expect(pathCrumbs("/C/Users", "snapshot")).toEqual([
      { label: "C", path: "/C" },
      { label: "Users", path: "/C/Users" },
    ]);
    expect(pathCrumbs("a/b", "remote").map((c) => c.path)).toEqual(["a", "a/b"]);
    expect(pathCrumbs("C:\\Users\\ada", "worker").map((c) => c.path)).toEqual([
      "C:\\",
      "C:\\Users",
      "C:\\Users\\ada",
    ]);
  });

  it("goes up one folder, and from an allowed folder back to their list", () => {
    expect(parentPath("restic/photos", "remote")).toBe("restic");
    expect(parentPath("restic", "remote")).toBe("");
    expect(parentPath("/srv/photos", "snapshot")).toBe("/srv");
    expect(parentPath("/srv", "snapshot")).toBe("/");
    expect(parentPath("/", "snapshot")).toBe("/");
    expect(parentPath("/srv/photos/2026", "worker", ["/srv/photos"])).toBe("/srv/photos");
    expect(parentPath("/srv/photos", "worker", ["/srv/photos"])).toBe("");
    expect(parentPath("D:\\Shared\\x", "worker", ["D:\\Shared"])).toBe("D:\\Shared");
    expect(parentPath("D:\\Shared", "worker", ["D:\\Shared"])).toBe("");
  });

  it("normalizes typed paths", () => {
    expect(normalizePath(" /restic/photos/ ", "remote")).toBe("restic/photos");
    expect(normalizePath("srv//photos/", "snapshot")).toBe("/srv/photos");
    expect(normalizePath("/srv/photos/", "worker")).toBe("/srv/photos");
    expect(normalizePath("C:", "worker")).toBe("C:\\");
    expect(normalizePath("C:\\Users\\", "worker")).toBe("C:\\Users");
  });
});
