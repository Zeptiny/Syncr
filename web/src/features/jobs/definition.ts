import type { MessageInitShape } from "@bufbuild/protobuf";

import type { Location } from "@/components/app/location-editor";
import type { FieldViolation, JobDefinition, JobDefinitionSchema } from "@/gen/syncr/v1/api_pb";
import { FilterAction, RcloneTransferMode } from "@/gen/syncr/v1/link_pb";
import { toNumber } from "@/lib/format";

import {
  backupState,
  checkState,
  emptyBackup,
  emptyCheck,
  emptyPrune,
  emptyRetentionJob,
  pruneState,
  retentionState,
  toBackup,
  toCheck,
  toPrune,
  toRetention,
  validateBackup,
  validateCheck,
  validatePrune,
  validateRetention,
  type BackupState,
  type CheckState,
  type PruneState,
  type RetentionState,
} from "./restic-definition";
import {
  bytesToQuantity,
  durationToQuantity,
  isDecimal,
  quantityToBytes,
  quantityToDuration,
  type ByteUnit,
  type DurationUnit,
} from "./units";

/** Compare is how a transfer decides that a file changed. */
export type Compare = "default" | "checksum" | "size";

/**
 * TransferState is the editable form of a transfer job definition, shared
 * by "Run a job" and schedules. Numbers are kept as typed text.
 */
export interface TransferState {
  mode: RcloneTransferMode;
  source: Location;
  destination: Location;
  transfers: string;
  checkers: string;
  /** bwlimit is the bandwidth limit as typed, in bwlimitUnit per second. */
  bwlimit: string;
  bwlimitUnit: ByteUnit;
  dryRun: boolean;
  compare: Compare;
  ignoreExisting: boolean;
  update: boolean;
  createEmptySrcDirs: boolean;
  deleteEmptySrcDirs: boolean;
  trackRenames: boolean;
  fastList: boolean;
  maxTransfer: string;
  maxTransferUnit: ByteUnit;
  maxDuration: string;
  maxDurationUnit: DurationUnit;
  retries: string;
  lowLevelRetries: string;
  filters: { action: FilterAction; pattern: string }[];
}

/**
 * DefinitionErrors are the definition's problems, by form section. The
 * first four belong to transfers; repository, paths, excludes, policy and
 * readData to the restic kinds; options to every kind.
 */
export type DefinitionErrors = Partial<
  Record<
    | "sourceRemote"
    | "sourcePath"
    | "destRemote"
    | "destPath"
    | "options"
    | "repository"
    | "paths"
    | "excludes"
    | "policy"
    | "readData",
    string
  >
>;

/** emptyTransfer is a blank copy definition. */
export function emptyTransfer(): TransferState {
  return {
    mode: RcloneTransferMode.COPY,
    source: { remoteId: "", path: "" },
    destination: { remoteId: "", path: "" },
    transfers: "",
    checkers: "",
    bwlimit: "",
    bwlimitUnit: "MiB",
    dryRun: false,
    compare: "default",
    ignoreExisting: false,
    update: false,
    createEmptySrcDirs: false,
    deleteEmptySrcDirs: false,
    trackRenames: false,
    fastList: false,
    maxTransfer: "",
    maxTransferUnit: "GiB",
    maxDuration: "",
    maxDurationUnit: "min",
    retries: "",
    lowLevelRetries: "",
    filters: [],
  };
}

const num = (v: number | bigint | undefined) => (v === undefined ? "" : String(toNumber(v)));

/** transferState fills the transfer form from a stored definition (a job or a schedule). */
export function transferState(def: JobDefinition | undefined): TransferState {
  const base = emptyTransfer();
  const op = def?.operation;
  if (op?.case !== "rcloneTransfer") return base;
  const t = op.value;
  const o = t.options;
  const bw = bytesToQuantity(o?.bandwidthLimitBytesPerSecond, "MiB");
  const maxTransfer = bytesToQuantity(o?.maxTransferBytes, "GiB");
  const maxDuration = durationToQuantity(o?.maxDuration, "min");
  return {
    ...base,
    mode: t.mode || RcloneTransferMode.COPY,
    source: { remoteId: t.source?.remoteId ?? "", path: t.source?.path ?? "" },
    destination: { remoteId: t.destination?.remoteId ?? "", path: t.destination?.path ?? "" },
    transfers: num(o?.transfers),
    checkers: num(o?.checkers),
    bwlimit: bw.text,
    bwlimitUnit: bw.unit,
    dryRun: o?.dryRun ?? false,
    compare: o?.checksum ? "checksum" : o?.sizeOnly ? "size" : "default",
    ignoreExisting: o?.ignoreExisting ?? false,
    update: o?.update ?? false,
    createEmptySrcDirs: o?.createEmptySrcDirs ?? false,
    deleteEmptySrcDirs: o?.deleteEmptySrcDirs ?? false,
    trackRenames: o?.trackRenames ?? false,
    fastList: o?.fastList ?? false,
    maxTransfer: maxTransfer.text,
    maxTransferUnit: maxTransfer.unit,
    maxDuration: maxDuration.text,
    maxDurationUnit: maxDuration.unit,
    retries: num(o?.retries),
    lowLevelRetries: num(o?.lowLevelRetries),
    filters: (o?.filters ?? []).map((f) => ({ action: f.action, pattern: f.pattern })),
  };
}

