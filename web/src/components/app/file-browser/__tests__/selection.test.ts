import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";

import { ProbeEntrySchema, ProbeEntryType, type ProbeEntry } from "@/gen/syncr/v1/link_pb";

import {
  commonParent,
  coveringFolder,
  emptySelection,
  fromEntries,
  groupByFolder,
  isInside,
  knownSize,
  pathsText,
  rangeBetween,
  removePaths,
  setSelected,
  uncovered,
  withPaths,
} from "../selection";

const entry = (path: string, type: ProbeEntryType, size = 0): ProbeEntry =>
  create(ProbeEntrySchema, {
    name: path.split(/[\\/]/).filter(Boolean).at(-1) ?? path,
    path,
    type,
    size: BigInt(size),
  });
const dir = (path: string) => entry(path, ProbeEntryType.DIR);
const file = (path: string, size = 10) => entry(path, ProbeEntryType.FILE, size);

describe("isInside", () => {
  it.each([
    ["/srv/a/b", "/srv/a", "snapshot", true],
    ["/srv/ab", "/srv/a", "snapshot", false],
    ["/srv/a", "/srv/a", "snapshot", false],
    ["/srv", "/", "snapshot", true],
    ["a/b", "a", "remote", true],
    ["ab", "a", "remote", false],
    ["a", "", "remote", true],
    ["/srv/x", "/srv", "worker", true],
    ["/srv/x", "/", "worker", true],
    ["C:\\Users\\ada\\x", "c:\\users\\ada", "worker", true],
    ["C:\\Users\\adam", "C:\\Users\\ada", "worker", false],
    ["C:\\x", "C:\\", "worker", true],
    ["D:\\x", "C:\\", "worker", false],
  ] as const)("%s in %s (%s) is %s", (path, folder, kind, want) => {
    expect(isInside(path, folder, kind)).toBe(want);
  });
});

describe("selection", () => {
  it("adds and removes, keeping the selection order", () => {
    let s = setSelected(emptySelection, [file("/a/2"), file("/a/1")], true);
    s = setSelected(s, [file("/a/2")], true);
    expect([...s.keys()]).toEqual(["/a/2", "/a/1"]);
    s = removePaths(s, ["/a/2"]);
    expect([...s.keys()]).toEqual(["/a/1"]);
    expect(setSelected(s, [file("/a/1")], false).size).toBe(0);
  });

  it("maps a table's ticked paths back, keeping other folders' entries", () => {
    const s = fromEntries([dir("/srv/photos")]);
    const listed = [file("/srv/docs/a"), file("/srv/docs/b")];
    const next = withPaths(s, ["/srv/photos", "/srv/docs/b", "/nowhere"], listed);
    expect([...next.keys()]).toEqual(["/srv/photos", "/srv/docs/b"]);
    expect(next.get("/srv/photos")?.type).toBe(ProbeEntryType.DIR);
  });

  it("lets a selected folder cover what is inside it", () => {
    const s = fromEntries([
      file("/srv/photos/2026/a.jpg"),
      dir("/srv/photos"),
      dir("/srv/photos/2026"),
      file("/srv/docs/notes.md"),
    ]);
    expect(coveringFolder(s, "/srv/photos/2026/a.jpg", "snapshot")).toBe("/srv/photos");
    expect(uncovered(s, "snapshot").map((i) => i.path)).toEqual([
      "/srv/photos",
      "/srv/docs/notes.md",
    ]);
    expect(pathsText(s, "snapshot")).toBe("/srv/photos\n/srv/docs/notes.md");
  });

  it("groups the selection by folder, covered items marked", () => {
    const s = fromEntries([file("/srv/photos/2026/a.jpg"), dir("/srv/photos/2026"), file("/b")]);
    expect(groupByFolder(s, "snapshot")).toEqual([
      {
        folder: "/srv/photos/2026",
        items: [
          expect.objectContaining({
            path: "/srv/photos/2026/a.jpg",
            coveredBy: "/srv/photos/2026",
          }),
        ],
      },
      { folder: "/srv/photos", items: [expect.objectContaining({ path: "/srv/photos/2026" })] },
      { folder: "/", items: [expect.objectContaining({ path: "/b" })] },
    ]);
  });
});

describe("commonParent", () => {
  it.each([
    [["/srv/photos/a.jpg"], "snapshot", "/srv/photos"],
    [["/srv/photos/a", "/srv/photos/b/c"], "snapshot", "/srv/photos"],
    [["/srv/a", "/mnt/b"], "snapshot", "/"],
    [["/srv"], "snapshot", "/"],
    [["a/b", "a/c/d"], "remote", "a"],
    [["a", "b"], "remote", ""],
    [["/srv/a", "/srv/b"], "worker", "/srv"],
    [["/srv", "/mnt"], "worker", "/"],
    [["C:\\Users\\ada\\x", "c:\\Users\\ada\\y\\z"], "worker", "C:\\Users\\ada"],
    [["C:\\x", "D:\\y"], "worker", undefined],
    [[], "remote", undefined],
  ] as const)("%j (%s) is %j", (paths, kind, want) => {
    expect(commonParent(paths, kind)).toBe(want);
  });
});

describe("rangeBetween", () => {
  const order = ["d", "c", "b", "a"]; // after a sort and a filter
  it("follows the shown order both ways", () => {
    expect(rangeBetween(order, "c", "a")).toEqual(["c", "b", "a"]);
    expect(rangeBetween(order, "a", "c")).toEqual(["c", "b", "a"]);
    expect(rangeBetween(order, "x", "b")).toEqual(["b"]);
    expect(rangeBetween(order, "a", "x")).toEqual([]);
  });
});

describe("knownSize", () => {
  it("adds files and measured folders, and says at least for the rest", () => {
    const s = fromEntries([file("/a/x", 100), dir("/a/d"), file("/a/d/y", 7), dir("/a/e")]);
    expect(knownSize(s, "snapshot")).toEqual({ bytes: 100n, atLeast: true, files: 1, folders: 2 });
    const measured: Record<string, bigint> = { "/a/d": 50n, "/a/e": 5n };
    expect(knownSize(s, "snapshot", (e) => measured[e.path])).toEqual({
      bytes: 155n,
      atLeast: false,
      files: 1,
      folders: 2,
    });
  });
});
