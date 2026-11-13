// The editable form of a two-way sync (rclone bisync, the "rclone_bisync"
// kind), its checks and its one-shot runs. definition.ts joins it with the
// other kinds into JobDefinitionState.
import { create, equals, type MessageInitShape } from "@bufbuild/protobuf";

import {
  JobDefinitionSchema,
  type FieldViolation,
  type Job,
  type JobDefinition,
  type RcloneBisyncDefinition,
  type RcloneBisyncDefinitionSchema,
} from "@/gen/syncr/v1/api_pb";
import {
  BisyncCompare,
  BisyncConflictLoser,
  BisyncConflictResolve,
  BisyncResyncMode,
  FilterAction,
  JobErrorCode,
} from "@/gen/syncr/v1/link_pb";

import { bytesToQuantity, isDecimal, quantityToBytes, type ByteUnit } from "./units";

// This module must not import definition.ts (which imports it); the shared
// types are repeated structurally instead.
interface Location {
  remoteId: string;
  path: string;
}

/** FilterRuleState is one filter rule as the form edits it. */
export interface FilterRuleState {
  action: FilterAction;
  pattern: string;
}

/** BisyncErrorKey names the parts of the two-way sync form an error can land on. */
export type BisyncErrorKey =
  | "path1Remote"
  | "path1Path"
  | "path2Remote"
  | "path2Path"
  | "maxDelete"
  | "conflicts"
  | "conflictSuffix"
  | "compare"
  | "resyncMode"
  | "filters"
  | "transfers"
  | "checkers"
  | "bwlimit"
  | "lowLevelRetries"
  | "options";

/** BisyncErrors are the two-way sync's problems, by field. */
export type BisyncErrors = Partial<Record<BisyncErrorKey, string>>;

/** CompareState says which properties tell that a file changed. */
export interface CompareState {
  size: boolean;
  modtime: boolean;
  checksum: boolean;
}

/**
 * BisyncState is the editable form of a two-way sync, shared by "Run a
 * job" and schedules. Numbers are kept as typed text. resync and force are
 * not here: they are set only by the job page's Resync now and Run once
 * anyway (withOneShot), never in the form.
 */
export interface BisyncState {
  /** path1 and path2 are rclone's Path1 and Path2: Folder A and Folder B. */
  path1: Location;
  path2: Location;
  /** maxDelete is the delete limit in percent; empty means 50. */
  maxDelete: string;
  checkAccess: boolean;
  conflictResolve: BisyncConflictResolve;
  conflictLoser: BisyncConflictLoser;
  conflictSuffix: string;
  compare: CompareState;
  resyncMode: BisyncResyncMode;
  transfers: string;
  checkers: string;
  /** bwlimit is the bandwidth limit as typed, in bwlimitUnit per second. */
  bwlimit: string;
  bwlimitUnit: ByteUnit;
  lowLevelRetries: string;
  createEmptySrcDirs: boolean;
  dryRun: boolean;
  filters: FilterRuleState[];
}

/** DEFAULT_MAX_DELETE is the delete limit of an unset max_delete_percent. */
export const DEFAULT_MAX_DELETE = 50;

/** MAX_BISYNC_PATH_BYTES bounds the two sides' paths together (jobkind's limit). */
export const MAX_BISYNC_PATH_BYTES = 200;

/** conflictSuffixPattern is jobkind's rule for conflict_suffix. */
export const conflictSuffixPattern = /^[A-Za-z0-9._-]{1,32}(,[A-Za-z0-9._-]{1,32})?$/;

/** defaultCompare is what an empty compare list means: size and modification time. */
const defaultCompare: CompareState = { size: true, modtime: true, checksum: false };

/** emptyBisync is a blank two-way sync with Syncr's defaults. */
export function emptyBisync(): BisyncState {
  return {
    path1: { remoteId: "", path: "" },
    path2: { remoteId: "", path: "" },
    maxDelete: "",
    checkAccess: false,
    conflictResolve: BisyncConflictResolve.NONE,
    conflictLoser: BisyncConflictLoser.NUM,
    conflictSuffix: "",
    compare: { ...defaultCompare },
    resyncMode: BisyncResyncMode.NEWER,
    transfers: "",
    checkers: "",
    bwlimit: "",
    bwlimitUnit: "MiB",
    lowLevelRetries: "",
    createEmptySrcDirs: false,
    dryRun: false,
    filters: [],
  };
}