/** isAbsolute reports whether p is an absolute path on Linux or Windows. */
export function isAbsolute(p: string): boolean {
  return p.startsWith("/") || /^[A-Za-z]:[\\/]/.test(p) || p.startsWith("\\\\");
}

/** validateTransfer checks what the browser can check before sending. */
export function validateTransfer(s: TransferState): DefinitionErrors {
  const e: DefinitionErrors = {};
  for (const [loc, path] of [
    [s.source, "sourcePath"],
    [s.destination, "destPath"],
  ] as const) {
    if (loc.remoteId === "" && !isAbsolute(loc.path.trim())) {
      e[path] = "Enter an absolute path on the worker, such as /srv/data.";
    }
  }
  if (
    s.source.remoteId === s.destination.remoteId &&
    s.source.path.trim() === s.destination.path.trim()
  ) {
    e.destPath = "The destination is the same as the source.";
  }
  const ints = [s.transfers, s.checkers, s.retries, s.lowLevelRetries];
  const decimals = [s.bwlimit, s.maxTransfer, s.maxDuration];
  if (
    ints.some((v) => v !== "" && !/^\d+$/.test(v)) ||
    decimals.some((v) => v !== "" && !isDecimal(v))
  ) {
    e.options = "Numbers must be zero or more.";
  }
  return e;
}

const optInt = (v: string) => (v === "" ? undefined : Number(v));

/** toTransferDefinition builds the JobDefinition the API takes. */
export function toTransferDefinition(
  s: TransferState,
): MessageInitShape<typeof JobDefinitionSchema> {
  const mode = s.mode;
  return {
    operation: {
      case: "rcloneTransfer",
      value: {
        mode,
        source: { remoteId: s.source.remoteId, path: s.source.path.trim() },
        destination: { remoteId: s.destination.remoteId, path: s.destination.path.trim() },
        options: {
          transfers: optInt(s.transfers),
          checkers: optInt(s.checkers),
          bandwidthLimitBytesPerSecond: quantityToBytes({ text: s.bwlimit, unit: s.bwlimitUnit }),
          dryRun: s.dryRun,
          checksum: s.compare === "checksum",
          sizeOnly: s.compare === "size",
          ignoreExisting: s.ignoreExisting,
          update: s.update,
          createEmptySrcDirs: s.createEmptySrcDirs,
          deleteEmptySrcDirs: mode === RcloneTransferMode.MOVE && s.deleteEmptySrcDirs,
          trackRenames: mode === RcloneTransferMode.SYNC && s.trackRenames,
          fastList: s.fastList,
          maxTransferBytes: quantityToBytes({ text: s.maxTransfer, unit: s.maxTransferUnit }),
          maxDuration: quantityToDuration({ text: s.maxDuration, unit: s.maxDurationUnit }),
          retries: optInt(s.retries),
          lowLevelRetries: optInt(s.lowLevelRetries),
          filters: s.filters
            .filter((f) => f.pattern.trim())
            .map((f) => ({ action: f.action, pattern: f.pattern.trim() })),
        },
      },
    },
  };
}

/**
 * definitionError places one API violation on a definition section, or
 * returns false when the field is not part of the definition. Paths look
 * like definition.rclone_transfer.source.path or
 * definition.restic_backup.paths[0].
 */
export function definitionError(v: FieldViolation, errors: DefinitionErrors): boolean {
  const f = v.field;
  if (f.includes(".restic_")) {
    if (f.endsWith("repository_id")) errors.repository = v.message;
    else if (f.includes(".paths")) errors.paths = v.message;
    else if (f.includes(".excludes")) errors.excludes = v.message;
    else if (f.includes(".policy")) errors.policy = v.message;
    else if (f.endsWith("read_data_percent")) errors.readData = v.message;
    else errors.options = v.message;
    return true;
  }
  if (f.includes("source"))
    errors[f.endsWith("remote_id") ? "sourceRemote" : "sourcePath"] = v.message;
  else if (f.includes("destination"))
    errors[f.endsWith("remote_id") ? "destRemote" : "destPath"] = v.message;
  else if (f.includes("options")) errors.options = v.message;
  else return false;
  return true;
}

/**
 * JobDefinitionState is the editable form of a job definition of any kind
 * a user can start or schedule. Restores are not here: they start from a
 * snapshot (the restore sheet); nor is restic_delete_data, which only
 * deleting a repository starts.
 */
export type JobDefinitionState =
  | { kind: "rclone_transfer"; transfer: TransferState }
  | { kind: "restic_backup"; backup: BackupState }
  | { kind: "restic_forget"; retention: RetentionState }
  | { kind: "restic_check"; check: CheckState }
  | { kind: "restic_prune"; prune: PruneState };

