/**
 * Backups of a remote in the dev mock (VITE_SYNCR_MOCK=1) and the test
 * APIs, matching the dev seed (internal/devseed/remote_backup_data.go):
 * what the workers report they can do (nas-01 mirrors and mounts,
 * backup-host only mirrors, office-pc neither, attic-nas and laptop
 * nothing), the repository "Nextcloud photos" whose snapshots are of the
 * Nextcloud remote, the schedule "Nightly Nextcloud photos backup" on
 * nas-01 with two weeks of runs (one failed on an empty folder), the runs
 * by hand (a mount of Storage box/Scans, an allowed empty Inbox that made
 * no snapshot, a mount on backup-host the controller refused), one mirror
 * running now (the mock only, for the live phases), and the controller's
 * checks of a backup of a remote.
 */
import { create } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";

import {
  JobSchema,
  JobStatus,
  type Job,
  type JobDefinition,
  type ResticBackupDefinition,
  type Worker,
} from "@/gen/syncr/v1/api_pb";
import {
  FilterAction,
  JobErrorCode,
  ResticRemoteSourceMethod,
  type JobOutput,
  type ResticBackupOutput,
} from "@/gen/syncr/v1/link_pb";
import type { Remote } from "@/gen/syncr/v1/remotes_pb";
import { RepositorySchema } from "@/gen/syncr/v1/restic_pb";
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
import { formatBytes } from "@/lib/format";
import { methodLabel, REMOTE_TAG_PREFIX } from "@/lib/remote-sources";
import { FEATURE_REMOTE_SOURCE } from "@/lib/workers";
import * as fx from "@/test/fixtures";

import { snapshots, type RepoState, type Tree } from "./m5";

const SEC = 1000;
const MIN = 60 * SEC;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;
const KiB = 1024;
const MiB = 1024 * KiB;
const GiB = 1024 * MiB;
const ts = timestampFromMs;

type Violation = { field: string; message: string };

/** FEATURE is the link feature of workers that back up remotes. */
export const FEATURE = FEATURE_REMOTE_SOURCE;

/** ids of the demo objects. */
export const ids = {
  repository: "7c3a9e1f-5b2d-4e8a-9f1c-0d2e3f4a5b81",
  schedule: "4b8f0d2a-6c1e-4f3a-9d5b-7e2c1a0f3b81",
  mountJob: "a7c0b0a1-1c0d-4e9f-8a7b-0000000000b1",
  emptyJob: "a7c0b0a1-1c0d-4e9f-8a7b-0000000000b2",
  refusedJob: "a7c0b0a1-1c0d-4e9f-8a7b-0000000000b3",
  runningJob: "a7c0b0a1-1c0d-4e9f-8a7b-0000000000b4",
};

/** Names and messages of the demo, as the dev seed has them. */
export const REPOSITORY_NAME = "Nextcloud photos";
export const SCHEDULE_NAME = "Nightly Nextcloud photos backup";
/** MIRROR_PATH is the snapshots' internal folder (a fixed fake key, as the seed's). */
export const MIRROR_PATH = "/var/lib/syncr/sources/mirror/5d1e0c3b9a7f4e2d8c6b0a1f3e5d7c9b";
export const NO_SNAPSHOT = "No snapshot: the remote folder was empty";
export const messages = {
  sourceEmpty:
    'the folder "Photos" on the remote is empty, so nothing was backed up (an emptied or wrong folder would push real snapshots out of retention); allow an empty folder in the backup\'s options to back it up anyway',
  emptyWarning:
    'the folder "Inbox" on the remote is empty and restic makes no empty snapshot, so no snapshot was made',
  cannotMount: 'worker "backup-host" cannot mount a remote: /dev/fuse is missing',
};

/** The controller's messages (step B's "API facts for the web"). */
export const api = {
  singleWorker:
    "a backup of a remote keeps its mirror or mount folder on one worker; choose a worker",
  local: "this remote is a folder on a worker; back up that folder instead",
  mirrorOnly: "is only available with the mirror method",
  tags: "must have at most 31 tags when backing up a folder of a remote (Syncr adds one)",
  tagPrefix: `must not start with "${REMOTE_TAG_PREFIX}": Syncr adds that tag itself`,
  paths: "must be empty when backing up a folder of a remote (remote_source)",
  excludes:
    "cannot be used when backing up a folder of a remote: leave things out with its filters instead",
  notWith: "cannot be used when backing up a folder of a remote",
};

