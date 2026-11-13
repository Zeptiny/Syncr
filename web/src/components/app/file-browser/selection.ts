// The selection model of the file browser: pure functions over a map of
// selected paths to their listed entries. A selection survives moving
// between folders of one source; a selected folder covers what is inside
// it, so actions act on the folder alone.
import { ProbeEntryType, type ProbeEntry } from "@/gen/syncr/v1/link_pb";
import { parentPath, pathCrumbs, rootPath, type PathKind } from "@/lib/paths";

/** Selection maps each selected path to its entry, in the order it was selected. */
export type Selection = ReadonlyMap<string, ProbeEntry>;

/** SelectedItem is one selected path with its entry. */
export interface SelectedItem {
  path: string;
  entry: ProbeEntry;
  /** coveredBy is the selected folder that holds this item, if any. */
  coveredBy?: string;
}

/** emptySelection is a selection with nothing in it. */
export const emptySelection: Selection = new Map();

const isDir = (e: ProbeEntry) => e.type === ProbeEntryType.DIR;
const isWindowsPath = (p: string) => /^[A-Za-z]:/.test(p) || p.startsWith("\\\\");

/** fromEntries builds a selection from entries, keyed by their paths. */
export function fromEntries(entries: Iterable<ProbeEntry>): Selection {
  const m = new Map<string, ProbeEntry>();
  for (const e of entries) m.set(e.path, e);
  return m;
}

/** setSelected ticks (on) or unticks entries, keeping the order of the rest. */
export function setSelected(sel: Selection, entries: Iterable<ProbeEntry>, on: boolean): Selection {
  const m = new Map(sel);
  for (const e of entries) {
    if (on) {
      if (!m.has(e.path)) m.set(e.path, e);
    } else m.delete(e.path);
  }
  return m;
}

/** removePaths drops paths from a selection. */
export function removePaths(sel: Selection, paths: Iterable<string>): Selection {
  const m = new Map(sel);
  for (const p of paths) m.delete(p);
  return m;
}

/**
 * withPaths turns a list of ticked paths (what a FileTable reports) back
 * into a selection: paths already selected keep their entries, new ones
 * take theirs from the listed entries. Paths found in neither are dropped.
 */
export function withPaths(
  sel: Selection,
  paths: readonly string[],
  listed: readonly ProbeEntry[],
): Selection {
  const byPath = new Map(listed.map((e) => [e.path, e]));
  const m = new Map<string, ProbeEntry>();
  for (const p of paths) {
    const e = sel.get(p) ?? byPath.get(p);
    if (e) m.set(p, e);
  }
  return m;
}

/**
 * isInside reports whether path lies strictly inside folder. Windows
 * worker paths compare without case and with either separator.
 */
export function isInside(path: string, folder: string, kind: PathKind): boolean {
  if (path === folder) return false;
  if (kind === "remote") return folder === "" ? path !== "" : path.startsWith(`${folder}/`);
  if (kind === "snapshot") return folder === "/" ? path !== "/" : path.startsWith(`${folder}/`);
  if (folder === "") return path !== "";
  if (isWindowsPath(folder) || isWindowsPath(path)) {
    const norm = (p: string) => p.replaceAll("/", "\\").toLowerCase();
    const f = norm(folder);
    const p = norm(path);
    if (p === f || p === f.replace(/\\+$/, "")) return false;
    return p.startsWith(f.endsWith("\\") ? f : `${f}\\`);
  }
  if (folder === "/") return path.startsWith("/") && path !== "/";
  return path.startsWith(`${folder}/`);
}

/**
 * coveringFolder returns the outermost selected folder that holds path,
 * or undefined when none does.
 */
export function coveringFolder(sel: Selection, path: string, kind: PathKind): string | undefined {
  let best: string | undefined;
  for (const [p, e] of sel) {
    if (!isDir(e) || !isInside(path, p, kind)) continue;
    if (best === undefined || isInside(best, p, kind)) best = p;
  }
  return best;
}

/** items lists the selection in order, each with the folder that covers it. */
export function items(sel: Selection, kind: PathKind): SelectedItem[] {
  return [...sel].map(([path, entry]) => {
    const coveredBy = coveringFolder(sel, path, kind);
    return coveredBy === undefined ? { path, entry } : { path, entry, coveredBy };
  });
}

