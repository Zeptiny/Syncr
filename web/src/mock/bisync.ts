/**
 * Two-way sync part of the dev mock (VITE_SYNCR_MOCK=1) and of the test
 * APIs, matching the dev seed (internal/devseed/bisync_data.go): the
 * "Cloudflare R2" remote, the schedule "Shared folder ⇄ R2" on nas-01 with
 * about three days of runs (the first run's resync, a run with two
 * conflicts, one that found Folder A empty, one stopped by an unreadable
 * file that the next run went on from, one that needed a resync after the
 * filters changed and the resync run by hand, and in the last hours runs
 * stopped by the delete limit), the first run of a new pair on
 * backup-host whose folder is empty (it asks for a resync), the workers
 * that run two-way syncs, and the controller's checks of a two-way sync
 * (RunJob and schedules).
 */
import { create } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromMs } from "@bufbuild/protobuf/wkt";

import {
  JobDefinitionSchema,
  JobDetailSchema,
  JobRemoteRefSchema,
  JobSchema,
  JobStatus,
  type Job,
  type JobDefinition,
  type JobRemoteRef,
  type Worker,
} from "@/gen/syncr/v1/api_pb";
import {
  BisyncResyncReason,
  FilterAction,
  JobErrorCode,
  JobOutputSchema,
  RcloneBisyncOutputSchema,
  type RcloneBisyncOutput,
} from "@/gen/syncr/v1/link_pb";
import { RemoteSchema, type Remote } from "@/gen/syncr/v1/remotes_pb";
import {
  MissedRunAction,
  OverlapPolicy,
  ScheduleRunSchema,
  ScheduleRunStatus,
  ScheduleRunTrigger,
  ScheduleSchema,
  type Schedule,
  type ScheduleRun,
} from "@/gen/syncr/v1/schedules_pb";
import { NotificationEvent } from "@/gen/syncr/v1/notify_pb";
import * as fx from "@/test/fixtures";

import { nextRuns, parseCron } from "./cron";

const SEC = 1000;
const MIN = 60 * SEC;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;
const KiB = 1024;
const MiB = 1024 * KiB;
const GiB = 1024 * MiB;

/** FEATURE and KIND are what a worker that runs two-way syncs reports. */
export const FEATURE = "rclone.bisync";
export const KIND = "rclone_bisync";

/** ids of the two-way sync demo objects. */
export const ids = {
  r2: "9e7d6c5b-2a1f-4b3c-8d9e-0f1a2b3c4d71",
  schedule: "4b8f0d2a-6c1e-4f3a-9d5b-7e2c1a0f3b71",
};

/** Names and paths of the demo, as the dev seed has them. */
export const SCHEDULE_NAME = "Shared folder ⇄ R2";
export const LOCAL_PATH = "/srv/shared";
export const REMOTE_NAME = "Cloudflare R2";
export const REMOTE_PATH = "shared";
export const TITLE = `Two-way sync ${LOCAL_PATH} ⇄ ${REMOTE_NAME}`;
/** NEW_PATH is the empty folder of the new pair on backup-host (with the same R2 path). */
export const NEW_PATH = "/backup/shared";
export const NEW_TITLE = `Two-way sync ${NEW_PATH} ⇄ ${REMOTE_NAME}`;
const CRON = "0 */2 * * *";
const TIMEZONE = "Europe/Berlin";
const FILES = 1268;

/** Messages of the failed runs, as the worker words them. */
export const messages = {
  sideUnavailable:
    "Folder A (path1) is empty; refusing to sync it with the other side (is the drive mounted and the path right?)",
  resyncRequired:
    "a resync is required: filters file has changed; run it once with resync to rebuild the pair's state from both sides as they are now",
  criticalError: `the two-way sync stopped with a critical error: failed to open source object: open ${LOCAL_PATH}/scans/2026-11-contract.pdf: permission denied; fix the cause and run it again: the next run goes on from the pair's last good state, without a resync`,
  emptyFolder:
    "Folder A (path1) is empty; a first run does not fill an empty folder on its own (a drive that is not mounted looks the same). If this is a new machine, run it once with resync to fill it from Folder B (path2); if the folder is on a drive, mount it first",
  safetyAbort: `safety check stopped the two-way sync: too many deletes (>50%, 731 of 1268) on Path1 "${LOCAL_PATH}/"; if these changes are intended, run it once with force`,
  conflictWarning:
    '2 files changed on both sides; both versions were kept, renamed with the suffix "conflict"',
};