const num = (v: number | undefined) => (v === undefined ? "" : String(v));

/** bisyncOf returns a definition's two-way sync, if it is one. */
export function bisyncOf(def: JobDefinition | undefined): RcloneBisyncDefinition | undefined {
  const op = def?.operation;
  return op?.case === "rcloneBisync" ? op.value : undefined;
}

/** bisyncState fills the form from a stored definition (a job or a schedule). */
export function bisyncState(b: RcloneBisyncDefinition | undefined): BisyncState {
  const base = emptyBisync();
  if (!b) return base;
  const o = b.options;
  const bw = bytesToQuantity(o?.bandwidthLimitBytesPerSecond, "MiB");
  const compare = o?.compare.length
    ? {
        size: o.compare.includes(BisyncCompare.SIZE),
        modtime: o.compare.includes(BisyncCompare.MODTIME),
        checksum: o.compare.includes(BisyncCompare.CHECKSUM),
      }
    : { ...defaultCompare };
  return {
    ...base,
    path1: { remoteId: b.path1?.remoteId ?? "", path: b.path1?.path ?? "" },
    path2: { remoteId: b.path2?.remoteId ?? "", path: b.path2?.path ?? "" },
    maxDelete: num(o?.maxDeletePercent),
    checkAccess: o?.checkAccess ?? false,
    conflictResolve: o?.conflictResolve || BisyncConflictResolve.NONE,
    conflictLoser: o?.conflictLoser || BisyncConflictLoser.NUM,
    conflictSuffix: o?.conflictSuffix ?? "",
    compare,
    resyncMode: o?.resyncMode || BisyncResyncMode.NEWER,
    transfers: num(o?.transfers),
    checkers: num(o?.checkers),
    bwlimit: bw.text,
    bwlimitUnit: bw.unit,
    lowLevelRetries: num(o?.lowLevelRetries),
    createEmptySrcDirs: o?.createEmptySrcDirs ?? false,
    dryRun: o?.dryRun ?? false,
    filters: (o?.filters ?? []).map((f) => ({ action: f.action, pattern: f.pattern })),
  };
}

const isAbsolute = (p: string) =>
  p.startsWith("/") || /^[A-Za-z]:[\\/]/.test(p) || p.startsWith("\\\\");

const isWindowsPath = (p: string) => /^[A-Za-z]:[\\/]/.test(p) || p.startsWith("\\\\");

/**
 * segments splits a location's path for comparison: on a remote at "/"
 * (a leading or trailing one does not count), on the worker at "/" or, for
 * a Windows path, at either slash and without regard to case.
 */
function segments(loc: Location): string[] {
  const p = loc.path.trim();
  const windows = loc.remoteId === "" && isWindowsPath(p);
  return (windows ? p.toLowerCase() : p)
    .split(windows ? /[\\/]+/ : /\/+/)
    .filter((s) => s !== "" && s !== ".");
}

/**
 * sidesOverlap reports whether two sides are the same folder or one is
 * inside the other: both on the same remote, or both on the worker (a
 * POSIX and a Windows path never overlap). A remote's root holds every
 * folder of that remote.
 */
export function sidesOverlap(a: Location, b: Location): boolean {
  if (a.remoteId !== b.remoteId) return false;
  if (a.remoteId === "" && isWindowsPath(a.path.trim()) !== isWindowsPath(b.path.trim()))
    return false;
  const x = segments(a);
  const y = segments(b);
  const n = Math.min(x.length, y.length);
  for (let i = 0; i < n; i++) if (x[i] !== y[i]) return false;
  return true;
}

const utf8 = new TextEncoder();

/**
 * pathBytes is about how long a side's path is for rclone's state file
 * names: its segments joined, so never longer than what the controller
 * counts (it rejects pairs over MAX_BISYNC_PATH_BYTES).
 */
