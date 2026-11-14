// Where a snapshot's files came from, for the snapshot pages: folders of
// its host (restic's paths), or a folder of a remote. A backup of a remote
// is tagged syncr-remote:<remote id> by the controller, and its paths are
// the worker's internal mirror or mount folder, which mean nothing to
// people: such snapshots are named after the remote instead. No React here.
import type { ResticSnapshot } from "@/gen/syncr/v1/link_pb";
import type { Remote } from "@/gen/syncr/v1/remotes_pb";
import { isRemoteTag, remoteTagId } from "@/lib/remote-sources";

/** RemoteNames maps the user's remote IDs to their names. */
export type RemoteNames = ReadonlyMap<string, string>;

/** remoteNames indexes the user's remotes by ID. */
export function remoteNames(remotes: readonly Pick<Remote, "id" | "name">[]): RemoteNames {
  return new Map(remotes.map((r) => [r.id, r.name]));
}

/** snapshotRemoteId is the remote a snapshot was backed up from, or undefined for folders of its host. */
export function snapshotRemoteId(s: Pick<ResticSnapshot, "tags">): string | undefined {
  for (const t of s.tags ?? []) {
    const id = remoteTagId(t);
    if (id) return id;
  }
  return undefined;
}

/** isRemoteSnapshot reports whether a snapshot is of a folder of a remote. */
export function isRemoteSnapshot(s: { tags?: readonly string[] } | undefined): boolean {
  return !!s?.tags?.some(isRemoteTag);
}

/**
 * remoteOrigin names the remote of a snapshot of a remote: "From Nextcloud",
 * or "From a remote" when the remote is not one of the user's (or gone).
 * Undefined for snapshots of folders.
 */
export function remoteOrigin(
  s: Pick<ResticSnapshot, "tags">,
  names: RemoteNames,
): string | undefined {
  const id = snapshotRemoteId(s);
  if (!id) return undefined;
  const name = names.get(id);
  return name ? `From ${name}` : "From a remote";
}

/** tagLabel shows a tag: Syncr's remote tag as "From <remote>", any other as it is. */
export function tagLabel(tag: string, names: RemoteNames): string {
  const id = remoteTagId(tag);
  if (!id) return tag;
  const name = names.get(id);
  return name ? `From ${name}` : "From a remote";
}

/** userTags are a snapshot's tags without Syncr's remote tag (shown as a badge instead). */
export function userTags(s: Pick<ResticSnapshot, "tags">): string[] {
  return s.tags.filter((t) => !isRemoteTag(t));
}

/**
 * originText is what a snapshot backed up, in one line: its paths, or
 * the remote ("From Nextcloud") for a snapshot of a remote.
 */
export function originText(s: Pick<ResticSnapshot, "tags" | "paths">, names: RemoteNames): string {
  return remoteOrigin(s, names) ?? s.paths.join(", ");
}

/**
 * REMOTE_RESTORE_HINT says why a snapshot of a remote is never restored
 * "to the original location": its paths are a worker's internal folder,
 * and restoring back to a remote is not possible yet.
 */
export const REMOTE_RESTORE_HINT =
  "This snapshot is of a remote: restore it to a worker folder, then copy it to the remote";