/** conflicts are the files changed on both sides in the run with conflicts. */
export const conflicts = ["budget-2026.xlsx", "notes/meeting-notes.md"];

/** r2Remote is the S3 remote on Cloudflare R2 the shared folder syncs with. */
export function r2Remote(now: number): Remote {
  return create(RemoteSchema, {
    id: ids.r2,
    name: REMOTE_NAME,
    backendType: "s3",
    options: {
      provider: "Cloudflare",
      endpoint: "https://4f0c1e2d3b4a59687766554433221100.r2.cloudflarestorage.com",
      region: "auto",
      access_key_id: "syncr-demo-r2",
      bucket: "ada-shared",
    },
    secretsSet: ["secret_access_key"],
    createdAt: timestampFromMs(now - 4 * DAY),
    updatedAt: timestampFromMs(now - 4 * DAY),
    version: 1n,
  });
}

/** Tree is a mock folder: names to sub-folders or file sizes in bytes. */
interface Tree {
  [name: string]: Tree | number;
}

/** sharedTree is the shared folder's content, the same on both sides, with the kept conflict versions. */
export function sharedTree(): Tree {
  return {
    "budget-2026.xlsx": 420 * KiB,
    "budget-2026.xlsx.conflict1": 10 * KiB,
    "budget-2026.xlsx.conflict2": 11 * KiB,
    "report.docx": 240 * KiB,
    notes: {
      "meeting-notes.md": 12 * KiB,
      "meeting-notes.md.conflict1": 10 * KiB,
      "meeting-notes.md.conflict2": 11 * KiB,
      "packing-list.md": 3 * KiB,
    },
    scans: { "2026-10-invoice.pdf": 640 * KiB, "2026-11-contract.pdf": 2.3 * MiB },
    photos: { "2026": { "11": { "IMG_2201.jpg": 3.2 * MiB, "IMG_2202.jpg": 3.1 * MiB } } },
  };
}

/**
 * applyWorkers makes the up-to-date workers run two-way syncs (the ones
 * the controller calls outdated stay without it).
 */
export function applyWorkers(workers: Worker[]): void {
  for (const w of workers) {
    if (!w.info || w.outdated || w.info.features.length === 0) continue;
    if (!w.info.features.includes(FEATURE)) w.info.features.push(FEATURE);
    if (!w.info.jobKinds.includes(KIND)) w.info.jobKinds.splice(1, 0, KIND);
  }
}

/**
 * definition is the demo pair's definition; oneShot sets resync or force
 * for one run, and path1 another Folder A (the new pair's).
 */
export function definition(oneShot?: "resync" | "force", path1 = LOCAL_PATH): JobDefinition {
  return create(JobDefinitionSchema, {
    operation: {
      case: "rcloneBisync",
      value: {
        path1: { remoteId: "", path: path1 },
        path2: { remoteId: ids.r2, path: REMOTE_PATH },
        options: {
          createEmptySrcDirs: true,
          filters: [
            { action: FilterAction.EXCLUDE, pattern: "*.tmp" },
            { action: FilterAction.EXCLUDE, pattern: ".~lock.*#" },
          ],
        },
        resync: oneShot === "resync",
        force: oneShot === "force",
      },
    },
  });
}

const r2Ref = (): JobRemoteRef =>
  create(JobRemoteRefSchema, { id: ids.r2, name: REMOTE_NAME, backendType: "s3" });

/** details summarizes an output as the controller does (jobkind Bisync.Summarize). */
export function details(out: RcloneBisyncOutput): { label: string; value: string }[] {
  const d: { label: string; value: string }[] = [];
  const resync = out.resyncReason !== BisyncResyncReason.UNSPECIFIED;
  if (out.resyncReason === BisyncResyncReason.FIRST_RUN)
    d.push({ label: "Resync", value: "First run" });
  if (out.resyncReason === BisyncResyncReason.REQUESTED)
    d.push({ label: "Resync", value: "Requested" });
  if (out.conflicts > 0n) d.push({ label: "Conflicts", value: out.conflicts.toLocaleString("en") });
  else if (!resync) d.push({ label: "Conflicts", value: "None" });
  if (out.dryRun) d.push({ label: "Dry run", value: "Nothing was changed" });
  return d;
}

function output(reason: BisyncResyncReason, paths: string[] = []): RcloneBisyncOutput {
  return create(RcloneBisyncOutputSchema, {
    resyncReason: reason,
    conflicts: BigInt(paths.length),
    conflictPaths: paths,
  });
}