function pathBytes(loc: Location): number {
  const p = loc.path.trim();
  const lead = loc.remoteId === "" ? (isWindowsPath(p) ? 0 : 1) : 0;
  return lead + utf8.encode(segments({ ...loc, path: p }).join("/")).length;
}

const intIn = (v: string, lo: number, hi: number) =>
  /^\d+$/.test(v) && Number(v) >= lo && Number(v) <= hi;

/**
 * validateBisync checks what the browser can check before sending, like
 * the kind's Check: absolute worker paths, sides that differ and do not
 * nest, their length together, the ranges, the conflict suffix, at least
 * one property to compare and the filter patterns.
 */
export function validateBisync(s: BisyncState): BisyncErrors {
  const e: BisyncErrors = {};
  for (const [loc, key, name] of [
    [s.path1, "path1Path", "Folder A"],
    [s.path2, "path2Path", "Folder B"],
  ] as const) {
    if (loc.remoteId === "" && !isAbsolute(loc.path.trim()))
      e[key] = `Enter an absolute path on the worker for ${name}, such as /srv/shared.`;
  }
  if (!e.path1Path && !e.path2Path) {
    if (sidesOverlap(s.path1, s.path2)) {
      e.path2Path =
        "Folder B must be another folder than Folder A, and neither may be inside the other.";
    } else if (pathBytes(s.path1) + pathBytes(s.path2) > MAX_BISYNC_PATH_BYTES) {
      e.path1Path = `The two paths together are too long for a two-way sync: rclone names its state files after them. Keep them under ${MAX_BISYNC_PATH_BYTES} characters together.`;
    }
  }
  if (s.maxDelete !== "" && !intIn(s.maxDelete, 1, 100))
    e.maxDelete = "Enter a whole percentage from 1 to 100, or leave it empty for 50%.";
  if (s.conflictSuffix.trim() !== "" && !conflictSuffixPattern.test(s.conflictSuffix.trim()))
    e.conflictSuffix =
      'Use one suffix, or two separated by a comma, each 1 to 32 letters, digits, ".", "_" or "-".';
  if (!s.compare.size && !s.compare.modtime && !s.compare.checksum)
    e.compare = "Choose at least one way to tell that a file changed.";
  if (s.transfers !== "" && !intIn(s.transfers, 1, 64))
    e.transfers = "Enter a whole number from 1 to 64.";
  if (s.checkers !== "" && !intIn(s.checkers, 1, 256))
    e.checkers = "Enter a whole number from 1 to 256.";
  if (s.lowLevelRetries !== "" && !intIn(s.lowLevelRetries, 0, 1000))
    e.lowLevelRetries = "Enter a whole number from 0 to 1000.";
  if (s.bwlimit !== "" && !isDecimal(s.bwlimit)) e.bwlimit = "Enter a number, zero or more.";
  if (s.filters.some((f) => /[\0\r\n]/.test(f.pattern)))
    e.filters = "A pattern must not contain line breaks.";
  return e;
}

const optInt = (v: string) => (v === "" ? undefined : Number(v));

/** toCompare lists the chosen properties; size and modification time alone is the default (empty). */
function toCompare(c: CompareState): BisyncCompare[] {
  if (c.size && c.modtime && !c.checksum) return [];
  return [
    ...(c.size ? [BisyncCompare.SIZE] : []),
    ...(c.modtime ? [BisyncCompare.MODTIME] : []),
    ...(c.checksum ? [BisyncCompare.CHECKSUM] : []),
  ];
}

/** cleanFilters drops empty rows and trims patterns. */
export function cleanFilters(filters: FilterRuleState[]): FilterRuleState[] {
  return filters
    .filter((f) => f.pattern.trim())
    .map((f) => ({ action: f.action, pattern: f.pattern.trim() }));
}