/** tag is the tag Syncr adds to a snapshot of a remote. */
export const tag = (remoteId: string) => `${REMOTE_TAG_PREFIX}${remoteId}`;

/**
 * applyWorkers gives the demo workers their remote-backup reports: nas-01
 * mirrors and mounts, backup-host mirrors ("/dev/fuse is missing"),
 * office-pc neither; the others report nothing (older releases).
 */
export function applyWorkers(workers: Worker[]): void {
  const reports: Record<string, [boolean, boolean, string, string]> = {
    [fx.ids.nas]: [true, true, "", ""],
    [fx.ids.backupHost]: [true, false, "", "/dev/fuse is missing"],
    [fx.ids.office]: [
      false,
      false,
      "remote_sources.mirror is off in worker.yaml",
      "WinFsp is not installed",
    ],
  };
  for (const w of workers) {
    const r = reports[w.id];
    if (!r || !w.info) continue;
    if (!w.info.features.includes(FEATURE)) w.info.features.push(FEATURE);
    w.info.remoteSources = {
      $typeName: "syncr.v1.RemoteSourceSupport",
      mirror: r[0],
      mount: r[1],
      mirrorReason: r[2],
      mountReason: r[3],
    };
  }
}

/** photosTree is what every snapshot of Nextcloud/Photos holds at its root. */
export function photosTree(): Tree {
  const month = (n: number, size: number) =>
    Object.fromEntries(
      Array.from({ length: n }, (_, i) => [
        `IMG_${String(2100 + i).padStart(4, "0")}.jpg`,
        size + i * 9 * KiB,
      ]),
    );
  return {
    "2026": { "10": month(12, 3.1 * MiB), "11": month(8, 3.4 * MiB) },
    "2025": { "12": month(10, 2.9 * MiB) },
    "README.txt": 2 * KiB,
  };
}

/** repository is the repository "Nextcloud photos" on MinIO, with snapshots of Nextcloud/Photos. */
export function repository(now: number): RepoState {
  const minio = fx.remotes(now).find((r) => r.id === fx.ids.minio);
  const snaps = snapshots(now, {
    count: 12,
    every: DAY,
    first: now % DAY > 3 * HOUR ? (now % DAY) - 3 * HOUR - 10 * MIN : 21 * HOUR,
    host: "nas-01",
    user: "syncr",
    paths: [MIRROR_PATH],
    tags: ["phone", tag(fx.ids.nextcloud)],
    bytes: 43.5 * GiB,
    files: 9_432,
    added: [300 * MiB, 950 * MiB],
    seed: 9101,
  });
  return {
    ownerId: fx.ids.admin,
    snapshots: snaps,
    tree: photosTree(),
    refreshedAt: now - 50 * MIN,
    stale: false,
    deleted: false,
    repo: create(RepositorySchema, {
      id: ids.repository,
      name: REPOSITORY_NAME,
      description: "The phone's photos, from Nextcloud every night",
      remoteId: fx.ids.minio,
      path: "restic/nextcloud-photos",
      remote: { id: fx.ids.minio, name: minio?.name ?? "MinIO archive", backendType: "s3" },
      resticId: "5e1d".repeat(16),
      formatVersion: 2,
      createdAt: ts(now - 20 * DAY),
      updatedAt: ts(now - 19 * DAY),
      version: 2n,
      stats: {
        snapshotCount: snaps.length,
        latestSnapshotAt: snaps[0]?.time,
        latestSnapshotBytes: snaps[0]?.summary?.totalBytesProcessed ?? 0n,
        snapshotsRefreshedAt: ts(now - 50 * MIN),
        snapshotsStale: false,
        scheduleCount: 1,
      },
    }),
  };
}

const filters = [
  { action: FilterAction.EXCLUDE, pattern: ".thumbnails/**" },
  { action: FilterAction.EXCLUDE, pattern: "*.tmp" },
];

