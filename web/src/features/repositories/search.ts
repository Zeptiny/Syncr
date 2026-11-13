// URL state of the repository pages, kept apart from them for code splitting.
// The repository password is never part of any URL.

/** The tabs of a repository's page. */
export const repositoryTabs = ["snapshots", "jobs", "schedules", "settings"] as const;

/** RepositoryTab is one tab of a repository's page. */
export type RepositoryTab = (typeof repositoryTabs)[number];

const str = (v: unknown) => (typeof v === "string" && v ? v : undefined);

/** RepositorySearch is the URL state of a repository's page. */
export interface RepositorySearch {
  tab?: RepositoryTab;
}

/** validateRepositorySearch parses a repository page's search params. */
export function validateRepositorySearch(s: Record<string, unknown>): RepositorySearch {
  return { tab: repositoryTabs.find((t) => t === s.tab && t !== "snapshots") };
}

/** NewRepositorySearch preselects the remote of a new repository and its path in it. */
export interface NewRepositorySearch {
  remote?: string;
  /** path is a folder in the remote (only with remote). */
  path?: string;
}

/** validateNewRepositorySearch parses /repositories/new's search params. */
export function validateNewRepositorySearch(s: Record<string, unknown>): NewRepositorySearch {
  const remote = str(s.remote);
  const path = remote ? str(s.path)?.replace(/^\/+|\/+$/g, "") : undefined;
  return { remote, path: path || undefined };
}

/** SnapshotSearch is the folder open in the snapshot browser and its listing worker. */
export interface SnapshotSearch {
  /** path is a snapshot tree path; absent for the root. */
  path?: string;
  /** worker pins the worker that lists; absent lets the controller choose. */
  worker?: string;
}

/** validateSnapshotSearch parses the snapshot browser's search params. */
export function validateSnapshotSearch(s: Record<string, unknown>): SnapshotSearch {
  const p = str(s.path);
  return {
    path: p && p.startsWith("/") && p !== "/" ? p : undefined,
    worker: str(s.worker),
  };
}

const snapshotId = (v: unknown) => {
  const s = str(v)?.toLowerCase();
  return s && /^[0-9a-f]{8,64}$/.test(s) ? s : undefined;
};

/** FindSearch is the search page's URL state: the pattern, the scope and the case. */
export interface FindSearch {
  /** q is the pattern; absent shows the form only. */
  q?: string;
  /** snapshot limits the search to one snapshot; absent searches all of them. */
  snapshot?: string;
  /** icase is false to match case; absent ignores case (the default). */
  icase?: false;
  /** worker pins the worker that searches (required for a repository on a worker's disk). */
  worker?: string;
}

/** validateFindSearch parses /repositories/$id/search's search params. */
export function validateFindSearch(s: Record<string, unknown>): FindSearch {
  const q = typeof s.q === "string" ? s.q.replace(/[\r\n\0]/g, "").trim() : "";
  const off = s.icase === false || s.icase === "false" || s.icase === 0 || s.icase === "0";
  return {
    q: q ? q.slice(0, 256) : undefined,
    snapshot: snapshotId(s.snapshot),
    icase: off ? false : undefined,
    worker: str(s.worker),
  };
}

/** CompareSearch is the compare page's URL state. */
export interface CompareSearch {
  /** from is the older snapshot, to the newer (IDs). */
  from?: string;
  to?: string;
  /** path is the folder compared; absent compares the whole snapshots. */
  path?: string;
  /** worker pins the worker that compares. */
  worker?: string;
}

/** validateCompareSearch parses /repositories/$id/compare's search params. */
export function validateCompareSearch(s: Record<string, unknown>): CompareSearch {
  const p = str(s.path);
  const path = p && p.startsWith("/") ? "/" + p.split("/").filter(Boolean).join("/") : undefined;
  return {
    from: snapshotId(s.from),
    to: snapshotId(s.to),
    path: path && path !== "/" ? path : undefined,
    worker: str(s.worker),
  };
}