/** toBisync builds the RcloneBisyncDefinition the API takes (never resync or force). */
export function toBisync(s: BisyncState): MessageInitShape<typeof RcloneBisyncDefinitionSchema> {
  return {
    path1: { remoteId: s.path1.remoteId, path: s.path1.path.trim() },
    path2: { remoteId: s.path2.remoteId, path: s.path2.path.trim() },
    options: {
      maxDeletePercent: optInt(s.maxDelete),
      checkAccess: s.checkAccess,
      // A default is sent unset, as the API documents it.
      conflictResolve:
        s.conflictResolve === BisyncConflictResolve.NONE
          ? BisyncConflictResolve.UNSPECIFIED
          : s.conflictResolve,
      conflictLoser:
        s.conflictLoser === BisyncConflictLoser.NUM
          ? BisyncConflictLoser.UNSPECIFIED
          : s.conflictLoser,
      conflictSuffix: s.conflictSuffix.trim(),
      compare: toCompare(s.compare),
      resyncMode:
        s.resyncMode === BisyncResyncMode.NEWER ? BisyncResyncMode.UNSPECIFIED : s.resyncMode,
      transfers: optInt(s.transfers),
      checkers: optInt(s.checkers),
      bandwidthLimitBytesPerSecond: quantityToBytes({ text: s.bwlimit, unit: s.bwlimitUnit }),
      lowLevelRetries: optInt(s.lowLevelRetries),
      createEmptySrcDirs: s.createEmptySrcDirs,
      dryRun: s.dryRun,
      filters: cleanFilters(s.filters),
    },
  };
}

const optionFields: [RegExp, BisyncErrorKey][] = [
  [/\.max_delete_percent$/, "maxDelete"],
  [/\.conflict_suffix$/, "conflictSuffix"],
  [/\.conflict_(resolve|loser)$/, "conflicts"],
  [/\.compare(\[\d+\])?$/, "compare"],
  [/\.resync_mode$/, "resyncMode"],
  [/\.filters(\[\d+\].*)?$/, "filters"],
  [/\.transfers$/, "transfers"],
  [/\.checkers$/, "checkers"],
  [/\.bandwidth_limit_bytes_per_second$/, "bwlimit"],
  [/\.low_level_retries$/, "lowLevelRetries"],
];

/**
 * bisyncError places one API violation of a two-way sync on its field and
 * returns the key, or undefined when the field is not the two-way sync's.
 * Paths look like definition.rclone_bisync.path2 (sides that nest),
 * definition.rclone_bisync.path1.path or
 * definition.rclone_bisync.options.filters[0].pattern.
 */
export function bisyncError(v: FieldViolation): BisyncErrorKey | undefined {
  const m = /(?:^|\.)rclone_bisync(\..*)?$/.exec(v.field);
  if (!m) return undefined;
  const rest = m[1] ?? "";
  const side = /^\.path([12])(\.(remote_id|path))?$/.exec(rest);
  if (side) return `path${side[1]}${side[3] === "remote_id" ? "Remote" : "Path"}` as BisyncErrorKey;
  if (rest.startsWith(".options")) {
    for (const [re, key] of optionFields) if (re.test(rest)) return key;
  }
  return "options";
}

/**
 * filtersNeedResync reports whether saving after changes from saved to
 * now makes the pair's next run fail with RESYNC_REQUIRED: the same two
 * sides (another side is a new pair, which resyncs on its own) with other
 * filter rules (rclone remembers a hash of them).
 */
export function filtersNeedResync(saved: BisyncState, now: BisyncState): boolean {
  const same = (a: Location, b: Location) =>
    a.remoteId === b.remoteId && a.path.trim() === b.path.trim();
  if (!same(saved.path1, now.path1) || !same(saved.path2, now.path2)) return false;
  const a = cleanFilters(saved.filters);
  const b = cleanFilters(now.filters);
  return (
    a.length !== b.length ||
    a.some((f, i) => f.action !== b[i].action || f.pattern !== b[i].pattern)
  );
}

/** OneShot is a single-run override of a two-way sync. */
export type OneShot = "resync" | "force";

/**
 * withOneShot returns a copy of a two-way sync's definition for one run
 * with resync or force set (and the other cleared), or undefined if the
 * definition is not a two-way sync. A resync always runs for real, even
 * from a dry-run definition: a dry run cannot rebuild the pair's state,
 * and the pair would still need its resync. A run past the safety check
 * keeps the definition's dry run: it then reports what it would change,
 * and never deletes for real what was only ever tried dry.
 */
