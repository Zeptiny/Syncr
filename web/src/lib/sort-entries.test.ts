import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ProbeEntrySchema, ProbeEntryType } from "@/gen/syncr/v1/link_pb";

import {
  defaultSort,
  loadSort,
  nextSort,
  parseSort,
  saveSort,
  SORT_STORAGE_KEY,
  sortEntries,
} from "./sort-entries";

const dir = (name: string) =>
  create(ProbeEntrySchema, { name, path: `/${name}`, type: ProbeEntryType.DIR });
const file = (name: string, size: number, ms: number) =>
  create(ProbeEntrySchema, {
    name,
    path: `/${name}`,
    type: ProbeEntryType.FILE,
    size: BigInt(size),
    modTime: timestampFromMs(ms),
  });

const entries = [
  file("b.txt", 30, 3000),
  dir("photos"),
  file("a10.txt", 10, 1000),
  dir("Archive"),
  file("a2.txt", 20, 2000),
];
const names = (list: { name: string }[]) => list.map((e) => e.name);

describe("sortEntries", () => {
  it("puts folders first and sorts names numerically, ignoring case", () => {
    expect(names(sortEntries(entries))).toEqual([
      "Archive",
      "photos",
      "a2.txt",
      "a10.txt",
      "b.txt",
    ]);
    expect(names(sortEntries(entries, { key: "name", desc: true }))).toEqual([
      "photos",
      "Archive",
      "b.txt",
      "a10.txt",
      "a2.txt",
    ]);
  });

  it("sorts by size and by time, folders still first", () => {
    expect(names(sortEntries(entries, { key: "size", desc: true }))).toEqual([
      "Archive",
      "photos",
      "b.txt",
      "a2.txt",
      "a10.txt",
    ]);
    expect(names(sortEntries(entries, { key: "modified", desc: false }))).toEqual([
      "Archive",
      "photos",
      "a10.txt",
      "a2.txt",
      "b.txt",
    ]);
  });

  it("sorts measured folders by size, unmeasured ones last when largest first", () => {
    const sizes: Record<string, bigint> = { "/photos": 5n };
    const got = sortEntries(entries, { key: "size", desc: true }, (e) => sizes[e.path]);
    expect(names(got).slice(0, 2)).toEqual(["photos", "Archive"]);
  });
});

describe("remembered sort", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    window.localStorage.clear();
  });

  it("parses only what it wrote", () => {
    expect(parseSort(null)).toEqual(defaultSort);
    expect(parseSort("{bad")).toEqual(defaultSort);
    expect(parseSort('{"key":"owner"}')).toEqual(defaultSort);
    expect(parseSort('{"key":"size","desc":true}')).toEqual({ key: "size", desc: true });
  });

  it("round-trips through localStorage", () => {
    saveSort({ key: "modified", desc: true });
    expect(window.localStorage.getItem(SORT_STORAGE_KEY)).toBe('{"key":"modified","desc":true}');
    expect(loadSort()).toEqual({ key: "modified", desc: true });
  });

  it("works without storage", () => {
    vi.spyOn(Storage.prototype, "getItem").mockImplementation(() => {
      throw new Error("denied");
    });
    vi.spyOn(Storage.prototype, "setItem").mockImplementation(() => {
      throw new Error("denied");
    });
    expect(loadSort()).toEqual(defaultSort);
    expect(() => saveSort({ key: "size", desc: false })).not.toThrow();
  });

  it("flips the same column and starts sizes and times largest first", () => {
    expect(nextSort(defaultSort, "name")).toEqual({ key: "name", desc: true });
    expect(nextSort(defaultSort, "size")).toEqual({ key: "size", desc: true });
    expect(nextSort({ key: "size", desc: true }, "size")).toEqual({ key: "size", desc: false });
  });
});
