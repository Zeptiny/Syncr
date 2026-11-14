// The editable form of a backup's remote source (a folder of one of the
// user's remotes instead of worker paths: ResticBackupDefinition.
// remote_source), its checks, and where the API's field errors about it go.
// restic-definition.ts embeds it in BackupState.
import type { MessageInitShape } from "@bufbuild/protobuf";

import type {
  FieldViolation,
  JobDefinition,
  JobRemoteRef,
  ResticRemoteSourceDefinition,
  ResticRemoteSourceDefinitionSchema,
} from "@/gen/syncr/v1/api_pb";
import { FilterAction, ResticRemoteSourceMethod } from "@/gen/syncr/v1/link_pb";
import type { Repository } from "@/gen/syncr/v1/restic_pb";
import { methodLabel } from "@/lib/remote-sources";

import { bytesToQuantity, isDecimal, quantityToBytes, type ByteUnit } from "./units";

/** RemoteSourceState is the folder of a remote a backup reads, and how. */
export interface RemoteSourceState {
  remoteId: string;
  /** path is relative to the remote's root; "" is the root itself. */
  path: string;
  method: ResticRemoteSourceMethod;
  filters: { action: FilterAction; pattern: string }[];
  /** discardMirror deletes the mirror after each run (mirror only). */
  discardMirror: boolean;
  /** allowEmpty lets an empty folder end the run without a snapshot instead of failing. */
  allowEmpty: boolean;
  /** transfers is rclone's parallel downloads as typed (mirror only). */
  transfers: string;
  /** bwlimit is the bandwidth limit as typed, in bwlimitUnit per second. */
  bwlimit: string;
  bwlimitUnit: ByteUnit;
  /** fastList lists the remote in fewer calls (mirror only). */
  fastList: boolean;
}

/** emptyRemoteSource is a mirror of no folder yet. */
export function emptyRemoteSource(): RemoteSourceState {
  return {
    remoteId: "",
    path: "",
    method: ResticRemoteSourceMethod.MIRROR,
    filters: [],
    discardMirror: false,
    allowEmpty: false,
    transfers: "",
    bwlimit: "",
    bwlimitUnit: "MiB",
    fastList: false,
  };
}

/** remoteSourceState fills the form from a stored remote source. */
export function remoteSourceState(d: ResticRemoteSourceDefinition | undefined): RemoteSourceState {
  const base = emptyRemoteSource();
  if (!d) return base;
  const bw = bytesToQuantity(d.bandwidthLimitBytesPerSecond, "MiB");
  return {
    remoteId: d.remoteId,
    path: d.path,
    method:
      d.method === ResticRemoteSourceMethod.MOUNT
        ? ResticRemoteSourceMethod.MOUNT
        : ResticRemoteSourceMethod.MIRROR,
    filters: d.filters.map((f) => ({ action: f.action, pattern: f.pattern })),
    discardMirror: d.discardMirror,
    allowEmpty: d.allowEmpty,
    transfers: d.transfers === undefined ? "" : String(d.transfers),
    bwlimit: bw.text,
    bwlimitUnit: bw.unit,
    fastList: d.fastList,
  };
}

/** isMirror reports whether the form reads the folder through a mirror. */
export const isMirror = (s: RemoteSourceState) => s.method !== ResticRemoteSourceMethod.MOUNT;

/**
 * toRemoteSource builds the remote source the API takes. The mirror's own
 * options are sent only with the mirror: the controller refuses them with
 * a mount.
 */
export function toRemoteSource(
  s: RemoteSourceState,
): MessageInitShape<typeof ResticRemoteSourceDefinitionSchema> {
  const mirror = isMirror(s);
  return {
    remoteId: s.remoteId,
    path: s.path.trim().replace(/^\/+|\/+$/g, ""),
    method: s.method,
    filters: s.filters
      .filter((f) => f.pattern.trim())
      .map((f) => ({ action: f.action, pattern: f.pattern.trim() })),
    discardMirror: mirror && s.discardMirror,
    allowEmpty: s.allowEmpty,
    transfers: mirror && s.transfers !== "" ? Number(s.transfers) : undefined,
    bandwidthLimitBytesPerSecond: quantityToBytes({ text: s.bwlimit, unit: s.bwlimitUnit }),
    fastList: mirror && s.fastList,
  };
}

