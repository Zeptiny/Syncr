import type { MessageInitShape } from "@bufbuild/protobuf";

import type { Location } from "@/components/app/location-editor";
import type { FieldViolation, JobDefinition, JobDefinitionSchema } from "@/gen/syncr/v1/api_pb";
import { FilterAction, RcloneTransferMode } from "@/gen/syncr/v1/link_pb";
import { toNumber } from "@/lib/format";

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
 * DefinitionState is the editable form of a transfer job definition, shared
 * by "Run a job" and schedules. Numbers are kept as typed text.
 */
export interface DefinitionState {
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

/** DefinitionErrors are the definition's problems, by form section. */
export type DefinitionErrors = Partial<
  Record<"sourceRemote" | "sourcePath" | "destRemote" | "destPath" | "options", string>
>;

/** emptyDefinition is a blank copy definition. */
export function emptyDefinition(): DefinitionState {
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

/** definitionState fills the form from a stored definition (a job or a schedule). */
export function definitionState(def: JobDefinition | undefined): DefinitionState {
  const base = emptyDefinition();
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

function isAbsolute(p: string): boolean {
  return p.startsWith("/") || /^[A-Za-z]:[\\/]/.test(p) || p.startsWith("\\\\");
}

/** validateDefinition checks what the browser can check before sending. */
export function validateDefinition(s: DefinitionState): DefinitionErrors {
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

/** toDefinition builds the JobDefinition the API takes. */
export function toDefinition(s: DefinitionState): MessageInitShape<typeof JobDefinitionSchema> {
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
 * like definition.rclone_transfer.source.path.
 */
export function definitionError(v: FieldViolation, errors: DefinitionErrors): boolean {
  const f = v.field;
  if (f.includes("source"))
    errors[f.endsWith("remote_id") ? "sourceRemote" : "sourcePath"] = v.message;
  else if (f.includes("destination"))
    errors[f.endsWith("remote_id") ? "destRemote" : "destPath"] = v.message;
  else if (f.includes("options")) errors.options = v.message;
  else return false;
  return true;
}
