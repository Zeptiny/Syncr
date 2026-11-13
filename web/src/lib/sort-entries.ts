// Sorting of listed folder entries: folders always come first, whatever
// the column. The choice is remembered per browser.
import { ProbeEntryType, type ProbeEntry } from "@/gen/syncr/v1/link_pb";
import { tsDate } from "@/lib/format";

/** SortKey is a column a file list can be sorted by. */
export type SortKey = "name" | "size" | "modified";

/** FileSort is a file list's sort: a column and a direction. */
export interface FileSort {
  key: SortKey;
  desc: boolean;
}

/** The default sort: by name, A to Z. */
export const defaultSort: FileSort = { key: "name", desc: false };

/** SORT_STORAGE_KEY is where the last sort is remembered. */
export const SORT_STORAGE_KEY = "syncr-file-sort";

const keys: readonly SortKey[] = ["name", "size", "modified"];

const isDir = (e: ProbeEntry) => e.type === ProbeEntryType.DIR;

const byName = (a: ProbeEntry, b: ProbeEntry) =>
  a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: "base" });

/**
 * sortEntries returns the entries sorted: folders first, then by the
 * chosen column, ties broken by name (A to Z). folderSize gives a folder's
 * measured size, so folders sort by size once measured; unmeasured ones
 * sort as the smallest.
 */
export function sortEntries(
  entries: readonly ProbeEntry[],
  sort: FileSort = defaultSort,
  folderSize?: (e: ProbeEntry) => bigint | undefined,
): ProbeEntry[] {
  const size = (e: ProbeEntry): bigint => (isDir(e) ? (folderSize?.(e) ?? -1n) : e.size);
  const time = (e: ProbeEntry): number => tsDate(e.modTime)?.getTime() ?? -Infinity;
  const column = (a: ProbeEntry, b: ProbeEntry): number => {
    switch (sort.key) {
      case "size": {
        const d = size(a) - size(b);
        return d === 0n ? 0 : d < 0n ? -1 : 1;
      }
      case "modified": {
        const ta = time(a);
        const tb = time(b);
        return ta === tb ? 0 : ta < tb ? -1 : 1;
      }
      default:
        return byName(a, b);
    }
  };
  return [...entries].sort((a, b) => {
    const dirs = Number(isDir(b)) - Number(isDir(a));
    if (dirs) return dirs;
    const c = column(a, b);
    if (c) return sort.desc ? -c : c;
    return byName(a, b);
  });
}

/** parseSort reads a remembered sort; anything unexpected is the default. */
export function parseSort(raw: string | null | undefined): FileSort {
  if (!raw) return defaultSort;
  try {
    const v: unknown = JSON.parse(raw);
    if (typeof v !== "object" || v === null) return defaultSort;
    const { key, desc } = v as Record<string, unknown>;
    const k = keys.find((x) => x === key);
    return k ? { key: k, desc: desc === true } : defaultSort;
  } catch {
    return defaultSort;
  }
}

/** loadSort returns the remembered sort, or the default when storage is unavailable. */
export function loadSort(): FileSort {
  try {
    return parseSort(window.localStorage.getItem(SORT_STORAGE_KEY));
  } catch {
    return defaultSort;
  }
}

/** saveSort remembers a sort; it does nothing when storage is unavailable. */
export function saveSort(sort: FileSort): void {
  try {
    window.localStorage.setItem(SORT_STORAGE_KEY, JSON.stringify(sort));
  } catch {
    // Storage is a convenience: a private window simply forgets.
  }
}

/** nextSort is the sort after a column header is clicked: the same column flips. */
export function nextSort(current: FileSort, key: SortKey): FileSort {
  if (current.key === key) return { key, desc: !current.desc };
  // Sizes and times read best largest or newest first.
  return { key, desc: key !== "name" };
}