/** definition is a backup of a folder of a remote into the demo repository. */
function definition(
  remoteId: string,
  path: string,
  method = ResticRemoteSourceMethod.MIRROR,
  extra: { allowEmpty?: boolean } = {},
): JobDefinition {
  const mirror = method === ResticRemoteSourceMethod.MIRROR;
  return {
    operation: {
      case: "resticBackup",
      value: {
        repositoryId: ids.repository,
        tags: ["phone"],
        remoteSource: {
          remoteId,
          path,
          method,
          filters,
          allowEmpty: extra.allowEmpty ?? false,
          transfers: mirror ? 8 : undefined,
        },
      },
    },
  } as unknown as JobDefinition;
}

/** goDuration writes seconds as Go does: 3m40s. */
function goDuration(ms: number): string {
  const s = Math.round(ms / SEC);
  const m = Math.floor(s / 60);
  return m ? `${m}m${s % 60}s` : `${s}s`;
}

/** mirroredText is the controller's "Mirrored" detail. */
function mirroredText(bytes: number, files: number, deleted: number, ms: number): string {
  return `${bytes ? formatBytes(bytes) : "0 bytes"}, ${files} files, ${deleted} deleted in ${goDuration(ms)}`;
}

interface RunSeed {
  bytes: number;
  files: number;
  deleted: number;
  mirrorMs: number;
}

/** output is a finished backup's output, with its remote-source stats. */
export function output(
  snapshotId: string,
  method: ResticRemoteSourceMethod,
  run: RunSeed,
  total: { bytes: number; files: number },
): JobOutput {
  const mirror = method === ResticRemoteSourceMethod.MIRROR;
  const value: Omit<ResticBackupOutput, "$typeName" | "$unknown"> = {
    snapshotId,
    dataAddedBytes: BigInt(Math.round(run.bytes)),
    filesNew: BigInt(snapshotId ? run.files : 0),
    filesChanged: BigInt(snapshotId ? Math.round(run.files / 10) : 0),
    filesUnmodified: BigInt(snapshotId ? total.files - run.files : 0),
    dirsNew: snapshotId ? 1n : 0n,
    dirsChanged: snapshotId ? 5n : 0n,
    dirsUnmodified: snapshotId ? 180n : 0n,
    dataAddedPackedBytes: BigInt(Math.round(run.bytes * 0.7)),
    totalFilesProcessed: BigInt(snapshotId ? total.files : 0),
    totalBytesProcessed: BigInt(snapshotId ? Math.round(total.bytes) : 0),
    remoteSource: {
      $typeName: "syncr.v1.RemoteSourceStats",
      method,
      mirroredBytes: BigInt(mirror ? Math.round(run.bytes) : 0),
      mirroredFiles: BigInt(mirror ? run.files : 0),
      deletedFiles: BigInt(mirror ? run.deleted : 0),
      mirrorElapsed: mirror ? durationFromMs(run.mirrorMs) : undefined,
      mirrorDiscarded: false,
    },
  };
  return {
    $typeName: "syncr.v1.JobOutput",
    output: { case: "resticBackup", value: { $typeName: "syncr.v1.ResticBackupOutput", ...value } },
  };
}

/** details are the controller's summary rows of a backup of a remote. */
export function details(
  snapshotId: string,
  method: ResticRemoteSourceMethod,
  run: RunSeed,
  total: { bytes: number; files: number },
): [string, string][] {
  const mirror = method === ResticRemoteSourceMethod.MIRROR;
  const rows: [string, string][] = [
    ["Snapshot", snapshotId ? snapshotId.slice(0, 8) : NO_SNAPSHOT],
    ["Read through", methodLabel(method)],
  ];
  if (mirror)
    rows.push(["Mirrored", mirroredText(run.bytes, run.files, run.deleted, run.mirrorMs)]);
  if (!snapshotId) return rows;
  rows.push(
    [
      "Files",
      `${run.files} new, ${Math.round(run.files / 10)} changed, ${(total.files - run.files).toLocaleString("en-US")} unmodified`,
    ],
    ["Folders", "1 new, 5 changed, 180 unmodified"],
    ["Data added", `${formatBytes(run.bytes)} (${formatBytes(run.bytes * 0.7)} stored)`],
    ["Processed", `${formatBytes(total.bytes)} in ${total.files.toLocaleString("en-US")} files`],
  );
  return rows;
}

/** History is the demo's schedule with its runs and jobs. */
export interface History {
  schedule: Schedule;
  runs: ScheduleRun[];
  jobs: Job[];
}

