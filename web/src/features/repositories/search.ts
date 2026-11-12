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

/** NewRepositorySearch preselects the remote of a new repository. */
export interface NewRepositorySearch {
  remote?: string;
}

/** validateNewRepositorySearch parses /repositories/new's search params. */
export function validateNewRepositorySearch(s: Record<string, unknown>): NewRepositorySearch {
  return { remote: str(s.remote) };
}

/** SnapshotSearch is the folder open in the snapshot browser. */
export interface SnapshotSearch {
  /** path is a snapshot tree path; absent for the root. */
  path?: string;
}

/** validateSnapshotSearch parses the snapshot browser's search params. */
export function validateSnapshotSearch(s: Record<string, unknown>): SnapshotSearch {
  const p = str(s.path);
  return { path: p && p.startsWith("/") && p !== "/" ? p : undefined };
}