export function withOneShot(
  def: JobDefinition | undefined,
  what: OneShot,
): MessageInitShape<typeof JobDefinitionSchema> | undefined {
  const b = bisyncOf(def);
  if (!def || !b) return undefined;
  const options = b.options && what === "resync" ? { ...b.options, dryRun: false } : b.options;
  return {
    ...def,
    operation: {
      case: "rcloneBisync",
      value: { ...b, options, resync: what === "resync", force: what === "force" },
    },
  };
}

/** sameLocation compares two sides of two-way syncs: the remote and the path. */
function sameLocation(a: Location | undefined, b: Location | undefined): boolean {
  const path = (p = "") => {
    const t = p.trim();
    return t.length > 1 ? t.replace(/[\\/]+$/, "") : t;
  };
  return (a?.remoteId ?? "") === (b?.remoteId ?? "") && path(a?.path) === path(b?.path);
}

/**
 * samePair reports whether two definitions are two-way syncs of the same
 * two locations, Folder A and Folder B in the same order: the same pair on
 * a worker, whatever their options.
 */
export function samePair(a: JobDefinition | undefined, b: JobDefinition | undefined): boolean {
  const x = bisyncOf(a);
  const y = bisyncOf(b);
  return !!x && !!y && sameLocation(x.path1, y.path1) && sameLocation(x.path2, y.path2);
}

/** sameSettings reports whether two definitions are equal, ignoring a two-way sync's resync and force. */
export function sameSettings(a: JobDefinition | undefined, b: JobDefinition | undefined): boolean {
  if (!a || !b) return false;
  const plain = (d: JobDefinition) => {
    const x = bisyncOf(d);
    return x
      ? create(JobDefinitionSchema, {
          ...d,
          operation: { case: "rcloneBisync", value: { ...x, resync: false, force: false } },
        })
      : d;
  };
  return equals(JobDefinitionSchema, plain(a), plain(b));
}

/** ScheduleLike is what planRecovery reads of a schedule. */
export interface ScheduleLike {
  id: string;
  definition?: JobDefinition;
  target?: { target: { case: string | undefined; value?: unknown } };
}

/** scheduleWorker returns the worker a schedule targets, or "" for a label. */
export function scheduleWorker(s: ScheduleLike | undefined): string {
  const t = s?.target?.target;
  return t?.case === "workerId" && typeof t.value === "string" ? t.value : "";
}

/**
 * RecoveryBlock says why a failed two-way sync offers no action:
 *
 * - newer: a newer job of the same pair exists on the worker (jobId, when
 *   it was found), so that job's outcome is the one to act on;
 * - scheduleChanged: on the job's page, its schedule now has other
 *   settings or another worker, so the schedule's page decides;
 * - settingsChanged: on the schedule's page, Run once anyway after a run
 *   with other settings than the schedule's now: its counts are not
 *   those of the settings that would run;
 * - pending: not known yet (loading).
 */
export type RecoveryBlock =
  | { why: "newer"; jobId?: string }
  | { why: "scheduleChanged"; scheduleId: string }
  | { why: "settingsChanged" }
  | { why: "pending" };

/** RecoveryRun is what a recovery action runs: a definition, once, on a worker. */
export interface RecoveryRun {
  definition: JobDefinition;
  workerId: string;
  /** from says whose settings they are: the failed job's or its schedule's. */
  from: "job" | "schedule";
}

/** RecoveryPlan is what a failed two-way sync offers, and what its action would run. */
export interface RecoveryPlan {
  recovery: Recovery;
  /** run is set when the action is offered. */
  run?: RecoveryRun;
  /** block says why it is not, for resync and force. */
  block?: RecoveryBlock;
}

/** RecoveryInput is what planRecovery decides from. */
export interface RecoveryInput {
  /** job is the failed run. */
  job: Job;
  /** from is the page that asks: the job's or its schedule's. */
  from: "job" | "schedule";
  /**
   * schedule is the job's schedule: the page's on the schedule page; on the
   * job page the one the job came from, when it still exists.
   */
  schedule?: ScheduleLike;
  /**
   * newer are the two-way sync jobs created on the worker the action would
   * run on since job (as ListJobs returns them, newest first), and
   * complete whether that is all of them; undefined while not known.
   */
  newer?: { jobs: Job[]; complete: boolean };
}

