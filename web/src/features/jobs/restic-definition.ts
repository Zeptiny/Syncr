// The editable forms of the restic job kinds a user can start or schedule:
// backup, retention (forget), check and prune. definition.ts joins them
// with transfers into JobDefinitionState.
import type { MessageInitShape } from "@bufbuild/protobuf";

import type {
  ResticBackupDefinition,
  ResticBackupDefinitionSchema,
  ResticCheckDefinition,
  ResticCheckDefinitionSchema,
  ResticForgetDefinition,
  ResticForgetDefinitionSchema,
  ResticPruneDefinition,
  ResticPruneDefinitionSchema,
} from "@/gen/syncr/v1/api_pb";
import {
  cleanTags,
  defaultRetention,
  hostProblem,
  keepCount,
  retentionCounts,
  retentionPolicyProblem,
  tagsProblem,
  type RetentionPolicyState,
} from "@/lib/retention";

import { MAX_REMOTE_SOURCE_TAGS, REMOTE_TAG_PREFIX } from "@/lib/remote-sources";

import {
  emptyRemoteSource,
  remoteSourceState,
  toRemoteSource,
  validateRemoteSource,
  type RemoteSourceErrorKey,
  type RemoteSourceState,
} from "./remote-source";
import { decimalToScaled, isDecimal } from "./units";

// This module must not import definition.ts (which imports it); the shared
// error type is repeated structurally instead.
type Errors = Partial<
  Record<
    | "options"
    | "repository"
    | "paths"
    | "excludes"
    | "policy"
    | "readData"
    | "tags"
    | RemoteSourceErrorKey,
    string
  >
>;

const MiB = 1024n * 1024n;

const isAbsolute = (p: string) =>
  p.startsWith("/") || /^[A-Za-z]:[\\/]/.test(p) || p.startsWith("\\\\");

const clean = (list: string[]) => [...new Set(list.map((x) => x.trim()).filter(Boolean))];

/**
 * BackupSource is what a backup reads: folders on the worker (paths) or a
 * folder of one of the user's remotes (remote).
 */
export type BackupSource = "paths" | "remote";

/**
 * BackupState is the editable form of a backup. It keeps both sources, so
 * switching between them loses nothing typed; only the chosen one is sent.
 */
export interface BackupState {
  repositoryId: string;
  source: BackupSource;
  /** remote is the folder of a remote, used when source is "remote". */
  remote: RemoteSourceState;
  paths: string[];
  excludes: string[];
  tags: string[];
  /** host overrides the host name recorded in the snapshot; "" uses the worker's. */
  host: string;
  /** useFsSnapshot backs up from a VSS snapshot (Windows only). */
  useFsSnapshot: boolean;
  excludeCaches: boolean;
  oneFileSystem: boolean;
  /** excludeLargerThan is a size in MiB as typed; "" for no limit. */
  excludeLargerThan: string;
  dryRun: boolean;
}

/** emptyBackup is a blank backup with one empty path. */
export function emptyBackup(): BackupState {
  return {
    repositoryId: "",
    source: "paths",
    remote: emptyRemoteSource(),
    paths: [""],
    excludes: [],
    tags: [],
    host: "",
    useFsSnapshot: false,
    excludeCaches: false,
    oneFileSystem: false,
    excludeLargerThan: "",
    dryRun: false,
  };
}

/** mibText shows a byte count in MiB, exactly when it is a whole number of them. */
function mibText(bytes: bigint | undefined): string {
  if (bytes === undefined || bytes <= 0n) return "";
  let rest = bytes % MiB;
  let frac = "";
  // MiB is a power of two, so the expansion ends within 20 digits.
  for (let i = 0; i < 20 && rest !== 0n; i++) {
    rest *= 10n;
    frac += String(rest / MiB);
    rest %= MiB;
  }
  return frac ? `${bytes / MiB}.${frac}` : String(bytes / MiB);
}

/** backupState fills the form from a stored backup definition. */
export function backupState(d: ResticBackupDefinition): BackupState {
  return {
    repositoryId: d.repositoryId,
    source: d.remoteSource ? "remote" : "paths",
    remote: remoteSourceState(d.remoteSource),
    paths: d.paths.length ? [...d.paths] : [""],
    excludes: [...d.excludes],
    tags: [...d.tags],
    host: d.host,
    useFsSnapshot: d.useFsSnapshot,
    excludeCaches: d.excludeCaches,
    oneFileSystem: d.oneFileSystem,
    excludeLargerThan: mibText(d.excludeLargerThanBytes),
    dryRun: d.dryRun,
  };
}

