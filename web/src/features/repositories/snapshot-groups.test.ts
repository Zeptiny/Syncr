import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { describe, expect, it } from "vitest";

import { ResticSnapshotSchema } from "@/gen/syncr/v1/link_pb";

import {
  filterSnapshots,
  findSnapshot,
  groupSnapshots,
  hostsOf,
  isFiltered,
  noFilter,
  previousInGroup,
  switcherChoices,
  tagsOf,
} from "./snapshot-groups";

const snap = (id: string, hour: number, host: string, paths: string[], tags: string[] = []) =>
  create(ResticSnapshotSchema, {
    id: id.padEnd(64, "0"),
    time: timestampFromMs(Date.UTC(2026, 10, 1, hour)),
    host,
    paths,
    tags,
  });

// Oldest first on purpose: every function orders by time itself.
const list = [
  snap("a1", 1, "nas-01", ["/srv/photos"], ["photos"]),
  snap("b1", 2, "nas-01", ["/srv/docs", "/srv/backup"], ["docs"]),
  snap("a2", 3, "nas-01", ["/srv/photos"], ["photos"]),
  snap("c1", 4, "laptop", ["/home/ada"]),
  snap("b2", 5, "nas-01", ["/srv/backup", "/srv/docs"], ["docs", "weekly"]),
  snap("a3", 6, "nas-01", ["/srv/photos"], ["photos"]),
];
const ids = (l: { id: string }[]) => l.map((s) => s.id.slice(0, 2));

describe("snapshot lists", () => {
  it("filters by text over ID, host, tag and path, and by host and tag", () => {
    expect(isFiltered(noFilter)).toBe(false);
    expect(ids(filterSnapshots(list, { ...noFilter, text: "LAPTOP" }))).toEqual(["c1"]);
    expect(ids(filterSnapshots(list, { ...noFilter, text: "b2" }))).toEqual(["b2"]);
    expect(ids(filterSnapshots(list, { ...noFilter, text: "/srv/back" }))).toEqual(["b1", "b2"]);
    expect(ids(filterSnapshots(list, { ...noFilter, text: "week" }))).toEqual(["b2"]);
    expect(ids(filterSnapshots(list, { text: "", host: "nas-01", tag: "photos" }))).toEqual([
      "a1",
      "a2",
      "a3",
    ]);
    expect(hostsOf(list)).toEqual(["laptop", "nas-01"]);
    expect(tagsOf(list)).toEqual(["docs", "photos", "weekly"]);
  });

  it("groups by host and paths in any order, newest group first", () => {
    const groups = groupSnapshots(list);
    expect(groups.map((g) => [g.host, g.paths, ids(g.snapshots)])).toEqual([
      ["nas-01", ["/srv/photos"], ["a3", "a2", "a1"]],
      ["nas-01", ["/srv/backup", "/srv/docs"], ["b2", "b1"]],
      ["laptop", ["/home/ada"], ["c1"]],
    ]);
  });

  it("finds the previous snapshot of a group", () => {
    expect(previousInGroup(list, list[5])?.id).toBe(list[2].id);
    expect(previousInGroup(list, list[4])?.id).toBe(list[1].id);
    expect(previousInGroup(list, list[0])).toBeUndefined();
  });

  it("orders the switcher: same host and paths first, older and newer within them", () => {
    const c = switcherChoices(list, list[2]); // a2
    expect(ids(c.same)).toEqual(["a3", "a2", "a1"]);
    expect(ids(c.others)).toEqual(["b2", "c1", "b1"]);
    expect(c.newer?.id).toBe(list[5].id);
    expect(c.older?.id).toBe(list[0].id);
    expect(switcherChoices(list, undefined).same).toEqual([]);
    expect(findSnapshot(list, "c1")?.id).toBe(list[3].id);
    expect(findSnapshot(list, "")).toBeUndefined();
  });
});