/**
 * history builds the schedule "Nightly Nextcloud photos backup" with 14
 * nights of runs (the one five nights ago found the folder empty), the
 * runs by hand and the mirror running now.
 */
export function history(now: number, repo: RepoState = repository(now)): History {
  const remotes = fx.remotes(now);
  const ref = (id: string) => {
    const r = remotes.find((x) => x.id === id);
    return { id, name: r?.name ?? "?", backendType: r?.backendType ?? "" };
  };
  const repoRef = { id: repo.repo.id, name: repo.repo.name, deleted: false };
  const minio = ref(fx.ids.minio);
  const nextcloud = ref(fx.ids.nextcloud);
  const title = (remote: string, path: string, method: ResticRemoteSourceMethod) =>
    `Back up ${remote}${path ? `/${path}` : ""} (${methodLabel(method).toLowerCase()}) → ${REPOSITORY_NAME}`;
  const total = { bytes: 43.5 * GiB, files: 9_432 };
  const jobs: Job[] = [];
  const runs: ScheduleRun[] = [];
  const job = (p: {
    id: string;
    at: number;
    worker?: string;
    remote?: string;
    path?: string;
    method?: ResticRemoteSourceMethod;
    status: JobStatus;
    runMs?: number;
    run?: RunSeed;
    snapshotId?: string;
    error?: [JobErrorCode, string];
    warnings?: string[];
    allowEmpty?: boolean;
    scheduled?: boolean;
  }): Job => {
    const method = p.method ?? ResticRemoteSourceMethod.MIRROR;
    const remote = p.remote ?? fx.ids.nextcloud;
    const path = p.path ?? "Photos";
    const started = p.status === JobStatus.FAILED && !p.runMs ? undefined : p.at + 4 * SEC;
    const ran = started !== undefined && p.status !== JobStatus.RUNNING;
    const out =
      p.run && ran && !p.error ? output(p.snapshotId ?? "", method, p.run, total) : undefined;
    const j = create(JobSchema, {
      id: p.id,
      workerId: p.worker ?? fx.ids.nas,
      kind: "restic_backup",
      status: p.status,
      ownerId: fx.ids.admin,
      createdAt: ts(p.at),
      dispatchedAt: started === undefined ? undefined : ts(started - 2 * SEC),
      startedAt: started === undefined ? undefined : ts(started),
      finishedAt:
        p.status === JobStatus.RUNNING ? undefined : ts((started ?? p.at) + (p.runMs ?? 0)),
      definition: definition(remote, path, method, { allowEmpty: p.allowEmpty }),
      title: title(ref(remote).name, path, method),
      repository: repoRef,
      remotes: remote === fx.ids.minio ? [minio] : [minio, ref(remote)],
      output: out,
      details:
        out && p.run
          ? details(p.snapshotId ?? "", method, p.run, total).map(([label, value]) => ({
              label,
              value,
            }))
          : [],
      error: p.error ? { code: p.error[0], message: p.error[1] } : undefined,
      warnings: p.warnings ?? [],
      exitCode: ran ? (p.error ? 1 : 0) : undefined,
      scheduleId: p.scheduled ? ids.schedule : "",
      schedule: p.scheduled ? { id: ids.schedule, name: SCHEDULE_NAME } : undefined,
      stats:
        ran && p.run
          ? {
              bytes: BigInt(Math.round(p.snapshotId ? total.bytes : 0)),
              totalBytes: BigInt(Math.round(p.snapshotId ? total.bytes : 0)),
              files: BigInt(p.snapshotId ? total.files : 0),
              totalFiles: BigInt(p.snapshotId ? total.files : 0),
              elapsed: durationFromMs(p.runMs ?? 0),
            }
          : undefined,
    });
    jobs.push(j);
    return j;
  };

  // The nights: snapshot n of the repository is night n (newest first).
  const nightly = (n: number) => {
    const snap = repo.snapshots[n];
    return snap?.time ? Number(snap.time.seconds) * 1000 - 4 * SEC : now - (n + 1) * DAY;
  };
  for (let n = 0; n < 14; n++) {
    const at = nightly(n);
    const id = `a7c0b0a1-1c0d-4e9f-8a7b-${(0x100 + n).toString(16).padStart(12, "0")}`;
    const empty = n === 5;
    const run = {
      bytes: (300 + ((n * 137) % 650)) * MiB,
      files: 10 + ((n * 7) % 30),
      deleted: n % 3 === 0 ? 1 : 0,
      mirrorMs: (90 + ((n * 41) % 150)) * SEC,
    };
    const j = empty
      ? job({
          id,
          at,
          status: JobStatus.FAILED,
          runMs: 6 * SEC,
          error: [JobErrorCode.SOURCE_EMPTY, messages.sourceEmpty],
          scheduled: true,
        })
      : job({
          id,
          at,
          status: JobStatus.SUCCEEDED,
          runMs: run.mirrorMs + 3 * MIN,
          run,
          snapshotId: repo.snapshots[n - (n > 5 ? 1 : 0)]?.id ?? "",
          scheduled: true,
        });
    runs.push(
      create(ScheduleRunSchema, {
        id: `e6f7c081-${n.toString(16).padStart(4, "0")}-4c5d-8e9f-000000000081`,
        scheduleId: ids.schedule,
        trigger: ScheduleRunTrigger.CRON,
        scheduledFor: ts(at),
        deadline: ts(at + 6 * HOUR),
        status: ScheduleRunStatus.LAUNCHED,
        workerId: fx.ids.nas,
        jobId: j.id,
        jobStatus: j.status,
        createdAt: ts(at),
        updatedAt: ts(at + 4 * SEC),
      }),
    );
  }

  // Runs by hand.
  job({
    id: ids.mountJob,
    at: now - 2 * DAY - 3 * HOUR,
    remote: fx.ids.storageBox,
    path: "Scans",
    method: ResticRemoteSourceMethod.MOUNT,
    status: JobStatus.SUCCEEDED,
    runMs: 4 * MIN,
    run: { bytes: 2.9 * MiB, files: 2, deleted: 0, mirrorMs: 0 },
    snapshotId: repo.snapshots[1]?.id.split("").reverse().join("") ?? "",
  });
  job({
    id: ids.emptyJob,
    at: now - 3 * DAY - 5 * HOUR,
    path: "Inbox",
    status: JobStatus.SUCCEEDED,
    runMs: 6 * SEC,
    run: { bytes: 0, files: 0, deleted: 3, mirrorMs: 2 * SEC },
    allowEmpty: true,
    warnings: [messages.emptyWarning],
  });
  job({
    id: ids.refusedJob,
    at: now - 4 * DAY - 2 * HOUR,
    worker: fx.ids.backupHost,
    remote: fx.ids.storageBox,
    path: "Scans",
    method: ResticRemoteSourceMethod.MOUNT,
    status: JobStatus.FAILED,
    error: [JobErrorCode.TOOL_UNAVAILABLE, messages.cannotMount],
  });

  const nextAt = nightly(0) + DAY;
  const schedule = create(ScheduleSchema, {
    id: ids.schedule,
    name: SCHEDULE_NAME,
    description: "The phone's photos, through a mirror on the NAS",
    cron: "10 3 * * *",
    timezone: "Europe/Berlin",
    target: { target: { case: "workerId", value: fx.ids.nas } },
    missedRun: { action: MissedRunAction.RUN_LATE },
    overlap: OverlapPolicy.SKIP,
    enabled: true,
    definition: definition(fx.ids.nextcloud, "Photos"),
    remotes: [minio, nextcloud],
    repository: { id: repo.repo.id, name: repo.repo.name },
    nextRunAt: ts(nextAt > now ? nextAt : nextAt + DAY),
    createdAt: ts(now - 20 * DAY),
    updatedAt: ts(now - 15 * DAY),
    version: 2n,
  });
  return { schedule, runs, jobs };
}