/** EditableKind is a job kind the definition editor can edit. */
export type EditableKind = JobDefinitionState["kind"];

/** editableKinds are the kinds of JobDefinitionState, in display order. */
export const editableKinds: EditableKind[] = [
  "rclone_transfer",
  "restic_backup",
  "restic_forget",
  "restic_check",
  "restic_prune",
];

/** scheduleKinds are the kinds a schedule may run: every editable kind, never a restore. */
export const scheduleKinds: EditableKind[] = editableKinds;

/** isEditableKind reports whether a job kind can be edited (and so run again or scheduled). */
export function isEditableKind(kind: string | undefined): kind is EditableKind {
  return editableKinds.includes(kind as EditableKind);
}

/** emptyDefinition is a blank definition of a kind, on a repository if given. */
export function emptyDefinition(
  kind: EditableKind = "rclone_transfer",
  repositoryId = "",
): JobDefinitionState {
  switch (kind) {
    case "restic_backup":
      return { kind, backup: { ...emptyBackup(), repositoryId } };
    case "restic_forget":
      return { kind, retention: { ...emptyRetentionJob(), repositoryId } };
    case "restic_check":
      return { kind, check: { ...emptyCheck(), repositoryId } };
    case "restic_prune":
      return { kind, prune: { ...emptyPrune(), repositoryId } };
    default:
      return { kind: "rclone_transfer", transfer: emptyTransfer() };
  }
}

/** definitionRepositoryId is the repository a definition works on ("" for transfers). */
export function definitionRepositoryId(s: JobDefinitionState): string {
  switch (s.kind) {
    case "restic_backup":
      return s.backup.repositoryId;
    case "restic_forget":
      return s.retention.repositoryId;
    case "restic_check":
      return s.check.repositoryId;
    case "restic_prune":
      return s.prune.repositoryId;
    default:
      return "";
  }
}

/** withKind switches the form to another kind, keeping the chosen repository. */
export function withKind(s: JobDefinitionState, kind: EditableKind): JobDefinitionState {
  return s.kind === kind ? s : emptyDefinition(kind, definitionRepositoryId(s));
}

/**
 * definitionState fills the form from a stored definition (a job or a
 * schedule). A kind the form cannot edit gives a blank transfer.
 */
export function definitionState(def: JobDefinition | undefined): JobDefinitionState {
  const op = def?.operation;
  switch (op?.case) {
    case "resticBackup":
      return { kind: "restic_backup", backup: backupState(op.value) };
    case "resticForget":
      return { kind: "restic_forget", retention: retentionState(op.value) };
    case "resticCheck":
      return { kind: "restic_check", check: checkState(op.value) };
    case "resticPrune":
      return { kind: "restic_prune", prune: pruneState(op.value) };
    default:
      return { kind: "rclone_transfer", transfer: transferState(def) };
  }
}

/** validateDefinition checks what the browser can check before sending. */
export function validateDefinition(s: JobDefinitionState): DefinitionErrors {
  switch (s.kind) {
    case "restic_backup":
      return validateBackup(s.backup);
    case "restic_forget":
      return validateRetention(s.retention);
    case "restic_check":
      return validateCheck(s.check);
    case "restic_prune":
      return validatePrune(s.prune);
    default:
      return validateTransfer(s.transfer);
  }
}

/** toDefinition builds the JobDefinition the API takes. */
export function toDefinition(s: JobDefinitionState): MessageInitShape<typeof JobDefinitionSchema> {
  switch (s.kind) {
    case "restic_backup":
      return { operation: { case: "resticBackup", value: toBackup(s.backup) } };
    case "restic_forget":
      return { operation: { case: "resticForget", value: toRetention(s.retention) } };
    case "restic_check":
      return { operation: { case: "resticCheck", value: toCheck(s.check) } };
    case "restic_prune":
      return { operation: { case: "resticPrune", value: toPrune(s.prune) } };
    default:
      return toTransferDefinition(s.transfer);
  }
}

/** isDryRun reports whether the definition only reports what it would do. */
export function isDryRun(s: JobDefinitionState): boolean {
  switch (s.kind) {
    case "restic_backup":
      return s.backup.dryRun;
    case "restic_forget":
      return s.retention.dryRun;
    case "restic_prune":
      return s.prune.dryRun;
    case "restic_check":
      return false;
    default:
      return s.transfer.dryRun;
  }
}

/**
 * forWorkerOS drops what a worker's OS cannot do: a file system snapshot
 * (VSS) is Windows-only. An unknown OS ("" for a label target) keeps it,
 * and the controller decides.
 */
export function forWorkerOS(s: JobDefinitionState, os: string | undefined): JobDefinitionState {
  if (s.kind !== "restic_backup" || !os || os.toLowerCase() === "windows") return s;
  return s.backup.useFsSnapshot
    ? { kind: s.kind, backup: { ...s.backup, useFsSnapshot: false } }
    : s;
}