const createdMs = (j: Job) =>
  Number(j.createdAt?.seconds ?? 0n) * 1000 + Math.floor((j.createdAt?.nanos ?? 0) / 1e6);

/**
 * planRecovery decides what a failed two-way sync offers (recoveryFor) and
 * which definition on which worker its action runs, so that an action
 * never runs settings that a newer run or a changed schedule superseded:
 *
 * - On the schedule's page, the action runs the schedule's current
 *   definition on the worker it targets now. Resync now is safe with any
 *   settings (a merge); Run once anyway only when the failed run had those
 *   same settings on that worker, since the dialog quotes its counts.
 * - On a job's page, the action runs that job's definition on its worker,
 *   and only while it is current: a scheduled job only while its schedule
 *   still has the same settings and worker (else the schedule's page
 *   decides); a job whose schedule was deleted, or one run by hand, as it
 *   is.
 * - Either way, not when a newer job of the same pair exists on that
 *   worker (one created after the failed run, the pair's resync included):
 *   its outcome is the one that counts.
 *
 * It returns undefined for a job that offers nothing (not a failed two-way
 * sync with one of the codes of recoveryFor).
 */
export function planRecovery(i: RecoveryInput): RecoveryPlan | undefined {
  const recovery = bisyncOf(i.job.definition) ? recoveryFor(i.job.error?.code) : undefined;
  if (!recovery) return undefined;
  if (recovery === "hint") return { recovery };
  const target = scheduleWorker(i.schedule);
  const current =
    !!i.schedule &&
    sameSettings(i.job.definition, i.schedule.definition) &&
    i.job.workerId === target;
  let run: RecoveryRun;
  if (i.from === "schedule") {
    if (!i.schedule?.definition || !target || !bisyncOf(i.schedule.definition)) return { recovery };
    if (recovery === "force" && !current) return { recovery, block: { why: "settingsChanged" } };
    run = { definition: i.schedule.definition, workerId: target, from: "schedule" };
  } else {
    if (i.schedule && !current)
      return { recovery, block: { why: "scheduleChanged", scheduleId: i.schedule.id } };
    run = { definition: i.job.definition!, workerId: i.job.workerId, from: "job" };
  }
  if (!i.newer) return { recovery, block: { why: "pending" } };
  const since = createdMs(i.job);
  const newer = i.newer.jobs.find(
    (j) =>
      j.id !== i.job.id &&
      j.workerId === run.workerId &&
      createdMs(j) > since &&
      samePair(j.definition, run.definition),
  );
  if (newer) return { recovery, block: { why: "newer", jobId: newer.id } };
  if (!i.newer.complete) return { recovery, block: { why: "newer" } };
  return { recovery, run };
}

/**
 * withoutOneShot returns a definition without a two-way sync's resync and
 * force, so "Run again" repeats the pair's normal run; other kinds are
 * returned as they are.
 */
export function withoutOneShot(
  def: JobDefinition | undefined,
): MessageInitShape<typeof JobDefinitionSchema> | undefined {
  const b = bisyncOf(def);
  if (!def || !b || (!b.resync && !b.force)) return def;
  return {
    ...def,
    operation: { case: "rcloneBisync", value: { ...b, resync: false, force: false } },
  };
}

/**
 * Recovery is what a failed two-way sync offers: a resync (RESYNC_REQUIRED),
 * one run past the safety checks (SAFETY_ABORT), or only a hint
 * (SIDE_UNAVAILABLE, which no override passes).
 */
export type Recovery = "resync" | "force" | "hint";

/** recoveryFor returns the recovery of an error code, or undefined for other codes. */
export function recoveryFor(code: JobErrorCode | undefined): Recovery | undefined {
  switch (code) {
    case JobErrorCode.RESYNC_REQUIRED:
      return "resync";
    case JobErrorCode.SAFETY_ABORT:
      return "force";
    case JobErrorCode.SIDE_UNAVAILABLE:
      return "hint";
    default:
      return undefined;
  }
}

