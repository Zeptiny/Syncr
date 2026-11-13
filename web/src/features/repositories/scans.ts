// Search, file versions and compare: the pure part. Matches of restic find
// grouped by path and by version, the exact-path pattern of "Show
// versions", and restic diff's changes by kind.
import { timestampMs, type Timestamp } from "@bufbuild/protobuf/wkt";

import {
  ProbeEntryType,
  ResticDiffModifier,
  type ProbeEntry,
  type ResticDiffChange,
  type ResticFindMatch,
  type ResticSnapshot,
} from "@/gen/syncr/v1/link_pb";
import { parentPath } from "@/lib/paths";

/**
 * escapeGlob makes a name match only itself in a restic find pattern:
 * "*", "?", "[" and "\" become character classes ("[*]"), which also work
 * on Windows, where "\" is a separator rather than an escape.
 */
export function escapeGlob(name: string): string {
  return name.replace(/[*?[\\]/g, (c) => (c === "\\" ? "[\\\\]" : `[${c}]`));
}

/**
 * exactPathPattern is the find pattern of one snapshot tree path: anchored
 * at the root ("/" first) and escaped, so it matches that path only, and,
 * when it is a folder, everything below it (restic returns the subtree).
 */
export function exactPathPattern(path: string): string {
  return "/" + path.split("/").filter(Boolean).map(escapeGlob).join("/");
}

/** onlyPath keeps the matches of exactly path (a folder's match brings its subtree). */
export function onlyPath(matches: readonly ResticFindMatch[], path: string): ResticFindMatch[] {
  return matches.filter((m) => m.entry?.path === path);
}

/** FileVersion is one distinct version of a path: the snapshots that hold it alike. */
export interface FileVersion {
  key: string;
  /** entry is the version as its newest snapshot holds it. */
  entry: ProbeEntry;
  /** snapshotIds hold it, newest first. */
  snapshotIds: string[];
}

/** FoundPath is one path a search found, with its versions. */
export interface FoundPath {
  path: string;
  /** entry is the path in the newest snapshot that has it. */
  entry: ProbeEntry;
  /** snapshotId is that newest snapshot. */
  snapshotId: string;
  /** snapshotIds have the path, newest first. */
  snapshotIds: string[];
  /** versions are newest first. */
  versions: FileVersion[];
}

const ms = (t: Timestamp | undefined) => (t ? timestampMs(t) : 0);

/**
 * versionKey tells versions apart: a file by size and modification time, a
 * link by its time; a folder is one version (its time changes with
 * anything inside it).
 */
export function versionKey(e: ProbeEntry): string {
  switch (e.type) {
    case ProbeEntryType.DIR:
      return "dir";
    case ProbeEntryType.FILE:
      return `file:${e.size}:${ms(e.modTime)}`;
    default:
      return `${e.type}:${ms(e.modTime)}`;
  }
}

/**
 * groupMatches groups a search's matches by path, in the order they were
 * first found, and each path's matches by version. snapshots (the
 * repository's list) order them newest first; snapshots it lacks keep the
 * order of the matches (restic searches newest first).
 */
export function groupMatches(
  matches: readonly ResticFindMatch[],
  snapshots: readonly ResticSnapshot[] = [],
): FoundPath[] {
  const time = new Map(snapshots.map((s) => [s.id, ms(s.time)]));
  const seen = new Map<string, number>();
  for (const m of matches) if (!seen.has(m.snapshotId)) seen.set(m.snapshotId, seen.size);
  // Newest first: by the snapshot's time when known, else by search order.
  const order = (a: string, b: string) => {
    const ta = time.get(a);
    const tb = time.get(b);
    if (ta !== undefined && tb !== undefined && ta !== tb) return tb - ta;
    return (seen.get(a) ?? 0) - (seen.get(b) ?? 0);
  };
  const groups = new Map<string, ResticFindMatch[]>();
  for (const m of matches) {
    if (!m.entry) continue;
    const list = groups.get(m.entry.path);
    if (list) list.push(m);
    else groups.set(m.entry.path, [m]);
  }
  return [...groups].map(([path, list]) => {
    const sorted = [...list].sort((a, b) => order(a.snapshotId, b.snapshotId));
    const versions = new Map<string, FileVersion>();
    for (const m of sorted) {
      const key = versionKey(m.entry!);
      const v = versions.get(key);
      if (v) {
        if (!v.snapshotIds.includes(m.snapshotId)) v.snapshotIds.push(m.snapshotId);
      } else versions.set(key, { key, entry: m.entry!, snapshotIds: [m.snapshotId] });
    }
    return {
      path,
      entry: sorted[0].entry!,
      snapshotId: sorted[0].snapshotId,
      snapshotIds: [...new Set(sorted.map((m) => m.snapshotId))],
      versions: [...versions.values()],
    };
  });
}

/** folderOf is the snapshot folder that shows a path: its parent. */
export function folderOf(path: string): string {
  return parentPath(path.replace(/\/+$/, "") || "/", "snapshot");
}

// ---------------------------------------------------------------------------
// Compare
// ---------------------------------------------------------------------------

/** ChangeKind is a kind of change restic diff reports. */
export type ChangeKind = "added" | "removed" | "changed" | "type" | "metadata" | "other";

/** changeKinds are the kinds in display order, with their labels. */
export const changeKinds: readonly { kind: ChangeKind; label: string }[] = [
  { kind: "added", label: "Added" },
  { kind: "removed", label: "Removed" },
  { kind: "changed", label: "Changed" },
  { kind: "type", label: "Type changed" },
  { kind: "metadata", label: "Metadata only" },
  { kind: "other", label: "Other" },
];

/** changeKind is the kind of one change. */
export function changeKind(c: ResticDiffChange): ChangeKind {
  switch (c.modifier) {
    case ResticDiffModifier.ADDED:
      return "added";
    case ResticDiffModifier.REMOVED:
      return "removed";
    case ResticDiffModifier.CHANGED:
      return "changed";
    case ResticDiffModifier.TYPE:
      return "type";
    case ResticDiffModifier.METADATA:
      return "metadata";
    default:
      return "other";
  }
}

/** kindLabel names a kind of change. */
export function kindLabel(k: ChangeKind): string {
  return changeKinds.find((x) => x.kind === k)?.label ?? k;
}

/**
 * kindCounts counts the changes of each kind that occurs, in display
 * order: the filters offer only these (restic without --metadata never
 * reports "Metadata only", for one).
 */
export function kindCounts(
  changes: readonly ResticDiffChange[],
): { kind: ChangeKind; label: string; count: number }[] {
  const n = new Map<ChangeKind, number>();
  for (const c of changes) n.set(changeKind(c), (n.get(changeKind(c)) ?? 0) + 1);
  return changeKinds.filter((k) => n.has(k.kind)).map((k) => ({ ...k, count: n.get(k.kind)! }));
}

/** filterChanges keeps the changes of the chosen kinds (all when none) whose path holds text. */
export function filterChanges(
  changes: readonly ResticDiffChange[],
  kinds: ReadonlySet<ChangeKind>,
  text: string,
): ResticDiffChange[] {
  const needle = text.trim().toLowerCase();
  return changes.filter(
    (c) =>
      (kinds.size === 0 || kinds.has(changeKind(c))) &&
      (!needle || c.path.toLowerCase().includes(needle)),
  );
}

/**
 * changePlace is where a change can be seen: the folder that shows it (a
 * changed folder opens itself), in the snapshot that has it (the older one
 * for a removal, else the newer).
 */
export function changePlace(
  c: ResticDiffChange,
  from: string,
  to: string,
): { snapshotId: string; path: string } {
  const dir = c.path.endsWith("/") && c.path !== "/";
  const path = dir ? c.path.replace(/\/+$/, "") : folderOf(c.path);
  return { snapshotId: changeKind(c) === "removed" ? from : to, path };
}

/** olderFirst orders two snapshots for a comparison: the older is "from". */
export function olderFirst(
  a: ResticSnapshot,
  b: ResticSnapshot,
): { from: ResticSnapshot; to: ResticSnapshot } {
  return ms(a.time) <= ms(b.time) ? { from: a, to: b } : { from: b, to: a };
}