/** describe names a definition's job like the controller (jobkind Bisync.Describe). */
export function describe(
  def: JobDefinition | undefined,
  remotes: Remote[],
): { title: string; remotes: JobRemoteRef[] } | undefined {
  const op = def?.operation;
  if (op?.case !== "rcloneBisync") return undefined;
  const b = op.value;
  const refs: JobRemoteRef[] = [];
  const side = (loc: { remoteId: string; path: string } | undefined) => {
    if (!loc?.remoteId) return loc?.path ?? "";
    const r = remotes.find((x) => x.id === loc.remoteId);
    if (!refs.some((x) => x.id === loc.remoteId))
      refs.push(
        create(JobRemoteRefSchema, {
          id: loc.remoteId,
          name: r?.name ?? "?",
          backendType: r?.backendType ?? "",
        }),
      );
    return [r?.name ?? "?", loc.path].filter(Boolean).join(" ");
  };
  const a = side(b.path1);
  const z = side(b.path2);
  let title = `Two-way sync ${a} ⇄ ${z}`;
  if (b.resync) title += " · Resync";
  if (b.force) title += " · Delete limit ignored";
  return { title, remotes: refs };
}

/** finish gives a two-way sync that the mock worker completed its output. */
export function finish(j: Job): void {
  const op = j.definition?.operation;
  if (op?.case !== "rcloneBisync") return;
  const out = output(
    op.value.resync ? BisyncResyncReason.REQUESTED : BisyncResyncReason.UNSPECIFIED,
  );
  out.dryRun = op.value.options?.dryRun ?? false;
  j.output = create(JobOutputSchema, { output: { case: "rcloneBisync", value: out } });
  j.details = details(out).map((d) => create(JobDetailSchema, d));
}

/** Violation is one field problem, as the controller reports it. */
export interface Violation {
  field: string;
  message: string;
}

const FIELD = "definition.rclone_bisync";
const absolute = (p: string) =>
  p.startsWith("/") || /^[A-Za-z]:[\\/]/.test(p) || p.startsWith("\\\\");
const trimmed = (p: string) => p.replace(/^\/+|\/+$/g, "");

/**
 * check returns the problems of a two-way sync like the API: worker paths
 * that are not absolute, sides that are equal or nested, the delete limit,
 * the conflict suffix; for a schedule also resync and force (one run only)
 * and a label target (the pair's state stays on one worker).
 */
export function check(
  def: JobDefinition | undefined,
  p: { forSchedule: boolean; label?: boolean },
): Violation[] {
  const op = def?.operation;
  if (op?.case !== "rcloneBisync") return [];
  const b = op.value;
  const v: Violation[] = [];
  for (const [name, loc] of [
    ["path1", b.path1],
    ["path2", b.path2],
  ] as const) {
    if (!loc?.remoteId && !absolute(loc?.path ?? ""))
      v.push({
        field: `${FIELD}.${name}.path`,
        message: 'must be an absolute path on the worker, e.g. "/srv/data" or "C:\\Data"',
      });
  }
  if (!v.length && b.path1?.remoteId === b.path2?.remoteId) {
    const x = trimmed(b.path1?.path ?? "");
    const y = trimmed(b.path2?.path ?? "");
    if (x === y || x === "" || y === "" || x.startsWith(`${y}/`) || y.startsWith(`${x}/`))
      v.push({
        field: `${FIELD}.path2`,
        message: "must differ from path1, and neither may be inside the other",
      });
  }
  const o = b.options;
  if (o?.maxDeletePercent !== undefined && (o.maxDeletePercent < 1 || o.maxDeletePercent > 100))
    v.push({ field: `${FIELD}.options.max_delete_percent`, message: "must be between 1 and 100" });
  if (o?.conflictSuffix && !/^[A-Za-z0-9._-]{1,32}(,[A-Za-z0-9._-]{1,32})?$/.test(o.conflictSuffix))
    v.push({
      field: `${FIELD}.options.conflict_suffix`,
      message:
        'must be one suffix, or two separated by a comma, each 1 to 32 letters, digits, ".", "_" or "-"',
    });
  if (p.forSchedule) {
    for (const f of ["resync", "force"] as const)
      if (b[f]) v.push({ field: `${FIELD}.${f}`, message: "can only be set for a single run" });
    if (p.label)
      v.push({
        field: "target.worker_label",
        message: "a two-way sync keeps its state on one worker; choose a worker",
      });
  }
  return v;
}