/** uncovered lists the items actions act on: those no selected folder holds. */
export function uncovered(sel: Selection, kind: PathKind): SelectedItem[] {
  return items(sel, kind).filter((i) => i.coveredBy === undefined);
}

/**
 * commonParent returns the deepest folder that holds every path: for one
 * path its parent. It returns undefined for no paths, and when the paths
 * share no folder at all (two Windows drives).
 */
export function commonParent(paths: readonly string[], kind: PathKind): string | undefined {
  if (paths.length === 0) return undefined;
  const chains = paths.map((p) => pathCrumbs(parentPath(p, kind), kind));
  let n = 0;
  for (; ; n++) {
    const step = chains[0][n];
    if (!step) break;
    if (!chains.every((c) => c[n] && same(c[n].path, step.path, kind))) break;
  }
  if (n > 0) return chains[0][n - 1].path;
  if (kind !== "worker") return rootPath(kind);
  // A worker's paths: the POSIX root holds every absolute POSIX path; two
  // Windows drives (or the list of allowed folders) have nothing above.
  return paths.every((p) => p.startsWith("/")) ? "/" : undefined;
}

function same(a: string, b: string, kind: PathKind): boolean {
  if (kind === "worker" && (isWindowsPath(a) || isWindowsPath(b)))
    return a.toLowerCase() === b.toLowerCase();
  return a === b;
}

/**
 * rangeBetween returns the paths from anchor to target inclusive, in the
 * order rows are shown (the current sort and filter). Without the anchor
 * in the list, the range is the target alone.
 */
export function rangeBetween(ordered: readonly string[], anchor: string, target: string): string[] {
  const b = ordered.indexOf(target);
  if (b < 0) return [];
  const a = ordered.indexOf(anchor);
  if (a < 0) return [target];
  const [lo, hi] = a <= b ? [a, b] : [b, a];
  return ordered.slice(lo, hi + 1);
}

/** KnownSize is what a selection is known to weigh. */
export interface KnownSize {
  bytes: bigint;
  /** atLeast says a selected folder is not measured, so bytes is a lower bound. */
  atLeast: boolean;
  /** files and folders count the uncovered items. */
  files: number;
  folders: number;
}

/**
 * knownSize adds up the uncovered items: files by their listed size,
 * folders by their measured size (folderSize) where known. Any unmeasured
 * folder, or one whose measurement is a lower bound (folderAtLeast), makes
 * the total a lower bound ("at least").
 */
export function knownSize(
  sel: Selection,
  kind: PathKind,
  folderSize?: (entry: ProbeEntry) => bigint | undefined,
  folderAtLeast?: (entry: ProbeEntry) => boolean,
): KnownSize {
  const out: KnownSize = { bytes: 0n, atLeast: false, files: 0, folders: 0 };
  for (const { entry } of uncovered(sel, kind)) {
    if (isDir(entry)) {
      out.folders++;
      const s = folderSize?.(entry);
      if (s === undefined || folderAtLeast?.(entry)) out.atLeast = true;
      if (s !== undefined) out.bytes += s;
    } else {
      out.files++;
      out.bytes += entry.size;
    }
  }
  return out;
}

/** SelectionGroup is the selected items of one folder. */
export interface SelectionGroup {
  folder: string;
  items: SelectedItem[];
}

/** groupByFolder groups the selection by parent folder, in selection order. */
export function groupByFolder(sel: Selection, kind: PathKind): SelectionGroup[] {
  const groups = new Map<string, SelectionGroup>();
  for (const item of items(sel, kind)) {
    const folder = parentPath(item.path, kind);
    let g = groups.get(folder);
    if (!g) {
      g = { folder, items: [] };
      groups.set(folder, g);
    }
    g.items.push(item);
  }
  return [...groups.values()];
}

/** pathsText writes the uncovered paths one per line, for the clipboard. */
export function pathsText(sel: Selection, kind: PathKind): string {
  return uncovered(sel, kind)
    .map((i) => i.path)
    .join("\n");
}
