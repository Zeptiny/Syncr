// Versions as the controller reports them (GetInfo), and what the docs make
// of them: the release notes to point at and the Git ref of GitHub links.

/**
 * A release tag, as internal/adapters/api/install.go defines it: vX.Y.Z or
 * vX.Y.Z-rc.N. Snapshot builds (dev-<sha>), git describe output
 * (v2.3.0-rc.1-4-gabc1234, -dirty), "dev" and Go pseudo-versions are not
 * releases.
 */
const RELEASE_RE = /^v(\d+)\.(\d+)\.(\d+)(?:-rc\.(\d+))?$/;

/** Release is a parsed release tag. */
export interface Release {
  major: number;
  minor: number;
  patch: number;
  /** The release candidate number; undefined for the final release. */
  rc?: number;
  /** The final version this release belongs to: vX.Y.Z. */
  base: string;
}

/** parseRelease parses a release tag, or returns undefined for anything else. */
export function parseRelease(version: string | undefined | null): Release | undefined {
  const m = RELEASE_RE.exec(version ?? "");
  if (!m) return undefined;
  const [major, minor, patch] = [m[1], m[2], m[3]].map(Number);
  const release: Release = { major, minor, patch, base: `v${major}.${minor}.${patch}` };
  if (m[4] !== undefined) release.rc = Number(m[4]);
  return release;
}

/**
 * compareReleases orders releases like the controller's
 * compareReleaseVersions: by version, and rc.N before the final release.
 */
export function compareReleases(a: Release, b: Release): number {
  const parts: [number, number][] = [
    [a.major, b.major],
    [a.minor, b.minor],
    [a.patch, b.patch],
  ];
  for (const [x, y] of parts) if (x !== y) return x < y ? -1 : 1;
  if (a.rc === b.rc) return 0;
  if (a.rc === undefined) return 1;
  if (b.rc === undefined) return -1;
  return a.rc < b.rc ? -1 : 1;
}

/**
 * notesSlug is the docs page with the release notes of a version
 * (releases/vX.Y.Z; a release candidate uses its final version's notes), or
 * undefined when the version is not a release.
 */
export function notesSlug(version: string | undefined): string | undefined {
  const release = parseRelease(version);
  return release ? `releases/${release.base}` : undefined;
}

/**
 * githubRef is the Git ref GitHub links point to: the tag of a release
 * (release candidates included), main for every other build.
 */
export function githubRef(version: string | undefined): string {
  return version !== undefined && parseRelease(version) ? version : "main";
}

/** isSafeRef accepts only a release tag or main: the values githubRef returns. */
export function isSafeRef(ref: string): boolean {
  return ref === "main" || parseRelease(ref) !== undefined;
}