/** RUNNING_TOTALS are the running demo job's phases: what it mirrors, then what restic reads. */
export const RUNNING_TOTALS = {
  mirror: { bytes: 620 * MiB, files: 31 },
  backup: { bytes: 2.4 * GiB, files: 9_440 },
};

/**
 * runningJob is the mirror of Nextcloud/Photos running now on nas-01, in
 * its mirror phase (the mock only: the dev seed has no running one).
 */
export function runningJob(now: number): Job {
  const remotes = fx.remotes(now);
  const minio = remotes.find((r) => r.id === fx.ids.minio)!;
  const nextcloud = remotes.find((r) => r.id === fx.ids.nextcloud)!;
  return create(JobSchema, {
    id: ids.runningJob,
    workerId: fx.ids.nas,
    kind: "restic_backup",
    status: JobStatus.RUNNING,
    ownerId: fx.ids.admin,
    createdAt: ts(now - 40 * SEC),
    dispatchedAt: ts(now - 38 * SEC),
    startedAt: ts(now - 36 * SEC),
    definition: definition(fx.ids.nextcloud, "Photos"),
    title: `Back up Nextcloud/Photos (mirror) → ${REPOSITORY_NAME}`,
    repository: { id: ids.repository, name: REPOSITORY_NAME, deleted: false },
    remotes: [
      { id: minio.id, name: minio.name, backendType: minio.backendType },
      { id: nextcloud.id, name: nextcloud.name, backendType: nextcloud.backendType },
    ],
    stats: {
      bytes: BigInt(Math.round(RUNNING_TOTALS.mirror.bytes * 0.2)),
      totalBytes: BigInt(RUNNING_TOTALS.mirror.bytes),
      files: 6n,
      totalFiles: BigInt(RUNNING_TOTALS.mirror.files),
    },
  });
}