/** validateBackup checks what the browser can check before sending. */
export function validateBackup(s: BackupState): Errors {
  const e: Errors = {};
  if (!s.repositoryId) e.repository = "Choose the repository to back up into.";
  if (s.source === "remote") {
    Object.assign(e, validateRemoteSource(s.remote));
    const tags = remoteTagsProblem(clean(s.tags));
    if (tags) e.tags = tags;
  } else {
    const paths = clean(s.paths);
    if (paths.length === 0) e.paths = "Add at least one folder or file to back up.";
    else if (paths.some((p) => !isAbsolute(p)))
      e.paths = "Enter absolute paths on the worker, such as /srv/data.";
  }
  if (s.excludeLargerThan !== "" && (!isDecimal(s.excludeLargerThan) || !larger(s)))
    e.options = "The size limit must be a number above zero.";
  return e;
}

/**
 * remoteTagsProblem checks the tags of a backup of a remote: Syncr adds
 * its own syncr-remote: tag, so there is room for one tag less and that
 * prefix is taken.
 */
export function remoteTagsProblem(tags: string[]): string | undefined {
  if (tags.length > MAX_REMOTE_SOURCE_TAGS)
    return `Use at most ${MAX_REMOTE_SOURCE_TAGS} tags: Syncr adds one to name the remote.`;
  const taken = tags.find((t) => t.startsWith(REMOTE_TAG_PREFIX));
  if (taken) return `Tags cannot start with “${REMOTE_TAG_PREFIX}”: Syncr adds that tag itself.`;
  return undefined;
}

function larger(s: BackupState): bigint | undefined {
  if (s.excludeLargerThan === "" || !isDecimal(s.excludeLargerThan)) return undefined;
  const bytes = decimalToScaled(s.excludeLargerThan, MiB);
  return bytes > 0n ? bytes : undefined;
}

/**
 * toBackup builds the backup definition the API takes: the chosen source
 * only. A backup of a remote never sends paths, restic excludes, one file
 * system or VSS, which the controller refuses with it.
 */
export function toBackup(s: BackupState): MessageInitShape<typeof ResticBackupDefinitionSchema> {
  if (s.source === "remote")
    return {
      repositoryId: s.repositoryId,
      remoteSource: toRemoteSource(s.remote),
      tags: clean(s.tags),
      host: s.host.trim(),
      excludeCaches: s.excludeCaches,
      excludeLargerThanBytes: larger(s),
      dryRun: s.dryRun,
    };
  return {
    repositoryId: s.repositoryId,
    paths: clean(s.paths),
    excludes: clean(s.excludes),
    tags: clean(s.tags),
    host: s.host.trim(),
    useFsSnapshot: s.useFsSnapshot,
    excludeCaches: s.excludeCaches,
    oneFileSystem: s.oneFileSystem,
    excludeLargerThanBytes: larger(s),
    dryRun: s.dryRun,
  };
}

/** RetentionState is the editable form of a retention ("forget") job. */
export interface RetentionState {
  repositoryId: string;
  policy: RetentionPolicyState;
  /** prune also frees the space of the removed snapshots (on by default). */
  prune: boolean;
  /** tags and host limit the snapshots the policy looks at. */
  tags: string[];
  host: string;
  dryRun: boolean;
}

/** emptyRetentionJob keeps 7 daily, 4 weekly and 12 monthly snapshots and prunes. */
export function emptyRetentionJob(): RetentionState {
  return {
    repositoryId: "",
    policy: defaultRetention(),
    prune: true,
    tags: [],
    host: "",
    dryRun: false,
  };
}

/** retentionState fills the form from a stored forget definition. */
export function retentionState(d: ResticForgetDefinition): RetentionState {
  const p = d.policy;
  const n = (v: number | undefined) => (v && v > 0 ? String(v) : "");
  return {
    repositoryId: d.repositoryId,
    policy: {
      keepLast: n(p?.keepLast),
      keepHourly: n(p?.keepHourly),
      keepDaily: n(p?.keepDaily),
      keepWeekly: n(p?.keepWeekly),
      keepMonthly: n(p?.keepMonthly),
      keepYearly: n(p?.keepYearly),
      keepWithin: p?.keepWithin ?? "",
      keepTags: [...(p?.keepTags ?? [])],
    },
    prune: d.prune,
    tags: [...d.tags],
    host: d.host,
    dryRun: d.dryRun,
  };
}

/**
 * validateRetention checks what the browser can check before sending: the
 * controller's rules for the policy (retentionPolicyProblem) and for the
 * tag and host filters, on the values toRetention sends.
 */
