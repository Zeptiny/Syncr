// A browser selection as an rclone transfer with a file manager's
// semantics: a selected folder "photos" copied into D arrives as
// D/photos. rclone copies a folder's contents, so the transfer reads the
// selection's common parent P and keeps only the selected items with
// anchored filter rules: "+ /rel" for a file, "+ /rel/**" for a folder,
// then "- **". Names are escaped for rclone's glob syntax. The rules are
// pinned against rclone 1.75.2 by rclone-filters.golden.json.

/** FilterItem is one selected item: its path in the source and whether it is a folder. */
export interface FilterItem {
  path: string;
  dir: boolean;
}

/** SelectionFilters is what a transfer of a selection reads and keeps. */
export interface SelectionFilters {
  /** source is the folder rclone reads: the selection's common parent. */
  source: string;
  /** filters are rclone filter lines in order ("+ /rel", …, "- **"). */
  filters: string[];
}

/** MAX_SELECTION_ITEMS keeps a transfer under the API's 1,000 filter rules (one is "- **"). */
export const MAX_SELECTION_ITEMS = 999;

/** MAX_PATTERN_BYTES is the API's limit on one filter pattern. */
export const MAX_PATTERN_BYTES = 4096;

/** EXCLUDE_REST is the closing rule: everything not included is left out. */
export const EXCLUDE_REST = "- **";

const isWindowsPath = (p: string) => /^[A-Za-z]:/.test(p) || p.startsWith("\\\\");

/**
 * escapeFilterName escapes one path segment for rclone's filter globs: a
 * backslash before each of \ * ? [ ] { }.
 */
export function escapeFilterName(name: string): string {
  return name.replace(/[\\*?[\]{}]/g, "\\$&");
}

/**
 * filterPattern writes the anchored rule pattern of a path relative to the
 * transfer's source (forward slashes): "/rel" for a file, "/rel/**" for a
 * folder and everything in it. Whitespace at the very end of a pattern
 * becomes a character class ("[ ]"), because rclone trims the lines of a
 * filter file and the job editor trims patterns.
 */
export function filterPattern(rel: string, dir: boolean): string {
  const escaped = "/" + rel.split("/").map(escapeFilterName).join("/");
  if (dir) return `${escaped}/**`;
  const m = /\s+$/u.exec(escaped);
  if (!m) return escaped;
  return escaped.slice(0, m.index) + [...m[0]].map((c) => `[${c}]`).join("");
}

/**
 * relativePath returns path relative to parent with forward slashes, or
 * undefined when path is not strictly inside parent. Paths are a worker's
 * (POSIX or Windows, where case and separators don't matter) or a
 * remote's (relative to its root, "" being the root).
 */
export function relativePath(path: string, parent: string): string | undefined {
  const windows = isWindowsPath(parent) || isWindowsPath(path);
  const head = path.slice(0, parent.length);
  if (windows ? head.toLowerCase() !== parent.toLowerCase() : head !== parent) return undefined;
  let rest = path.slice(parent.length);
  const sep = windows ? /^[\\/]+/ : /^\/+/;
  if (parent !== "" && !/[\\/]$/.test(parent) && !sep.test(rest)) return undefined;
  rest = rest.replace(sep, "");
  if (rest === "") return undefined;
  return windows ? rest.split(/[\\/]+/).join("/") : rest;
}

const utf8 = new TextEncoder();

/**
 * selectionFilters builds the transfer of a selection: the source is
 * parent (the items' common parent, see commonParent), and the filters
 * keep exactly the items. items must be the uncovered ones (no item inside
 * another selected folder). It returns an error to show instead when the
 * selection cannot be named by filters.
 */
export function selectionFilters(
  items: readonly FilterItem[],
  parent: string | undefined,
): SelectionFilters | { error: string } {
  if (items.length === 0) return { error: "Select at least one item." };
  if (parent === undefined)
    return { error: "The items are on different drives. Copy each drive's items separately." };
  if (items.length > MAX_SELECTION_ITEMS)
    return {
      error: `Select at most ${MAX_SELECTION_ITEMS} items, or select the folder that holds them.`,
    };
  const filters: string[] = [];
  for (const item of items) {
    const name = item.path.split(/[\\/]/).pop() || item.path;
    if (/[\0\r\n]/.test(item.path))
      return { error: `"${name}" has a line break in its name, so it can't be copied from here.` };
    const rel = relativePath(item.path, parent);
    if (rel === undefined) return { error: `${item.path} is not inside ${parent || "the root"}.` };
    const pattern = filterPattern(rel, item.dir);
    if (utf8.encode(pattern).length > MAX_PATTERN_BYTES)
      return { error: `The path of "${name}" is too long to copy from here.` };
    filters.push(`+ ${pattern}`);
  }
  filters.push(EXCLUDE_REST);
  return { source: parent, filters };
}

/** FilterLine is a filter line split into its action and pattern. */
export interface FilterLine {
  include: boolean;
  pattern: string;
}

/** parseFilterLine splits "+ pattern" or "- pattern". */
export function parseFilterLine(line: string): FilterLine {
  return { include: line.startsWith("+"), pattern: line.slice(2) };
}
