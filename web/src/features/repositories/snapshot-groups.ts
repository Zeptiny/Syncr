// Pure logic of snapshot lists: filtering, restic's grouping by host and
// paths, the previous snapshot of a group and the snapshot switcher's
// order. No React here, so it is easy to test.
import type { ResticSnapshot } from "@/gen/syncr/v1/link_pb";
import { tsDate } from "@/lib/format";

import { remoteOrigin, type RemoteNames } from "./snapshot-origin";

/** SnapshotFilter narrows a snapshot list; empty fields match everything. */
export interface SnapshotFilter {
  /**
   * text matches part of the ID, the host, a tag or a path, or the remote
   * a snapshot is of ("From Nextcloud"), ignoring case.
   */
  text: string;
  host: string;
  tag: string;
}

/** noFilter matches every snapshot. */
export const noFilter: SnapshotFilter = { text: "", host: "", tag: "" };

/** isFiltered reports whether a filter narrows anything. */
export function isFiltered(f: SnapshotFilter): boolean {
  return !!(f.text.trim() || f.host || f.tag);
}

const time = (s: ResticSnapshot) => tsDate(s.time)?.getTime() ?? 0;

/** newestFirst sorts snapshots by time, newest first (ties by ID). */
export function newestFirst(list: readonly ResticSnapshot[]): ResticSnapshot[] {
  return [...list].sort((a, b) => time(b) - time(a) || a.id.localeCompare(b.id));
}

/**
 * filterSnapshots keeps the snapshots that match every field of f, in
 * order. names (the user's remotes) lets the text match a remote's name.
 */
export function filterSnapshots(
  list: readonly ResticSnapshot[],
  f: SnapshotFilter,
  names: RemoteNames = new Map(),
): ResticSnapshot[] {
  const needle = f.text.trim().toLowerCase();
  return list.filter(
    (s) =>
      (!f.host || s.host === f.host) &&
      (!f.tag || s.tags.includes(f.tag)) &&
      (!needle ||
        s.id.toLowerCase().includes(needle) ||
        s.host.toLowerCase().includes(needle) ||
        s.tags.some((t) => t.toLowerCase().includes(needle)) ||
        s.paths.some((p) => p.toLowerCase().includes(needle)) ||
        !!remoteOrigin(s, names)?.toLowerCase().includes(needle)),
  );
}

/** hostsOf lists the hosts of a snapshot list, sorted. */
export function hostsOf(list: readonly ResticSnapshot[]): string[] {
  return [...new Set(list.map((s) => s.host).filter(Boolean))].sort();
}

/** tagsOf lists the tags of a snapshot list, sorted. */
export function tagsOf(list: readonly ResticSnapshot[]): string[] {
  return [...new Set(list.flatMap((s) => s.tags))].sort();
}

/** groupKey is restic's grouping (--group-by host,paths): the host and the sorted paths. */
export function groupKey(s: ResticSnapshot): string {
  return JSON.stringify([s.host, [...s.paths].sort()]);
}

/** SnapshotGroup is the snapshots of one host and set of paths, newest first. */
export interface SnapshotGroup {
  key: string;
  host: string;
  paths: string[];
  /** first is the group's newest snapshot, e.g. to name the remote of a group of a remote. */
  first: ResticSnapshot;
  snapshots: ResticSnapshot[];
}

/** groupSnapshots groups by host and paths; the group with the newest snapshot comes first. */
export function groupSnapshots(list: readonly ResticSnapshot[]): SnapshotGroup[] {
  const groups = new Map<string, SnapshotGroup>();
  for (const s of newestFirst(list)) {
    const key = groupKey(s);
    let g = groups.get(key);
    if (!g) {
      g = { key, host: s.host, paths: [...s.paths].sort(), first: s, snapshots: [] };
      groups.set(key, g);
    }
    g.snapshots.push(s);
  }
  return [...groups.values()];
}

/**
 * previousInGroup returns the snapshot before s in its group (same host
 * and paths): what "Compare with previous" compares s with.
 */
export function previousInGroup(
  list: readonly ResticSnapshot[],
  s: ResticSnapshot,
): ResticSnapshot | undefined {
  const key = groupKey(s);
  const same = newestFirst(list).filter((x) => groupKey(x) === key);
  const i = same.findIndex((x) => x.id === s.id);
  return i < 0 ? undefined : same[i + 1];
}

/** findSnapshot finds a snapshot by full ID or by an ID prefix. */
export function findSnapshot(
  list: readonly ResticSnapshot[],
  id: string,
): ResticSnapshot | undefined {
  return list.find((s) => s.id === id) ?? (id ? list.find((s) => s.id.startsWith(id)) : undefined);
}

/** SwitcherChoices are what the snapshot switcher offers from one snapshot. */
export interface SwitcherChoices {
  /** same are the snapshots of the current one's host and paths, newest first. */
  same: ResticSnapshot[];
  /** others are the rest, newest first. */
  others: ResticSnapshot[];
  /** older and newer are the neighbours in the same group. */
  older?: ResticSnapshot;
  newer?: ResticSnapshot;
}

/**
 * switcherChoices orders a repository's snapshots for the switcher: the
 * current snapshot's host and paths first. Older and Newer step within
 * that group, which is how one backup set moves through time.
 */
export function switcherChoices(
  list: readonly ResticSnapshot[],
  current: ResticSnapshot | undefined,
): SwitcherChoices {
  const all = newestFirst(list);
  if (!current) return { same: [], others: all };
  const key = groupKey(current);
  const same = all.filter((s) => groupKey(s) === key);
  const others = all.filter((s) => groupKey(s) !== key);
  const i = same.findIndex((s) => s.id === current.id);
  return {
    same,
    others,
    newer: i > 0 ? same[i - 1] : undefined,
    older: i >= 0 ? same[i + 1] : undefined,
  };
}