export function validateRetention(s: RetentionState): Errors {
  const e: Errors = {};
  if (!s.repositoryId) e.repository = "Choose the repository to clean up.";
  const policy = retentionPolicyProblem(s.policy);
  if (policy) e.policy = policy;
  const filters = tagsProblem(cleanTags(s.tags)) ?? hostProblem(s.host.trim());
  if (filters) e.options = filters;
  return e;
}

/** toRetention builds the forget definition the API takes. */
export function toRetention(
  s: RetentionState,
): MessageInitShape<typeof ResticForgetDefinitionSchema> {
  const counts = Object.fromEntries(
    retentionCounts.map((c) => {
      // validateRetention refuses anything but whole numbers: never send a
      // fraction to an int32 field.
      const n = keepCount(s.policy[c.key]);
      return [c.key, Number.isInteger(n) ? n : 0];
    }),
  ) as Record<(typeof retentionCounts)[number]["key"], number>;
  return {
    repositoryId: s.repositoryId,
    policy: {
      ...counts,
      keepWithin: s.policy.keepWithin.trim(),
      keepTags: cleanTags(s.policy.keepTags),
    },
    prune: s.prune,
    tags: cleanTags(s.tags),
    host: s.host.trim(),
    dryRun: s.dryRun,
  };
}

/** ReadData is how much pack data a check reads back. */
export type ReadData = "0" | "1" | "10" | "100" | "custom";

/** readDataOptions are the choices of a check, in display order. */
export const readDataOptions: { value: ReadData; label: string; help: string }[] = [
  { value: "0", label: "Structure only", help: "Fast. Checks the index and the snapshots." },
  { value: "1", label: "1%", help: "Reads a random 1% of the data." },
  { value: "10", label: "10%", help: "A deeper monthly check." },
  { value: "100", label: "100%", help: "Reads everything. Slow and downloads it all." },
  { value: "custom", label: "Custom", help: "Choose the share yourself." },
];

/** CheckState is the editable form of a check. */
export interface CheckState {
  repositoryId: string;
  readData: ReadData;
  /** custom is the percentage typed for "Custom". */
  custom: string;
}

/** emptyCheck checks the structure only. */
export function emptyCheck(): CheckState {
  return { repositoryId: "", readData: "0", custom: "" };
}

/** checkState fills the form from a stored check definition. */
export function checkState(d: ResticCheckDefinition): CheckState {
  const pct = d.readDataPercent;
  const preset = (["0", "1", "10", "100"] as const).find((v) => Number(v) === pct);
  return {
    repositoryId: d.repositoryId,
    readData: preset ?? "custom",
    custom: preset ? "" : String(pct),
  };
}

/** checkPercent is the percentage a check form asks for, or undefined when it is not valid. */
export function checkPercent(s: CheckState): number | undefined {
  if (s.readData !== "custom") return Number(s.readData);
  if (!/^\d{1,3}$/.test(s.custom)) return undefined;
  const n = Number(s.custom);
  return n >= 1 && n <= 100 ? n : undefined;
}

/** validateCheck checks what the browser can check before sending. */
export function validateCheck(s: CheckState): Errors {
  const e: Errors = {};
  if (!s.repositoryId) e.repository = "Choose the repository to check.";
  if (checkPercent(s) === undefined) e.readData = "Enter a whole percentage from 1 to 100.";
  return e;
}

/** toCheck builds the check definition the API takes. */
export function toCheck(s: CheckState): MessageInitShape<typeof ResticCheckDefinitionSchema> {
  return { repositoryId: s.repositoryId, readDataPercent: checkPercent(s) ?? 0 };
}

/** PruneState is the editable form of a prune. */
export interface PruneState {
  repositoryId: string;
  dryRun: boolean;
}

/** emptyPrune is a blank prune. */
export function emptyPrune(): PruneState {
  return { repositoryId: "", dryRun: false };
}

/** pruneState fills the form from a stored prune definition. */
export function pruneState(d: ResticPruneDefinition): PruneState {
  return { repositoryId: d.repositoryId, dryRun: d.dryRun };
}

/** validatePrune checks what the browser can check before sending. */
export function validatePrune(s: PruneState): Errors {
  return s.repositoryId ? {} : { repository: "Choose the repository to prune." };
}

/** toPrune builds the prune definition the API takes. */
export function toPrune(s: PruneState): MessageInitShape<typeof ResticPruneDefinitionSchema> {
  return { repositoryId: s.repositoryId, dryRun: s.dryRun };
}