/** sourceOf returns a definition's remote source backup, if it is one. */
export function sourceOf(def: JobDefinition | undefined): ResticBackupDefinition | undefined {
  const op = def?.operation;
  return op?.case === "resticBackup" && op.value.remoteSource ? op.value : undefined;
}

const segments = (p: string) => p.split("/").filter((s) => s !== "" && s !== ".");

/** CheckDeps are what the controller's checks look at. */
export interface CheckDeps {
  remotes: Remote[];
  repositories: RepoState[];
  workers: Worker[];
  /** workerId is RunJob's worker: its report decides the method (schedules only warn, so never). */
  workerId?: string;
  /** label marks a schedule that targets a label. */
  label?: boolean;
}

/**
 * check returns the controller's problems with a backup of a remote (step
 * B's field errors), or none for any other definition. Another user's or
 * a missing remote is NOT_FOUND, as the API answers.
 */
export function check(def: JobDefinition | undefined, d: CheckDeps): Violation[] {
  const b = sourceOf(def);
  if (!b?.remoteSource) return [];
  const src = b.remoteSource;
  const field = "definition.restic_backup";
  const rs = `${field}.remote_source`;
  const v: Violation[] = [];
  if (b.paths.length) v.push({ field: `${field}.paths`, message: api.paths });
  if (b.excludes.length) v.push({ field: `${field}.excludes`, message: api.excludes });
  if (b.oneFileSystem) v.push({ field: `${field}.one_file_system`, message: api.notWith });
  if (b.useFsSnapshot) v.push({ field: `${field}.use_fs_snapshot`, message: api.notWith });
  if (!src.remoteId) v.push({ field: `${rs}.remote_id`, message: "is required" });
  else {
    const remote = d.remotes.find((r) => r.id === src.remoteId);
    if (!remote) throw new ConnectError("remote not found", Code.NotFound);
    if (fx.backends.find((x) => x.id === remote.backendType)?.capabilities?.local)
      v.push({ field: `${rs}.remote_id`, message: api.local });
  }
  if (segments(src.path).includes(".."))
    v.push({ field: `${rs}.path`, message: 'path must not contain ".." segments' });
  const mirror = src.method === ResticRemoteSourceMethod.MIRROR;
  if (
    src.method !== ResticRemoteSourceMethod.MIRROR &&
    src.method !== ResticRemoteSourceMethod.MOUNT
  )
    v.push({ field: `${rs}.method`, message: "must be mirror or mount" });
  else if (!mirror) {
    if (src.discardMirror) v.push({ field: `${rs}.discard_mirror`, message: api.mirrorOnly });
    if (src.transfers !== undefined) v.push({ field: `${rs}.transfers`, message: api.mirrorOnly });
    if (src.fastList) v.push({ field: `${rs}.fast_list`, message: api.mirrorOnly });
  }
  if (src.transfers !== undefined && (src.transfers < 1 || src.transfers > 64))
    v.push({ field: `${rs}.transfers`, message: "must be between 1 and 64" });
  if (b.tags.length > 31) v.push({ field: `${field}.tags`, message: api.tags });
  else
    b.tags.forEach((t, i) => {
      if (t.startsWith(REMOTE_TAG_PREFIX))
        v.push({ field: `${field}.tags[${i}]`, message: api.tagPrefix });
    });
  const repo = d.repositories.find((r) => r.repo.id === b.repositoryId)?.repo;
  if (repo && repo.remoteId === src.remoteId) {
    const a = segments(src.path);
    const r = segments(repo.path);
    const pre = (x: string[], y: string[]) => x.every((s, i) => y[i] === s);
    const where = `"${repo.name}" (at ${repo.path ? `"${repo.path}"` : "the remote's root"})`;
    if (a.length === r.length && pre(a, r))
      v.push({
        field: `${rs}.path`,
        message: `is the folder of repository ${where}: a backup cannot read its own repository`,
      });
    else if (a.length < r.length && pre(a, r))
      v.push({
        field: `${rs}.path`,
        message: `holds repository ${where}: a backup cannot read its own repository; choose a folder beside it`,
      });
    else if (r.length < a.length && pre(r, a))
      v.push({
        field: `${rs}.path`,
        message: `is inside the folder of repository ${where}: a backup cannot read its own repository`,
      });
  }
  if (d.label) v.push({ field: "target.worker_label", message: api.singleWorker });
  const w = d.workers.find((x) => x.id === d.workerId);
  if (w?.info && !v.length) {
    const rep = w.info.remoteSources;
    if (!rep && !w.info.features.includes(FEATURE))
      v.push({
        field: "worker_id",
        message: `worker "${w.name}" must be upgraded to back up a folder of a remote`,
      });
    else if (rep && mirror && !rep.mirror)
      v.push({
        field: "worker_id",
        message: `worker "${w.name}" cannot keep a mirror of a remote: ${rep.mirrorReason || "it gave no reason"}`,
      });
    else if (rep && !mirror && !rep.mount)
      v.push({
        field: "worker_id",
        message: `worker "${w.name}" cannot mount a remote: ${rep.mountReason || "it gave no reason"}`,
      });
  }
  return v;
}

