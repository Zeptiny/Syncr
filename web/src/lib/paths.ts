// Path arithmetic of the path browser. Three kinds of places are browsed:
// a worker's folders (native paths; "" lists the allowed folders), a remote
// (paths relative to its root, "" being the root) and a snapshot tree
// (absolute, forward slashes, "/" being the root).

/** PathKind says how a browsed path is written. */
export type PathKind = "worker" | "remote" | "snapshot";

/** Crumb is one step of a path, with the path that opens it. */
export interface Crumb {
  label: string;
  path: string;
}

const isWindowsPath = (p: string) => /^[A-Za-z]:/.test(p) || p.startsWith("\\\\");

/** rootPath is the top of a kind of place. */
export function rootPath(kind: PathKind): string {
  return kind === "snapshot" ? "/" : "";
}

/** normalizePath trims a typed path into the form listings use. */
export function normalizePath(path: string, kind: PathKind): string {
  const p = path.trim();
  if (kind === "remote") return p.replace(/^\/+|\/+$/g, "");
  if (kind === "snapshot") return "/" + p.split("/").filter(Boolean).join("/");
  if (isWindowsPath(p)) {
    if (/^[A-Za-z]:\\?$/.test(p)) return `${p.slice(0, 2)}\\`;
    return p.replace(/\\+$/, "");
  }
  return p.length > 1 ? p.replace(/\/+$/, "") : p;
}

/** joinPath appends a name to a browsed path. */
export function joinPath(path: string, name: string, kind: PathKind): string {
  if (kind === "remote") return path ? `${path}/${name}` : name;
  if (kind === "worker" && isWindowsPath(path))
    return path.endsWith("\\") ? path + name : `${path}\\${name}`;
  return path.endsWith("/") ? path + name : `${path}/${name}`;
}

/** pathCrumbs splits a path into steps, the root not included. */
export function pathCrumbs(path: string, kind: PathKind): Crumb[] {
  const crumbs: Crumb[] = [];
  if (kind === "worker" && isWindowsPath(path)) {
    const parts = path.split("\\").filter(Boolean);
    let at = "";
    parts.forEach((part, i) => {
      at = i === 0 ? `${part}\\` : joinPath(at, part, kind);
      crumbs.push({ label: part, path: at });
    });
    return crumbs;
  }
  const parts = path.split("/").filter(Boolean);
  let at = kind === "remote" ? "" : "/";
  for (const part of parts) {
    at = joinPath(at, part, kind);
    crumbs.push({ label: part, path: at });
  }
  return crumbs;
}

/**
 * parentPath is the folder above path, or the root when there is none. For
 * a worker, roots are its allowed folders: the step above one of them is
 * the list of allowed folders ("").
 */
export function parentPath(path: string, kind: PathKind, roots: string[] = []): string {
  const root = rootPath(kind);
  if (path === root) return root;
  if (kind === "worker") {
    const same = (a: string, b: string) => normalizePath(a, kind) === normalizePath(b, kind);
    if (roots.some((r) => same(r, path))) return root;
  }
  const crumbs = pathCrumbs(path, kind);
  if (crumbs.length >= 2) return crumbs[crumbs.length - 2].path;
  if (kind === "worker" && crumbs.length === 1 && !isWindowsPath(path) && path !== "/") return "/";
  return root;
}