/** resyncModes are the choices of which copy a resync keeps, the default first. */
export const resyncModes: { value: BisyncResyncMode; label: string }[] = [
  { value: BisyncResyncMode.NEWER, label: "The newer copy" },
  { value: BisyncResyncMode.OLDER, label: "The older copy" },
  { value: BisyncResyncMode.LARGER, label: "The larger copy" },
  { value: BisyncResyncMode.SMALLER, label: "The smaller copy" },
  { value: BisyncResyncMode.PATH1, label: "Folder A's copy" },
  { value: BisyncResyncMode.PATH2, label: "Folder B's copy" },
];

/** resyncModeLabel names what a resync keeps, in lower case: "the newer copy". */
export function resyncModeLabel(m: BisyncResyncMode | undefined): string {
  const v = m || BisyncResyncMode.NEWER;
  return (resyncModes.find((x) => x.value === v)?.label ?? resyncModes[0].label).replace(
    /^The /,
    "the ",
  );
}

/** conflictResolutions are the choices of a conflict's winner, the default first. */
export const conflictResolutions: { value: BisyncConflictResolve; label: string }[] = [
  { value: BisyncConflictResolve.NONE, label: "No winner: keep both versions" },
  { value: BisyncConflictResolve.NEWER, label: "The newer version wins" },
  { value: BisyncConflictResolve.OLDER, label: "The older version wins" },
  { value: BisyncConflictResolve.LARGER, label: "The larger version wins" },
  { value: BisyncConflictResolve.SMALLER, label: "The smaller version wins" },
  { value: BisyncConflictResolve.PATH1, label: "Folder A's version wins" },
  { value: BisyncConflictResolve.PATH2, label: "Folder B's version wins" },
];

/**
 * conflictsSummary says in a few words how a definition keeps the files
 * changed on both sides: "both versions kept, numbered (.conflict1, .conflict2…)".
 */
export function conflictsSummary(b: RcloneBisyncDefinition | undefined): string {
  const o = b?.options;
  const suffix = (o?.conflictSuffix || "conflict").split(",");
  const pathname = o?.conflictLoser === BisyncConflictLoser.PATHNAME;
  const two = suffix.length > 1;
  const names = pathname
    ? two
      ? `.${suffix[0]} for Folder A's, .${suffix[1]} for Folder B's`
      : `.${suffix[0]}1 for Folder A's, .${suffix[0]}2 for Folder B's`
    : two
      ? `.${suffix[0]}1… for Folder A's, .${suffix[1]}1… for Folder B's`
      : `.${suffix[0]}1, .${suffix[0]}2…`;
  const resolve = o?.conflictResolve || BisyncConflictResolve.NONE;
  if (resolve === BisyncConflictResolve.NONE) return `both versions kept, renamed (${names})`;
  const winner = conflictResolutions.find((x) => x.value === resolve)?.label.toLowerCase() ?? "";
  return `${winner}; the other is kept, renamed (${names})`;
}

/** bisyncOptionSummary lists the options of a two-way sync that differ from the defaults. */
export function bisyncOptionSummary(b: RcloneBisyncDefinition | undefined): string {
  const o = b?.options;
  const parts: string[] = [];
  parts.push(`delete limit ${o?.maxDeletePercent ?? DEFAULT_MAX_DELETE}%`);
  if (o?.checkAccess) parts.push("checks access (RCLONE_TEST)");
  if (o?.compare.includes(BisyncCompare.CHECKSUM)) parts.push("compares checksums");
  else if (o?.compare.length === 1 && o.compare[0] === BisyncCompare.SIZE)
    parts.push("compares size only");
  if (o?.dryRun) parts.push("dry run");
  if (o?.createEmptySrcDirs) parts.push("empty folders");
  if (o?.transfers) parts.push(`${o.transfers} transfers`);
  if (o?.checkers) parts.push(`${o.checkers} checkers`);
  if (o?.filters.length)
    parts.push(`${o.filters.length} ${o.filters.length === 1 ? "filter" : "filters"}`);
  return parts.join(", ");
}