/** title names a backup of a remote as the controller does. */
export function title(def: JobDefinition | undefined, remotes: Remote[], repoName: string): string {
  const src = sourceOf(def)?.remoteSource;
  if (!src) return "";
  const name = remotes.find((r) => r.id === src.remoteId)?.name ?? "a remote";
  const path = src.path.replace(/^\/+|\/+$/g, "");
  const dry = sourceOf(def)?.dryRun ? " (dry run)" : "";
  return `Back up ${path ? `${name}/${path}` : name} (${methodLabel(src.method).toLowerCase()}) → ${repoName}${dry}`;
}

/**
 * finish completes a backup of a remote the mock ran: a snapshot tagged
 * with the remote (unless a dry run), its output and summary rows.
 */
export function finish(j: Job, repo: RepoState, hostname: string): void {
  const b = sourceOf(j.definition);
  const src = b?.remoteSource;
  if (!b || !src) return;
  const run = { bytes: 410 * MiB, files: 21, deleted: 2, mirrorMs: 48 * SEC };
  const total = {
    bytes: Number(repo.snapshots[0]?.summary?.totalBytesProcessed ?? 2 * GiB),
    files: Number(repo.snapshots[0]?.summary?.totalFilesProcessed ?? 1200),
  };
  let snapshotId = "";
  if (!b.dryRun) {
    const [snap] = snapshots(Date.now() + 1, {
      count: 1,
      every: DAY,
      first: 0,
      host: hostname,
      user: "syncr",
      paths: [
        `/var/lib/syncr/sources/${methodLabel(src.method).toLowerCase()}/${"ab12".repeat(8)}`,
      ],
      tags: [...b.tags, tag(src.remoteId)],
      bytes: total.bytes,
      files: total.files,
      added: [run.bytes, run.bytes],
      seed: Date.now() & 0xffff,
    });
    repo.snapshots.unshift(snap);
    snapshotId = snap.id;
  }
  j.output = output(snapshotId, src.method, run, total);
  j.details = details(snapshotId, src.method, run, total)
    .filter(([label]) => !(b.dryRun && label === "Snapshot"))
    .map(([label, value]) => ({ $typeName: "syncr.v1.JobDetail", label, value }));
}
