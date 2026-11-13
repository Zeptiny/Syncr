import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { describe, expect, it } from "vitest";

import {
  ProbeEntryType,
  ResticDiffChangeSchema,
  ResticDiffModifier,
  ResticFindMatchSchema,
  ResticSnapshotSchema,
} from "@/gen/syncr/v1/link_pb";

import {
  changePlace,
  escapeGlob,
  exactPathPattern,
  filterChanges,
  folderOf,
  groupMatches,
  kindCounts,
  olderFirst,
  onlyPath,
} from "./scans";
import { validateCompareSearch, validateFindSearch } from "./search";

const t0 = Date.UTC(2026, 10, 1);
const snap = (id: string, day: number) =>
  create(ResticSnapshotSchema, {
    id: id.padEnd(64, "0"),
    time: timestampFromMs(t0 + day * 86_400_000),
  });
const match = (snapshot: string, path: string, size: number, mtime: number, dir = false) =>
  create(ResticFindMatchSchema, {
    snapshotId: snapshot.padEnd(64, "0"),
    entry: {
      name: path.split("/").at(-1),
      path,
      type: dir ? ProbeEntryType.DIR : ProbeEntryType.FILE,
      size: BigInt(size),
      modTime: timestampFromMs(t0 + mtime),
    },
  });
const change = (path: string, modifier: ResticDiffModifier) =>
  create(ResticDiffChangeSchema, { path, modifier });

describe("find patterns", () => {
  it("escapes glob characters as classes, which also hold on Windows", () => {
    expect(escapeGlob("[x] todo?.md")).toBe("[[]x] todo[?].md");
    expect(escapeGlob("a*b\\c")).toBe("a[*]b[\\\\]c");
    expect(escapeGlob("plain name.txt")).toBe("plain name.txt");
  });

  it("anchors an exact path at the root", () => {
    expect(exactPathPattern("/srv/photos/[x] todo.md")).toBe("/srv/photos/[[]x] todo.md");
    expect(exactPathPattern("/C/Users/ada/*.txt")).toBe("/C/Users/ada/[*].txt");
  });

  it("keeps only the exact path of a folder's subtree", () => {
    const ms = [
      match("a", "/srv/docs", 0, 0, true),
      match("a", "/srv/docs/notes.md", 1, 0),
      match("b", "/srv/docs", 0, 0, true),
    ];
    expect(onlyPath(ms, "/srv/docs").map((m) => m.snapshotId[0])).toEqual(["a", "b"]);
  });
});

describe("groupMatches", () => {
  const snaps = [snap("c", 3), snap("b", 2), snap("a", 1)];

  it("groups by path in the order found, and versions by size and time, newest first", () => {
    const groups = groupMatches(
      [
        match("c", "/srv/albums.json", 30, 300),
        match("c", "/srv/2026", 0, 5, true),
        match("b", "/srv/albums.json", 20, 200),
        match("a", "/srv/albums.json", 20, 200),
        match("b", "/srv/2026", 0, 9, true),
      ],
      snaps,
    );
    expect(groups.map((g) => g.path)).toEqual(["/srv/albums.json", "/srv/2026"]);
    const [albums, folder] = groups;
    expect(albums.snapshotId).toBe("c".padEnd(64, "0"));
    expect(albums.entry.size).toBe(30n);
    expect(albums.snapshotIds.map((s) => s[0])).toEqual(["c", "b", "a"]);
    expect(albums.versions.map((v) => [v.entry.size, v.snapshotIds.map((s) => s[0])])).toEqual([
      [30n, ["c"]],
      [20n, ["b", "a"]],
    ]);
    // A folder is one version, whatever its time.
    expect(folder.versions).toHaveLength(1);
    expect(folder.versions[0].snapshotIds).toHaveLength(2);
  });

  it("orders by the snapshots' times, else by the search's order", () => {
    const groups = groupMatches([match("a", "/x", 1, 1), match("c", "/x", 2, 2)], snaps);
    expect(groups[0].versions.map((v) => v.entry.size)).toEqual([2n, 1n]);
    const unknown = groupMatches([match("e", "/x", 1, 1), match("d", "/x", 2, 2)]);
    expect(unknown[0].versions.map((v) => v.entry.size)).toEqual([1n, 2n]);
  });

  it("opens a match's folder", () => {
    expect(folderOf("/srv/photos/a.jpg")).toBe("/srv/photos");
    expect(folderOf("/srv")).toBe("/");
  });
});

describe("compare", () => {
  const changes = [
    change("/srv/new.txt", ResticDiffModifier.ADDED),
    change("/srv/old/", ResticDiffModifier.REMOVED),
    change("/srv/a.txt", ResticDiffModifier.CHANGED),
    change("/srv/b.txt", ResticDiffModifier.CHANGED),
    change("/srv/link", ResticDiffModifier.TYPE),
  ];

  it("counts only the kinds that occur, in display order", () => {
    expect(kindCounts(changes).map((k) => [k.label, k.count])).toEqual([
      ["Added", 1],
      ["Removed", 1],
      ["Changed", 2],
      ["Type changed", 1],
    ]);
  });

  it("filters by kind and text", () => {
    expect(filterChanges(changes, new Set(), "")).toHaveLength(5);
    expect(filterChanges(changes, new Set(["changed"]), "").map((c) => c.path)).toEqual([
      "/srv/a.txt",
      "/srv/b.txt",
    ]);
    expect(
      filterChanges(changes, new Set(["changed", "added"]), " B.TXT ").map((c) => c.path),
    ).toEqual(["/srv/b.txt"]);
  });

  it("opens a change where it can be seen", () => {
    expect(changePlace(changes[0], "f", "t")).toEqual({ snapshotId: "t", path: "/srv" });
    expect(changePlace(changes[1], "f", "t")).toEqual({ snapshotId: "f", path: "/srv/old" });
    expect(changePlace(changes[4], "f", "t")).toEqual({ snapshotId: "t", path: "/srv" });
  });

  it("puts the older snapshot first", () => {
    const [a, b] = [snap("a", 1), snap("b", 2)];
    expect(olderFirst(b, a)).toEqual({ from: a, to: b });
    expect(olderFirst(a, b)).toEqual({ from: a, to: b });
  });
});

describe("URL state", () => {
  it("keeps a search's pattern, scope and case", () => {
    expect(validateFindSearch({ q: " *.jpg\n", snapshot: "ABCDEF12", icase: false, x: 1 })).toEqual(
      { q: "*.jpg", snapshot: "abcdef12", icase: false, worker: undefined },
    );
    expect(validateFindSearch({ q: "", snapshot: "latest", icase: true })).toEqual({
      q: undefined,
      snapshot: undefined,
      icase: undefined,
      worker: undefined,
    });
  });

  it("keeps a comparison's snapshots and folder", () => {
    expect(validateCompareSearch({ from: "1234abcd", to: "nope", path: "/srv//docs/" })).toEqual({
      from: "1234abcd",
      to: undefined,
      path: "/srv/docs",
      worker: undefined,
    });
    expect(validateCompareSearch({ path: "/" }).path).toBeUndefined();
    expect(validateCompareSearch({ path: "srv" }).path).toBeUndefined();
  });
});