/** RemoteSourceErrorKey is a section of the form an error about the remote source belongs to. */
export type RemoteSourceErrorKey =
  "sourceRemoteId" | "sourceFolder" | "sourceMethod" | "sourceFilters" | "sourceOptions";

/**
 * validateRemoteSource checks what the browser can check before sending;
 * the folder's own rules and the overlap with the repository are the
 * controller's.
 */
export function validateRemoteSource(
  s: RemoteSourceState,
): Partial<Record<RemoteSourceErrorKey, string>> {
  const e: Partial<Record<RemoteSourceErrorKey, string>> = {};
  if (!s.remoteId) e.sourceRemoteId = "Choose the remote to back up.";
  if (s.path.split("/").some((seg) => seg === ".."))
    e.sourceFolder = 'A folder cannot contain "..".';
  const mirror = isMirror(s);
  if (mirror && s.transfers !== "") {
    const n = Number(s.transfers);
    if (!/^\d+$/.test(s.transfers) || n < 1 || n > 64)
      e.sourceOptions = "Parallel downloads must be a whole number from 1 to 64.";
  }
  if (!e.sourceOptions && s.bwlimit !== "" && !isDecimal(s.bwlimit))
    e.sourceOptions = "The bandwidth limit must be a number.";
  return e;
}

/**
 * remoteSourceError places an API violation about the remote source, or
 * returns undefined when the field is not one. Fields look like
 * definition.restic_backup.remote_source.path.
 */
export function remoteSourceError(v: FieldViolation): RemoteSourceErrorKey | undefined {
  const m = /\.remote_source(\..*)?$/.exec(v.field);
  if (!m) return undefined;
  const rest = m[1] ?? "";
  if (rest.startsWith(".remote_id")) return "sourceRemoteId";
  if (rest.startsWith(".path")) return "sourceFolder";
  if (rest.startsWith(".method")) return "sourceMethod";
  if (rest.startsWith(".filters")) return "sourceFilters";
  return "sourceOptions";
}

/** remoteSourceOf returns a definition's remote source, if it backs up a folder of a remote. */
export function remoteSourceOf(
  def: JobDefinition | undefined,
): ResticRemoteSourceDefinition | undefined {
  const op = def?.operation;
  return op?.case === "resticBackup" ? op.value.remoteSource : undefined;
}

const segments = (p: string) => p.split("/").filter((s) => s !== "" && s !== ".");

/**
 * repositoryOverlap says how the folder relates to the repository's when
 * both are on the same remote (the controller's RemoteSourceOverlap),
 * folders compared whole: undefined when neither holds the other.
 */
export function repositoryOverlap(
  s: Pick<RemoteSourceState, "remoteId" | "path">,
  repo: Pick<Repository, "remoteId" | "path" | "name"> | undefined,
): string | undefined {
  if (!repo || !s.remoteId || s.remoteId !== repo.remoteId) return undefined;
  const src = segments(s.path);
  const r = segments(repo.path);
  const prefix = (a: string[], b: string[]) => a.every((x, i) => b[i] === x);
  const where = repo.path ? `at ${repo.path}` : "at the remote's root";
  if (src.length === r.length && prefix(src, r))
    return `This is the folder of repository ${repo.name} (${where}): a backup cannot read its own repository.`;
  if (src.length < r.length && prefix(src, r))
    return `This folder holds repository ${repo.name} (${where}): a backup cannot read its own repository. Choose a folder beside it.`;
  if (r.length < src.length && prefix(r, src))
    return `This folder is inside repository ${repo.name} (${where}): a backup cannot read its own repository.`;
  return undefined;
}

/**
 * remoteSourceTitle names a backup of a remote as the controller titles
 * it: "Nextcloud/Photos (mirror)", or "Nextcloud (mount)" for its root.
 * remotes are the job's or schedule's remote refs.
 */
export function remoteSourceTitle(
  src: Pick<ResticRemoteSourceDefinition, "remoteId" | "path" | "method">,
  remotes: Pick<JobRemoteRef, "id" | "name">[],
): string {
  const name = remotes.find((r) => r.id === src.remoteId)?.name || "a remote";
  const path = src.path.replace(/^\/+|\/+$/g, "");
  return `${path ? `${name}/${path}` : name} (${methodLabel(src.method).toLowerCase()})`;
}
