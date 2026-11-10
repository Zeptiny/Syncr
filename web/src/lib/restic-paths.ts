// Path forms of restic snapshots and worker folders. A snapshot's tree
// always uses forward slashes; a snapshot made on Windows starts with the
// drive letter (/C/Users/ada). The same rules live in the controller
// (jobkind.TreeToLocal).

/** isWindowsOS reports whether a worker OS name is Windows. */
export function isWindowsOS(os: string | undefined): boolean {
  return (os ?? "").toLowerCase() === "windows";
}

/**
 * treeToLocal turns a snapshot tree path into the path it had on a machine
 * running os, or returns undefined when there is no safe answer (the tree
 * root on Windows, a first segment that is not a drive letter, a segment
 * holding a backslash or a colon). The user then types the folder.
 */
export function treeToLocal(tree: string, os: string | undefined): string | undefined {
  if (!tree.startsWith("/")) return undefined;
  if (!isWindowsOS(os)) return tree;
  const segments = tree.split("/").filter(Boolean);
  if (segments.length === 0) return undefined;
  if (!/^[A-Za-z]$/.test(segments[0])) return undefined;
  if (segments.some((s) => s.includes("\\") || s.includes(":"))) return undefined;
  const rest = segments.slice(1).join("\\");
  return `${segments[0].toUpperCase()}:\\${rest}`;
}

/** isWindowsTree reports whether a snapshot's paths look like Windows ones. */
export function isWindowsTree(paths: string[]): boolean {
  return paths.some((p) => /^[A-Za-z]:[\\/]/.test(p) || p.startsWith("\\\\"));
}

function normalize(path: string, windows: boolean): string {
  let p = windows ? path.replaceAll("/", "\\").toLowerCase() : path;
  const sep = windows ? "\\" : "/";
  while (p.length > 1 && p.endsWith(sep)) p = p.slice(0, -1);
  return p;
}

/**
 * isWithinRoots reports whether path lies in one of a worker's allowed
 * folders. It compares text only (the worker also resolves symlinks), so
 * the UI uses it for a warning and never to block. Windows paths compare
 * without case. A path with ".." segments is never inside.
 */
export function isWithinRoots(path: string, roots: string[], os: string | undefined): boolean {
  const windows = isWindowsOS(os);
  const sep = windows ? "\\" : "/";
  const p = normalize(path.trim(), windows);
  if (!p || p.split(sep).includes("..")) return false;
  return roots.some((root) => {
    const r = normalize(root, windows);
    if (!r) return false;
    if (p === r) return true;
    return p.startsWith(r.endsWith(sep) ? r : r + sep);
  });
}
