// What every page needs to know about backups of a folder of a remote
// (ResticBackupDefinition.remote_source): the snapshot tag that names the
// remote, the methods a worker reads the folder with, and what a worker
// reports it can do (WorkerInfo.remote_sources). No React here.
import type { Worker } from "@/gen/syncr/v1/api_pb";
import { ResticRemoteSourceMethod } from "@/gen/syncr/v1/link_pb";

import { FEATURE_REMOTE_SOURCE, hasFeature } from "./workers";

/**
 * REMOTE_TAG_PREFIX starts the tag the controller adds to every snapshot of
 * a remote: syncr-remote:<remote id> (internal/link/remotesource.go).
 */
export const REMOTE_TAG_PREFIX = "syncr-remote:";

/** MAX_REMOTE_SOURCE_TAGS is how many tags a user may set on a backup of a remote (Syncr adds one). */
export const MAX_REMOTE_SOURCE_TAGS = 31;

/** remoteTagId returns the remote ID a tag names, or undefined for any other tag. */
export function remoteTagId(tag: string): string | undefined {
  return tag.startsWith(REMOTE_TAG_PREFIX) && tag.length > REMOTE_TAG_PREFIX.length
    ? tag.slice(REMOTE_TAG_PREFIX.length)
    : undefined;
}

/** isRemoteTag reports whether a tag is the one Syncr adds to snapshots of a remote. */
export function isRemoteTag(tag: string): boolean {
  return remoteTagId(tag) !== undefined;
}

/** MethodMeta describes a way of reading a remote folder. */
export interface MethodMeta {
  method: ResticRemoteSourceMethod;
  label: string;
  /** progress is the live phase's label ("Mirroring…"). */
  verb: string;
}

/** methodLabel names a method: "Mirror" or "Mount". */
export function methodLabel(m: ResticRemoteSourceMethod | undefined): string {
  return m === ResticRemoteSourceMethod.MOUNT ? "Mount" : "Mirror";
}

/** RemoteSourceSupport is what a worker can do for backups of a remote, as the UI uses it. */
export type RemoteSourceSupport =
  /** The worker has not reported yet (pending, or never connected): the controller decides. */
  | { kind: "unknown" }
  /** The worker runs a release without the feature. */
  | { kind: "outdated" }
  | {
      kind: "reported";
      mirror: boolean;
      mount: boolean;
      mirrorReason: string;
      mountReason: string;
    };

/**
 * remoteSourceSupport reads a worker's report. A worker that reported
 * nothing (an older release) is outdated once it has connected; one that
 * has not connected is unknown.
 */
export function remoteSourceSupport(w: Worker | undefined): RemoteSourceSupport {
  const info = w?.info;
  if (!info) return { kind: "unknown" };
  const rs = info.remoteSources;
  if (!rs) return hasFeature(w, FEATURE_REMOTE_SOURCE) ? { kind: "unknown" } : { kind: "outdated" };
  return {
    kind: "reported",
    mirror: rs.mirror,
    mount: rs.mount,
    mirrorReason: rs.mirrorReason,
    mountReason: rs.mountReason,
  };
}

/**
 * methodProblem says why a worker cannot read a remote with a method, in a
 * few words for a list ("outdated", "can't mount: /dev/fuse is missing"),
 * or undefined when it can or has not said.
 */
export function methodProblem(
  w: Worker | undefined,
  method: ResticRemoteSourceMethod,
): string | undefined {
  const s = remoteSourceSupport(w);
  if (s.kind === "outdated") return "outdated";
  if (s.kind !== "reported") return undefined;
  if (method === ResticRemoteSourceMethod.MOUNT)
    return s.mount ? undefined : `can't mount${s.mountReason ? `: ${s.mountReason}` : ""}`;
  return s.mirror ? undefined : `can't mirror${s.mirrorReason ? `: ${s.mirrorReason}` : ""}`;
}

/**
 * methodRefusal is the sentence a method card shows when the worker cannot
 * use it: the controller's wording of RunJob's refusal, or undefined.
 */
export function methodRefusal(
  w: Worker | undefined,
  method: ResticRemoteSourceMethod,
): string | undefined {
  const s = remoteSourceSupport(w);
  if (!w || s.kind === "unknown") return undefined;
  if (s.kind === "outdated") return `${w.name} must be upgraded to back up a folder of a remote.`;
  const mount = method === ResticRemoteSourceMethod.MOUNT;
  if (mount ? s.mount : s.mirror) return undefined;
  const why = (mount ? s.mountReason : s.mirrorReason) || "it gave no reason";
  return `${w.name} ${mount ? "cannot mount a remote" : "cannot keep a mirror of a remote"}: ${why}.`;
}

/**
 * AbilityRow is one method of a worker's report for the workers pages:
 * "Mirror ✓", or "Mount ✗" with the reason.
 */
export interface AbilityRow {
  label: string;
  ok: boolean;
  reason: string;
}

/**
 * workerAbilities lists what a worker reported for backups of a remote, or
 * undefined when it reported nothing (an older release, or not connected).
 */
export function workerAbilities(w: Worker | undefined): AbilityRow[] | undefined {
  const s = remoteSourceSupport(w);
  if (s.kind !== "reported") return undefined;
  return [
    { label: "Mirror", ok: s.mirror, reason: s.mirror ? "" : s.mirrorReason },
    { label: "Mount", ok: s.mount, reason: s.mount ? "" : s.mountReason },
  ];
}

/** abilitiesText writes a worker's report on one line: "Mirror ✓ · Mount ✗ – /dev/fuse is missing". */
export function abilitiesText(rows: AbilityRow[]): string {
  return rows
    .map((r) => `${r.label} ${r.ok ? "✓" : "✗"}${!r.ok && r.reason ? ` – ${r.reason}` : ""}`)
    .join(" · ");
}

/** PHASE_MIRROR and PHASE_BACKUP are JobProgress.phase's values. */
export const PHASE_MIRROR = "mirror";
export const PHASE_BACKUP = "backup";

/** phaseLabel names a live phase: "Mirroring…", "Backing up…", or "" for none. */
export function phaseLabel(phase: string | undefined): string {
  if (phase === PHASE_MIRROR) return "Mirroring…";
  if (phase === PHASE_BACKUP) return "Backing up…";
  return "";
}