/** History is the demo pair's schedule, its runs (newest first) and its jobs. */
export interface History {
  schedule: Schedule;
  runs: ScheduleRun[];
  jobs: Job[];
}

function hexId(prefix: number, n: number, tail: string): string {
  const head = ((prefix + n * 0x9e3779b1) >>> 0).toString(16).padStart(8, "0");
  return `${head}-${tail}-${n.toString(16).padStart(12, "0")}`;
}

/**
 * history returns the schedule "Shared folder ⇄ R2" with about three days
 * of runs every two hours, as the dev seed: the first run's resync, quiet
 * runs, a run with two conflicts, one that found Folder A empty
 * (SIDE_UNAVAILABLE), one stopped by an unreadable file
 * (EXECUTION_FAILED; the next run went on from the last good state), the
 * last one at or before 7 hours ago needing a resync after the filters
 * changed (RESYNC_REQUIRED) followed 25 minutes later by the resync run by
 * hand, and in the last 4 hours runs stopped by the delete limit
 * (SAFETY_ABORT), the newest among them. Besides, 40 minutes ago, the
 * first run of a new pair on backup-host, run by hand, found its folder
 * empty and asks for a resync (RESYNC_REQUIRED, rclone not started).
 */
export function history(now: number): History {
  const expr = parseCron(CRON, TIMEZONE);
  const start = now - (3 * DAY + 4 * HOUR);
  const fires = nextRuns(expr, start, 60, now - 35 * SEC);
  const resyncAt = fires.filter((t) => t <= now - 7 * HOUR).at(-1) ?? fires[0];
  const runs: ScheduleRun[] = [];
  const jobs: Job[] = [];
  const base = { ownerId: fx.ids.admin, workerId: fx.ids.nas, kind: KIND, title: TITLE };
  const job = (
    n: number,
    at: number,
    o: {
      runMs: number;
      status: JobStatus;
      bytes: number;
      files: number;
      error?: [JobErrorCode, string, number];
      out?: RcloneBisyncOutput;
      warnings?: string[];
      oneShot?: "resync";
      /** newPair: the new pair's first run on backup-host, run by hand. */
      newPair?: boolean;
    },
  ): Job => {
    const started = at + 4 * SEC;
    const manual = !!o.oneShot || !!o.newPair;
    const j = create(JobSchema, {
      ...base,
      id: hexId(0xb15c0000, n, "8a6b-4c5d-8e9f"),
      workerId: o.newPair ? fx.ids.backupHost : base.workerId,
      title: o.newPair ? NEW_TITLE : o.oneShot ? `${TITLE} · Resync` : TITLE,
      status: o.status,
      createdAt: timestampFromMs(at + SEC),
      dispatchedAt: timestampFromMs(started - 500),
      startedAt: timestampFromMs(started),
      finishedAt: timestampFromMs(started + o.runMs),
      exitCode: o.error && o.error[2] >= 0 ? o.error[2] : o.error ? undefined : 0,
      stats: {
        bytes: BigInt(Math.round(o.bytes)),
        totalBytes: BigInt(Math.round(o.bytes)),
        files: BigInt(o.files),
        totalFiles: o.newPair ? 0n : o.error ? BigInt(FILES) : BigInt(o.files),
        checks: BigInt(o.newPair || o.error?.[0] === JobErrorCode.SIDE_UNAVAILABLE ? 0 : 2 * FILES),
        deletes: o.error ? 0n : BigInt(n % 4),
        errors: o.error && !o.newPair ? 1n : 0n,
        elapsed: durationFromMs(o.runMs),
      },
      error: o.error ? { code: o.error[0], message: o.error[1] } : undefined,
      warnings: o.warnings ?? [],
      definition: definition(o.oneShot, o.newPair ? NEW_PATH : LOCAL_PATH),
      remotes: [r2Ref()],
      output: o.out ? { output: { case: "rcloneBisync", value: o.out } } : undefined,
      details: o.out ? details(o.out) : [],
      scheduleId: manual ? "" : ids.schedule,
      schedule: manual ? undefined : { id: ids.schedule, name: SCHEDULE_NAME },
    });
    jobs.push(j);
    return j;
  };
  fires.forEach((at, n) => {
    let j: Job;
    if (at > now - 4 * HOUR) {
      j = job(n, at, {
        runMs: 14 * SEC,
        status: JobStatus.FAILED,
        bytes: 0,
        files: 0,
        error: [JobErrorCode.SAFETY_ABORT, messages.safetyAbort, 1],
      });
    } else if (at === resyncAt) {
      j = job(n, at, {
        runMs: 38 * SEC,
        status: JobStatus.FAILED,
        bytes: 0,
        files: 0,
        error: [JobErrorCode.RESYNC_REQUIRED, messages.resyncRequired, 7],
      });
    } else if (n === 0) {
      j = job(n, at, {
        runMs: 9 * MIN + 12 * SEC,
        status: JobStatus.SUCCEEDED,
        bytes: 3 * GiB + 410 * MiB,
        files: FILES - 14,
        out: output(BisyncResyncReason.FIRST_RUN),
      });
    } else if (n === 6) {
      j = job(n, at, {
        runMs: 41 * SEC,
        status: JobStatus.SUCCEEDED,
        bytes: 4.4 * MiB,
        files: 6,
        out: output(BisyncResyncReason.UNSPECIFIED, conflicts),
        warnings: [messages.conflictWarning],
      });
    } else if (n === 13) {
      j = job(n, at, {
        runMs: 6 * SEC,
        status: JobStatus.FAILED,
        bytes: 0,
        files: 0,
        error: [JobErrorCode.SIDE_UNAVAILABLE, messages.sideUnavailable, 7],
      });
    } else if (n === 20) {
      j = job(n, at, {
        runMs: 27 * SEC,
        status: JobStatus.FAILED,
        bytes: 0,
        files: 0,
        error: [JobErrorCode.EXECUTION_FAILED, messages.criticalError, 7],
      });
    } else {
      j = job(n, at, {
        runMs: (18 + ((n * 37) % 77)) * SEC,
        status: JobStatus.SUCCEEDED,
        bytes: (1 + ((n * 13) % 59)) * MiB,
        files: (n * 7) % 24,
        out: output(BisyncResyncReason.UNSPECIFIED),
      });
    }
    runs.push(
      create(ScheduleRunSchema, {
        id: hexId(0xe6f7c000, n, "8a6b-4c5d-8e9f"),
        scheduleId: ids.schedule,
        trigger: ScheduleRunTrigger.CRON,
        scheduledFor: timestampFromMs(at),
        deadline: timestampFromMs(at + HOUR),
        status: ScheduleRunStatus.LAUNCHED,
        workerId: fx.ids.nas,
        jobId: j.id,
        jobStatus: j.status,
        createdAt: timestampFromMs(at),
        updatedAt: timestampFromMs(at + 4 * SEC),
      }),
    );
  });
  // The resync run by hand, accepting the new filters.
  job(fires.length, resyncAt + 25 * MIN, {
    runMs: 2 * MIN + 47 * SEC,
    status: JobStatus.SUCCEEDED,
    bytes: 2.3 * MiB,
    files: 1,
    out: output(BisyncResyncReason.REQUESTED),
    oneShot: "resync",
  });
  // The new pair's first run, refused before rclone started.
  job(fires.length + 1, now - 40 * MIN, {
    runMs: 0,
    status: JobStatus.FAILED,
    bytes: 0,
    files: 0,
    error: [JobErrorCode.RESYNC_REQUIRED, messages.emptyFolder, -1],
    newPair: true,
  });
  runs.reverse();
  jobs.sort((a, b) => Number((b.createdAt?.seconds ?? 0n) - (a.createdAt?.seconds ?? 0n)));
  const next = nextRuns(expr, now, 1)[0];
  const schedule = create(ScheduleSchema, {
    id: ids.schedule,
    ownerId: fx.ids.admin,
    name: SCHEDULE_NAME,
    description: "Keep the NAS's shared folder and the R2 copy the laptop syncs with in step.",
    cron: CRON,
    timezone: TIMEZONE,
    target: { target: { case: "workerId", value: fx.ids.nas } },
    missedRun: { action: MissedRunAction.RUN_LATE, maxDelay: durationFromMs(HOUR) },
    overlap: OverlapPolicy.SKIP,
    enabled: true,
    definition: definition(),
    remotes: [r2Ref()],
    contacts: [
      {
        contactId: fx.ids.opsDiscord,
        events: [NotificationEvent.FAILED, NotificationEvent.MISSED],
        contactName: "Ops Discord",
        channelType: "discord",
      },
    ],
    nextRunAt: next ? timestampFromMs(next) : undefined,
    lastFiredAt: runs[0]?.scheduledFor,
    lastRun: runs[0],
    createdAt: timestampFromMs(start - HOUR),
    updatedAt: timestampFromMs(start - HOUR),
    version: 2n,
  });
  return { schedule, runs, jobs };
}
