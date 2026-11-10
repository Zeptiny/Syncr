/**
 * Milestone 5 part of the dev-only mock: restic repositories with their
 * snapshots and trees, the worker probes (file listings, "Test remote"),
 * and the restic jobs and schedules of the demo data. transport.ts
 * registers it; tests use its data and services too.
 *
 * It behaves like the real API where the UI depends on it: the same field
 * paths and messages, the deleting and failed-deletion states, jobs that
 * wait for an older job on the same repository.
 */
import { create } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromMs, timestampMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, type ConnectRouter } from "@connectrpc/connect";

import {
  JobSchema,
  JobStatus,
  WorkerSchema,
  WorkerStatus,
  type Job,
  type JobDefinition,
  type JobRepositoryRef,
  type Worker,
} from "@/gen/syncr/v1/api_pb";
import {
  JobErrorCode,
  ProbeEntrySchema,
  ProbeEntryType,
  ResticRestoreOverwrite,
  ResticSnapshotSchema,
  WorkerMode,
  type JobOutput,
  type ProbeEntry,
  type ResticSnapshot,
} from "@/gen/syncr/v1/link_pb";
import { ProbeService } from "@/gen/syncr/v1/probe_pb";
import type { Remote } from "@/gen/syncr/v1/remotes_pb";
import {
  RepositoryCreateMode,
  RepositorySchema,
  ResticService,
  type Repository,
} from "@/gen/syncr/v1/restic_pb";
import {
  MissedRunAction,
  OverlapPolicy,
  ScheduleSchema,
  type Schedule,
} from "@/gen/syncr/v1/schedules_pb";
import type { User } from "@/gen/syncr/v1/users_pb";
import * as fx from "@/test/fixtures";

import { next } from "./cron";

type Violation = { field: string; message: string };
type MessageInit<T> = Omit<Partial<T>, "$typeName" | "$unknown">;

const MIN = 60_000;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;
const MiB = 1024 ** 2;
const GiB = 1024 ** 3;
const ts = timestampFromMs;

/** The link features and probe operations a current worker reports. */
export const FEATURES = ["probe", "restic.v2", "restic.delete"];
export const PROBE_OPS = [
  "list_local",
  "list_remote",
  "about",
  "restic_snapshots",
  "restic_ls",
  "restic_init",
  "restic_config",
];
const RESTIC_KINDS = [
  "restic_backup",
  "restic_forget",
  "restic_prune",
  "restic_check",
  "restic_restore",
  "restic_delete_data",
];

/** IDs of the milestone 5 demo objects. */
export const ids = {
  oldNas: "5a1e2b7c-1d3f-4e8a-9b0c-2d4f6a8b0c05",
  photosVault: "7d3a1c9e-5b2f-4a6d-8c1e-3f5a7b9d1e01",
  nasDocs: "7d3a1c9e-5b2f-4a6d-8c1e-3f5a7b9d1e02",
  officeLaptop: "7d3a1c9e-5b2f-4a6d-8c1e-3f5a7b9d1e03",
  oldLaptop: "7d3a1c9e-5b2f-4a6d-8c1e-3f5a7b9d1e04",
  archive2019: "7d3a1c9e-5b2f-4a6d-8c1e-3f5a7b9d1e05",
  retiredNas: "7d3a1c9e-5b2f-4a6d-8c1e-3f5a7b9d1e06",
  aliceBackups: "7d3a1c9e-5b2f-4a6d-8c1e-3f5a7b9d1e07",
  aliceB2: "9e7d6c5b-2a1f-4b3c-8d9e-0f1a2b3c4d08",
  nightlyBackup: "4b8f0d2a-6c1e-4f3a-9d5b-7e2c1a0f3b11",
  weeklyRetention: "4b8f0d2a-6c1e-4f3a-9d5b-7e2c1a0f3b12",
  monthlyCheck: "4b8f0d2a-6c1e-4f3a-9d5b-7e2c1a0f3b13",
  officeBackup: "4b8f0d2a-6c1e-4f3a-9d5b-7e2c1a0f3b14",
  oldLaptopBackup: "4b8f0d2a-6c1e-4f3a-9d5b-7e2c1a0f3b15",
  archiveCheck: "4b8f0d2a-6c1e-4f3a-9d5b-7e2c1a0f3b16",
  runningBackup: "e5f4a3b2-1c0d-4e9f-8a7b-6c5d4e3f2a01",
  waitingBackup: "e5f4a3b2-1c0d-4e9f-8a7b-6c5d4e3f2a02",
  oldLaptopCheck: "e5f4a3b2-1c0d-4e9f-8a7b-6c5d4e3f2a03",
  oldLaptopDelete: "e5f4a3b2-1c0d-4e9f-8a7b-6c5d4e3f2a04",
  retiredDelete: "e5f4a3b2-1c0d-4e9f-8a7b-6c5d4e3f2a05",
  archiveDelete: "e5f4a3b2-1c0d-4e9f-8a7b-6c5d4e3f2a06",
  lastBackup: "e5f4a3b2-1c0d-4e9f-8a7b-6c5d4e3f2a07",
  restore: "e5f4a3b2-1c0d-4e9f-8a7b-6c5d4e3f2a08",
  failedCheck: "e5f4a3b2-1c0d-4e9f-8a7b-6c5d4e3f2a09",
};

// rand is a small deterministic generator, so the data looks the same on
// every load and in tests.
function rand(seed: number): () => number {
  let x = seed >>> 0 || 1;
  return () => {
    x ^= x << 13;
    x ^= x >>> 17;
    x ^= x << 5;
    return (x >>> 0) / 0x1_0000_0000;
  };
}

function hex(r: () => number, n: number): string {
  let s = "";
  while (s.length < n) s += Math.floor(r() * 16).toString(16);
  return s;
}

/**
 * workers returns the fixture workers as milestone 5 sees them: connected
 * ones report the probe and restic features, nas-01 allows /srv and /mnt,
 * and old-nas is a worker too old for either.
 */
export function workers(now: number): Worker[] {
  const list = fx.workers(now);
  for (const w of list) {
    if (!w.info) continue;
    w.info.features = [...FEATURES];
    w.info.probeOps = [...PROBE_OPS];
    w.info.jobKinds = ["rclone_transfer", ...RESTIC_KINDS];
    if (!w.info.tools.some((t) => t.name === "restic"))
      w.info.tools.push({ $typeName: "syncr.v1.ToolVersion", name: "restic", version: "0.18.1" });
    if (w.id === fx.ids.nas) w.info.localRoots = ["/srv", "/mnt"];
    if (w.id === fx.ids.office) w.info.localRoots = ["C:\\Users\\ada", "D:\\Shared"];
  }
  list.push(
    create(WorkerSchema, {
      id: ids.oldNas,
      name: "old-nas",
      labels: [],
      mode: WorkerMode.ACTIVE,
      status: WorkerStatus.ONLINE,
      lastSeen: ts(now - 9_000),
      createdAt: ts(now - 200 * DAY),
      info: {
        workerVersion: "v2.0.0-beta.3",
        os: "linux",
        arch: "amd64",
        hostname: "old-nas",
        tools: [{ name: "rclone", version: "v1.69.1" }],
        jobKinds: ["rclone_transfer"],
        maxConcurrentJobs: 1,
        localRoots: ["/volume1"],
      },
    }),
  );
  return list;
}

/** Tree is a mock folder: names to sub-folders or file sizes in bytes. */
export interface Tree {
  [name: string]: Tree | number;
}

function photoMonth(r: () => number, month: number): Tree {
  const t: Tree = {};
  const n = 8 + Math.floor(r() * 6);
  for (let i = 0; i < n; i++) {
    const num = String(1000 + month * 37 + i * 3).padStart(4, "0");
    t[`IMG_${num}.jpg`] = Math.round((2.2 + r() * 5) * MiB);
  }
  if (month % 3 === 0) t[`VID_${String(200 + month)}.mp4`] = Math.round((80 + r() * 240) * MiB);
  return t;
}

function photos(): Tree {
  const r = rand(77);
  const y2026: Tree = {};
  for (let m = 1; m <= 11; m++) y2026[String(m).padStart(2, "0")] = photoMonth(r, m);
  const y2025: Tree = {};
  for (let m = 1; m <= 12; m++) y2025[String(m).padStart(2, "0")] = photoMonth(r, m);
  return { "2025": y2025, "2026": y2026, "catalog.lrcat": 412 * MiB, "README.txt": 1840 };
}

const docs: Tree = {
  contracts: { "lease-2024.pdf": 1.4 * MiB, "insurance.pdf": 820_000, "internet.pdf": 310_000 },
  invoices: {
    "2026-08.pdf": 96_000,
    "2026-09.pdf": 101_500,
    "2026-10.pdf": 88_200,
    "2026-11.pdf": 93_700,
  },
  taxes: { "2025": { "return.pdf": 2.1 * MiB, "receipts.zip": 48 * MiB } },
  "notes.md": 12_400,
  "passwords.kdbx": 68_000,
};

const media: Tree = {
  films: { "big-buck-bunny.mkv": 1.6 * GiB, "sintel.mkv": 1.1 * GiB },
  music: { albums: { "live-2019.flac": 412 * MiB }, "playlist.m3u": 2_100 },
  series: {},
};

const officeDocs: Tree = {
  Projects: {
    Bridge: { "plan-v3.dwg": 18 * MiB, "costs.xlsx": 240_000, "notes.docx": 61_000 },
    Harbour: { "survey.pdf": 9.4 * MiB },
  },
  Letters: { "2026-10 council.docx": 44_000, "2026-11 supplier.docx": 39_500 },
  "budget.xlsx": 512_000,
  "desktop.ini": 282,
};

const repoDir: Tree = {
  config: 155,
  data: {},
  index: {},
  keys: {},
  locks: {},
  snapshots: {},
};

/** workerTrees are the files each worker shows, keyed by worker ID then by root. */
export function workerTrees(): Record<string, Record<string, Tree>> {
  return {
    [fx.ids.nas]: {
      "/srv": { photos: photos(), docs, backup: { "db-2026-11-09.sql.gz": 86 * MiB } },
      "/mnt": { media },
    },
    [fx.ids.backupHost]: {
      "/srv/backup": { db: { "dump-2026-11-09.sql.gz": 86 * MiB }, restores: {} },
    },
    [fx.ids.office]: {
      "C:\\Users\\ada": { Documents: officeDocs, Desktop: {} },
      "D:\\Shared": { Projects: {} },
    },
  };
}

/** remoteTrees are the folders each remote shows, keyed by remote ID. */
export function remoteTrees(): Record<string, Tree> {
  return {
    [fx.ids.minio]: {
      restic: { "nas-docs": repoDir, "archive-2019": repoDir, legacy: repoDir },
      archive: { "2025": { "q1.tar": 3.1 * GiB, "q2.tar": 2.9 * GiB } },
    },
    [fx.ids.b2]: {
      restic: { photos: repoDir },
      photos: { "2026": {} },
    },
    [fx.ids.storageBox]: {
      backups: { office: repoDir, "old-laptop": repoDir },
      "db-dumps": { "dump-2026-11-09.sql.gz": 86 * MiB },
      archive: {},
    },
    [fx.ids.nextcloud]: { Documents: {}, Projects: {} },
    [fx.ids.photos]: photos(),
    [fx.ids.media]: media,
  };
}

function walk(tree: Tree, parts: string[]): Tree | undefined {
  let at: Tree = tree;
  for (const p of parts) {
    const nextNode = at[p];
    if (nextNode === undefined || typeof nextNode === "number") return undefined;
    at = nextNode;
  }
  return at;
}

/** entries lists a mock folder; join builds each entry's path. */
function entries(
  tree: Tree,
  now: number,
  join: (name: string) => string,
  dirsOnly = false,
): ProbeEntry[] {
  return Object.entries(tree)
    .filter(([, v]) => !dirsOnly || typeof v !== "number")
    .map(([name, v], i) => {
      const dir = typeof v !== "number";
      return create(ProbeEntrySchema, {
        name,
        path: join(name),
        type: dir ? ProbeEntryType.DIR : ProbeEntryType.FILE,
        size: dir ? 0n : BigInt(Math.round(v)),
        modTime: ts(now - (3 + ((name.length * 7 + i * 13) % 200)) * DAY - i * 37 * MIN),
        mode: dir ? "drwxr-xr-x" : "-rw-r--r--",
      });
    });
}

interface SnapshotSeed {
  count: number;
  /** every is the time between snapshots; first is how long ago the newest was. */
  every: number;
  first: number;
  host: string;
  user: string;
  paths: string[];
  tags: string[];
  /** bytes and files are what one backup processes; added is the range it adds. */
  bytes: number;
  files: number;
  added: [number, number];
  seed: number;
}

function snapshots(now: number, s: SnapshotSeed): ResticSnapshot[] {
  const r = rand(s.seed);
  const out: ResticSnapshot[] = [];
  for (let i = 0; i < s.count; i++) {
    const at = now - s.first - i * s.every;
    const growth = 1 - i * 0.004;
    const added = Math.round(s.added[0] + r() * (s.added[1] - s.added[0]));
    const changed = Math.round(20 + r() * 400);
    const fresh = Math.round(10 + r() * 300);
    const files = Math.round(s.files * growth);
    out.push(
      create(ResticSnapshotSchema, {
        id: hex(r, 64),
        time: ts(at),
        host: s.host,
        username: s.user,
        paths: s.paths,
        tags: s.tags,
        programVersion: "restic 0.18.1",
        summary: {
          backupStart: ts(at),
          backupEnd: ts(at + Math.round((6 + r() * 18) * MIN)),
          filesNew: BigInt(fresh),
          filesChanged: BigInt(changed),
          filesUnmodified: BigInt(Math.max(0, files - fresh - changed)),
          dirsNew: 2n,
          dirsChanged: 14n,
          dirsUnmodified: 380n,
          dataBlobs: BigInt(Math.round(added / (1.2 * MiB))),
          treeBlobs: 96n,
          dataAddedBytes: BigInt(Math.round(added * 1.12)),
          dataAddedPackedBytes: BigInt(added),
          totalFilesProcessed: BigInt(files),
          totalBytesProcessed: BigInt(Math.round(s.bytes * growth)),
        },
      }),
    );
  }
  return out;
}

/** RepoState is a mock repository with what the real one keeps server-side. */
export interface RepoState {
  repo: Repository;
  ownerId: string;
  snapshots: ResticSnapshot[];
  /** tree is the snapshot tree every snapshot of the repository shows. */
  tree: Tree;
  refreshedAt: number | undefined;
  stale: boolean;
  /** deleted marks a forgotten repository (kept for job history only). */
  deleted: boolean;
}

/** M5Data is the milestone 5 demo data. */
export interface M5Data {
  repositories: RepoState[];
  schedules: Schedule[];
  jobs: Job[];
}

function nest(path: string[], leaf: Tree): Tree {
  return path.reduceRight<Tree>((acc, p) => ({ [p]: acc }), leaf);
}

interface JobSeed {
  id: string;
  kind: string;
  status: JobStatus;
  ago: number;
  /** duration in ms, for started jobs. */
  duration?: number;
  worker: string;
  definition: MessageInit<JobDefinition>["operation"];
  title: string;
  repo: RepoState | { id: string; name: string; deleted: true };
  remote?: string;
  output?: JobOutput["output"];
  details?: [string, string][];
  error?: [JobErrorCode, string];
  warnings?: string[];
  waitingFor?: string;
  schedule?: { id: string; name: string };
  bytes?: number;
  total?: number;
  files?: number;
}

/** data builds the milestone 5 demo data relative to now. */
export function data(now: number): M5Data {
  const remoteRef = (id: string) => {
    const r = fx.remotes(now).find((x) => x.id === id);
    return { id, name: r?.name ?? "?", backendType: r?.backendType ?? "" };
  };
  const repo = (
    p: {
      id: string;
      name: string;
      description?: string;
      remote: string;
      path: string;
      age: number;
      owner?: string;
      remoteRef?: { id: string; name: string; backendType: string };
    },
    snaps: ResticSnapshot[],
    tree: Tree,
    opts: { stale?: boolean; refreshedAgo?: number; schedules?: number } = {},
  ): RepoState => {
    const latest = snaps[0];
    return {
      ownerId: p.owner ?? fx.ids.admin,
      snapshots: snaps,
      tree,
      refreshedAt: now - (opts.refreshedAgo ?? 12 * MIN),
      stale: opts.stale ?? false,
      deleted: false,
      repo: create(RepositorySchema, {
        id: p.id,
        name: p.name,
        description: p.description ?? "",
        remoteId: p.remote,
        path: p.path,
        remote: p.remoteRef ?? remoteRef(p.remote),
        resticId: hex(rand(p.id.charCodeAt(35) * 131 + 7), 64),
        formatVersion: 2,
        createdAt: ts(now - p.age),
        updatedAt: ts(now - p.age + DAY),
        version: 2n,
        stats: {
          snapshotCount: snaps.length,
          latestSnapshotAt: latest?.time,
          latestSnapshotBytes: latest?.summary?.totalBytesProcessed ?? 0n,
          snapshotsRefreshedAt: ts(now - (opts.refreshedAgo ?? 12 * MIN)),
          snapshotsStale: opts.stale ?? false,
          scheduleCount: opts.schedules ?? 0,
        },
      }),
    };
  };

  const photosTree = nest(["srv", "photos"], photos());
  const vault = repo(
    {
      id: ids.photosVault,
      name: "Photos vault",
      description: "Every photo and video, nightly from the NAS",
      remote: fx.ids.b2,
      path: "restic/photos",
      age: 64 * DAY,
    },
    snapshots(now, {
      count: 30,
      every: DAY,
      first: 3 * HOUR + 12 * MIN,
      host: "nas-01",
      user: "syncr",
      paths: ["/srv/photos"],
      tags: ["photos"],
      bytes: 120.4 * GiB,
      files: 48_210,
      added: [0.5 * GiB, 3 * GiB],
      seed: 9001,
    }),
    photosTree,
    { schedules: 3 },
  );
  const nasDocs = repo(
    {
      id: ids.nasDocs,
      name: "NAS documents",
      description: "Contracts, invoices and tax papers",
      remote: fx.ids.minio,
      path: "restic/nas-docs",
      age: 40 * DAY,
    },
    snapshots(now, {
      count: 14,
      every: 2 * DAY,
      first: 26 * HOUR,
      host: "nas-01",
      user: "syncr",
      paths: ["/srv/docs"],
      tags: ["documents"],
      bytes: 3.8 * GiB,
      files: 6_420,
      added: [4 * MiB, 90 * MiB],
      seed: 9002,
    }),
    nest(["srv", "docs"], docs),
    { stale: true, refreshedAgo: 26 * HOUR },
  );
  const office = repo(
    {
      id: ids.officeLaptop,
      name: "Office laptop",
      description: "Ada's documents, hourly while the PC is on",
      remote: fx.ids.storageBox,
      path: "backups/office",
      age: 21 * DAY,
    },
    snapshots(now, {
      count: 10,
      every: 9 * HOUR,
      first: 19 * HOUR,
      host: "OFFICE-PC",
      user: "OFFICE-PC\\ada",
      paths: ["C:\\Users\\ada\\Documents"],
      tags: ["office"],
      bytes: 11.6 * GiB,
      files: 18_940,
      added: [20 * MiB, 400 * MiB],
      seed: 9003,
    }),
    nest(["C", "Users", "ada", "Documents"], officeDocs),
    { schedules: 1, refreshedAgo: 2 * HOUR },
  );
  const oldLaptop = repo(
    {
      id: ids.oldLaptop,
      name: "Old laptop",
      description: "The laptop that was replaced in spring",
      remote: fx.ids.storageBox,
      path: "backups/old-laptop",
      age: 300 * DAY,
    },
    snapshots(now, {
      count: 6,
      every: 7 * DAY,
      first: 190 * DAY,
      host: "old-laptop",
      user: "ada",
      paths: ["/home/ada"],
      tags: [],
      bytes: 64 * GiB,
      files: 210_300,
      added: [100 * MiB, 2 * GiB],
      seed: 9004,
    }),
    nest(["home", "ada"], { Documents: docs, ".bashrc": 3_771 }),
    { schedules: 1, refreshedAgo: 3 * DAY },
  );
  oldLaptop.repo.deleting = true;
  oldLaptop.repo.deletionJobId = ids.oldLaptopDelete;
  oldLaptop.repo.deletionRequestedAt = ts(now - 4 * MIN);
  const archive = repo(
    {
      id: ids.archive2019,
      name: "Archive 2019",
      description: "Closed project archive",
      remote: fx.ids.minio,
      path: "restic/archive-2019",
      age: 500 * DAY,
    },
    snapshots(now, {
      count: 3,
      every: 30 * DAY,
      first: 400 * DAY,
      host: "nas-01",
      user: "syncr",
      paths: ["/srv/archive/2019"],
      tags: ["archive"],
      bytes: 22 * GiB,
      files: 9_800,
      added: [1 * GiB, 4 * GiB],
      seed: 9005,
    }),
    nest(["srv", "archive", "2019"], { "projects.tar": 21 * GiB }),
    { schedules: 1, refreshedAgo: 6 * DAY },
  );
  archive.repo.deletionJobId = ids.archiveDelete;
  archive.repo.deletionRequestedAt = ts(now - 2 * DAY);
  archive.repo.deletionError =
    "deleting the data failed: repository_locked: another restic client is using the repository; nothing was deleted";
  const alice = repo(
    {
      id: ids.aliceBackups,
      name: "Alice's backups",
      remote: ids.aliceB2,
      remoteRef: { id: ids.aliceB2, name: "Alice's B2", backendType: "b2" },
      path: "restic",
      age: 12 * DAY,
      owner: fx.ids.alice,
    },
    snapshots(now, {
      count: 4,
      every: 3 * DAY,
      first: 20 * HOUR,
      host: "backup-host",
      user: "syncr",
      paths: ["/srv/backup/alice"],
      tags: [],
      bytes: 1.2 * GiB,
      files: 940,
      added: [2 * MiB, 60 * MiB],
      seed: 9006,
    }),
    nest(["srv", "backup", "alice"], { "notes.txt": 4_100 }),
  );
  const repositories = [vault, nasDocs, office, oldLaptop, archive, alice];

  const jobs: Job[] = [];
  const job = (s: JobSeed): Job => {
    const created = now - s.ago;
    const started = s.status === JobStatus.QUEUED ? undefined : created + 4_000;
    const finished =
      started !== undefined && s.duration !== undefined && s.status !== JobStatus.RUNNING
        ? started + s.duration
        : undefined;
    const r = s.repo;
    const ref: MessageInit<JobRepositoryRef> =
      "repo" in r ? { id: r.repo.id, name: r.repo.name, deleted: false } : r;
    const remote = "repo" in r ? r.repo.remote : s.remote ? remoteRef(s.remote) : undefined;
    const j = create(JobSchema, {
      id: s.id,
      workerId: s.worker,
      kind: s.kind,
      status: s.status,
      ownerId: fx.ids.admin,
      createdAt: ts(created),
      dispatchedAt: started === undefined ? undefined : ts(started - 2_000),
      startedAt: started === undefined ? undefined : ts(started),
      finishedAt: finished === undefined ? undefined : ts(finished),
      definition: { operation: s.definition },
      title: s.title,
      repository: ref,
      remotes: remote ? [remote] : [],
      waitingForJobId: s.waitingFor ?? "",
      details: (s.details ?? []).map(([label, value]) => ({ label, value })),
      output: s.output ? { output: s.output } : undefined,
      error: s.error ? { code: s.error[0], message: s.error[1] } : undefined,
      warnings: s.warnings ?? [],
      scheduleId: s.schedule?.id ?? "",
      schedule: s.schedule,
      stats:
        s.bytes === undefined
          ? undefined
          : {
              bytes: BigInt(Math.round(s.bytes)),
              totalBytes: BigInt(Math.round(s.total ?? s.bytes)),
              files: BigInt(s.files ?? 0),
              totalFiles: BigInt(s.files ?? 0),
              elapsed: s.duration === undefined ? undefined : durationFromMs(s.duration),
            },
    });
    jobs.push(j);
    return j;
  };
  const jobId = (n: number) =>
    `${((0xb5c0ffee + n * 0x9e3779b1) >>> 0).toString(16).padStart(8, "0")}-1c0d-4e9f-8a7b-${n.toString(16).padStart(12, "0")}`;
  const nightly = { id: ids.nightlyBackup, name: "Nightly photos backup" };

  // Nightly backups of the photos: one job per snapshot.
  vault.snapshots.forEach((snap, i) => {
    const sum = snap.summary!;
    const at = timestampMs(snap.time!);
    const duration = timestampMs(sum.backupEnd!) - at;
    const warn = i === 11;
    job({
      id: i === 0 ? ids.lastBackup : jobId(100 + i),
      kind: "restic_backup",
      status: JobStatus.SUCCEEDED,
      ago: now - at + 4_000,
      duration,
      worker: fx.ids.nas,
      definition: {
        case: "resticBackup",
        value: {
          repositoryId: vault.repo.id,
          paths: ["/srv/photos"],
          tags: ["photos"],
          excludes: ["*.tmp", ".thumbnails"],
          excludeCaches: true,
        } as never,
      },
      title: "Back up /srv/photos → Photos vault",
      repo: vault,
      schedule: nightly,
      bytes: Number(sum.totalBytesProcessed),
      files: Number(sum.totalFilesProcessed),
      warnings: warn ? ["3 items could not be read"] : [],
      output: {
        case: "resticBackup",
        value: {
          $typeName: "syncr.v1.ResticBackupOutput",
          snapshotId: snap.id,
          dataAddedBytes: sum.dataAddedBytes,
          filesNew: sum.filesNew,
          filesChanged: sum.filesChanged,
          filesUnmodified: sum.filesUnmodified,
          dirsNew: sum.dirsNew,
          dirsChanged: sum.dirsChanged,
          dirsUnmodified: sum.dirsUnmodified,
          dataAddedPackedBytes: sum.dataAddedPackedBytes,
          totalFilesProcessed: sum.totalFilesProcessed,
          totalBytesProcessed: sum.totalBytesProcessed,
        },
      },
      details: [
        ["Snapshot", snap.id.slice(0, 8)],
        [
          "Files",
          `${sum.filesNew} new, ${sum.filesChanged} changed, ${sum.filesUnmodified} unmodified`,
        ],
        ["Data added", `${(Number(sum.dataAddedPackedBytes) / GiB).toFixed(2)} GiB stored`],
      ],
    });
  });
  vault.repo.stats!.lastBackup = {
    $typeName: "syncr.v1.RepositoryJobRef",
    jobId: ids.lastBackup,
    status: JobStatus.SUCCEEDED,
    createdAt: jobs[0].createdAt,
    finishedAt: jobs[0].finishedAt,
  };

  // Weekly retention runs that removed snapshots.
  const r = rand(515);
  for (let w = 0; w < 4; w++) {
    const removed = Array.from({ length: 5 + Math.floor(r() * 3) }, () => hex(r, 64));
    job({
      id: jobId(200 + w),
      kind: "restic_forget",
      status: JobStatus.SUCCEEDED,
      ago: (2 + w * 7) * DAY + 5 * HOUR,
      duration: Math.round((9 + r() * 14) * MIN),
      worker: fx.ids.nas,
      definition: {
        case: "resticForget",
        value: {
          repositoryId: vault.repo.id,
          policy: { keepDaily: 7, keepWeekly: 4, keepMonthly: 12 },
          prune: true,
        } as never,
      },
      title: "Apply retention to Photos vault",
      repo: vault,
      schedule: { id: ids.weeklyRetention, name: "Weekly retention: photos" },
      output: {
        case: "resticForget",
        value: {
          $typeName: "syncr.v1.ResticForgetOutput",
          removedSnapshotIds: removed,
          keptCount: 30 - w,
        },
      },
      details: [
        ["Removed", `${removed.length} snapshots`],
        ["Kept", `${30 - w} snapshots`],
        ["Pruned", "yes"],
      ],
    });
  }

  // Four checks; one found errors.
  const checks: [number, number, boolean, string][] = [
    [9 * DAY + 6 * HOUR, 10, true, jobId(300)],
    [16 * DAY + 6 * HOUR, 0, true, jobId(301)],
    [23 * DAY + 6 * HOUR, 0, false, ids.failedCheck],
    [29 * DAY + 6 * HOUR, 0, true, jobId(303)],
  ];
  for (const [ago, pct, ok, id] of checks) {
    job({
      id,
      kind: "restic_check",
      status: ok ? JobStatus.SUCCEEDED : JobStatus.FAILED,
      ago,
      duration: (pct ? 41 : 6) * MIN,
      worker: fx.ids.nas,
      definition: {
        case: "resticCheck",
        value: { repositoryId: vault.repo.id, readDataPercent: pct } as never,
      },
      title: "Check Photos vault",
      repo: vault,
      schedule: pct ? { id: ids.monthlyCheck, name: "Monthly deep check: photos" } : undefined,
      error: ok
        ? undefined
        : [
            JobErrorCode.EXECUTION_FAILED,
            "the check found 2 error(s); restic suggests repairing the index",
          ],
      output: ok
        ? {
            case: "resticCheck",
            value: {
              $typeName: "syncr.v1.ResticCheckOutput",
              numErrors: 0n,
              suggestRepairIndex: false,
              suggestPrune: false,
              brokenPacks: [],
              readDataPercent: pct,
            },
          }
        : undefined,
      details: ok
        ? [
            ["Errors", "none"],
            ["Data read", pct ? `${pct}%` : "structure only"],
          ]
        : [],
    });
  }
  vault.repo.stats!.lastCheck = {
    $typeName: "syncr.v1.RepositoryJobRef",
    jobId: checks[0][3],
    status: JobStatus.SUCCEEDED,
    createdAt: ts(now - checks[0][0]),
    finishedAt: ts(now - checks[0][0] + 41 * MIN),
  };

  // One restore, done.
  job({
    id: ids.restore,
    kind: "restic_restore",
    status: JobStatus.SUCCEEDED,
    ago: 5 * DAY + 2 * HOUR,
    duration: 7 * MIN,
    worker: fx.ids.nas,
    definition: {
      case: "resticRestore",
      value: {
        repositoryId: vault.repo.id,
        snapshotId: vault.snapshots[5].id,
        target: "/srv/restore-photos",
        subfolder: "/srv/photos/2026/08",
        overwrite: ResticRestoreOverwrite.NEVER,
        verify: true,
      } as never,
    },
    title: "Restore /srv/photos/2026/08 from Photos vault → /srv/restore-photos",
    repo: vault,
    bytes: 1.9 * GiB,
    files: 312,
    output: {
      case: "resticRestore",
      value: {
        $typeName: "syncr.v1.ResticRestoreOutput",
        totalFiles: 318n,
        filesRestored: 312n,
        filesSkipped: 6n,
        filesDeleted: 0n,
        totalBytes: BigInt(Math.round(1.94 * GiB)),
        bytesRestored: BigInt(Math.round(1.9 * GiB)),
        bytesSkipped: BigInt(Math.round(0.04 * GiB)),
      },
    },
    details: [
      ["Restored", "312 files, 1.9 GiB"],
      ["Skipped", "6 files that were already there"],
      ["Existing files", "kept"],
    ],
  });

  // Live: a backup running on NAS documents and one waiting behind it.
  job({
    id: ids.runningBackup,
    kind: "restic_backup",
    status: JobStatus.RUNNING,
    ago: 3 * MIN,
    duration: 3 * MIN,
    worker: fx.ids.nas,
    definition: {
      case: "resticBackup",
      value: { repositoryId: nasDocs.repo.id, paths: ["/srv/docs"], tags: ["documents"] } as never,
    },
    title: "Back up /srv/docs → NAS documents",
    repo: nasDocs,
    bytes: 1.3 * GiB,
    total: 3.8 * GiB,
    files: 2_210,
  });
  job({
    id: ids.waitingBackup,
    kind: "restic_backup",
    status: JobStatus.QUEUED,
    ago: 70_000,
    worker: fx.ids.backupHost,
    definition: {
      case: "resticBackup",
      value: { repositoryId: nasDocs.repo.id, paths: ["/srv/backup/db"], tags: ["db"] } as never,
    },
    title: "Back up /srv/backup/db → NAS documents",
    repo: nasDocs,
    waitingFor: ids.runningBackup,
  });
  nasDocs.repo.stats!.lastBackup = {
    $typeName: "syncr.v1.RepositoryJobRef",
    jobId: ids.runningBackup,
    status: JobStatus.RUNNING,
    createdAt: ts(now - 3 * MIN),
  };

  // Old laptop: a check holds the repository, and the data deletion waits.
  job({
    id: ids.oldLaptopCheck,
    kind: "restic_check",
    status: JobStatus.RUNNING,
    ago: 11 * MIN,
    worker: fx.ids.nas,
    definition: {
      case: "resticCheck",
      value: { repositoryId: oldLaptop.repo.id, readDataPercent: 100 } as never,
    },
    title: "Check Old laptop",
    repo: oldLaptop,
  });
  job({
    id: ids.oldLaptopDelete,
    kind: "restic_delete_data",
    status: JobStatus.QUEUED,
    ago: 4 * MIN,
    worker: fx.ids.nas,
    definition: {
      case: "resticDeleteData",
      value: { repositoryId: oldLaptop.repo.id } as never,
    },
    title: "Delete the data of Old laptop",
    repo: oldLaptop,
    waitingFor: ids.oldLaptopCheck,
  });

  // History: one finished deletion of a forgotten repository, one failed.
  job({
    id: ids.retiredDelete,
    kind: "restic_delete_data",
    status: JobStatus.SUCCEEDED,
    ago: 12 * DAY,
    duration: 18 * MIN,
    worker: fx.ids.nas,
    definition: { case: "resticDeleteData", value: { repositoryId: ids.retiredNas } as never },
    title: "Delete the data of Retired NAS",
    repo: { id: ids.retiredNas, name: "Retired NAS", deleted: true },
    remote: fx.ids.storageBox,
    output: {
      case: "resticDeleteData",
      value: {
        $typeName: "syncr.v1.ResticDeleteDataOutput",
        deletedObjects: 48_213n,
        removedParts: ["locks", "snapshots", "index", "data", "keys", "config"],
        directoryRemoved: true,
        alreadyGone: false,
      },
    },
    details: [
      ["Objects deleted", "48,213"],
      ["Folder", "removed"],
    ],
  });
  job({
    id: ids.archiveDelete,
    kind: "restic_delete_data",
    status: JobStatus.FAILED,
    ago: 2 * DAY,
    duration: 40_000,
    worker: fx.ids.nas,
    definition: {
      case: "resticDeleteData",
      value: { repositoryId: archive.repo.id } as never,
    },
    title: "Delete the data of Archive 2019",
    repo: archive,
    error: [
      JobErrorCode.REPOSITORY_LOCKED,
      "another restic client is using the repository; nothing was deleted",
    ],
  });

  const sched = (p: {
    id: string;
    name: string;
    description?: string;
    cron: string;
    target: { case: "workerId"; value: string } | { case: "workerLabel"; value: string };
    definition: MessageInit<JobDefinition>["operation"];
    repo: RepoState;
    paused?: string;
  }): Schedule => {
    const enabled = !p.paused;
    const at = enabled ? next(p.cron, "Europe/Berlin", now) : undefined;
    return create(ScheduleSchema, {
      id: p.id,
      ownerId: fx.ids.admin,
      name: p.name,
      description: p.description ?? "",
      cron: p.cron,
      timezone: "Europe/Berlin",
      target: { target: p.target },
      missedRun: { action: MissedRunAction.RUN_LATE, maxDelay: durationFromMs(6 * HOUR) },
      overlap: OverlapPolicy.SKIP,
      enabled,
      pausedReason: p.paused ?? "",
      definition: { operation: p.definition },
      remotes: p.repo.repo.remote ? [p.repo.repo.remote] : [],
      repository: { id: p.repo.repo.id, name: p.repo.repo.name },
      nextRunAt: at ? ts(at) : undefined,
      createdAt: ts(now - 60 * DAY),
      updatedAt: ts(now - 20 * DAY),
      version: 2n,
    });
  };
  const schedules = [
    sched({
      id: ids.nightlyBackup,
      name: "Nightly photos backup",
      description: "Every night at two, on whichever NAS is up",
      cron: "0 2 * * *",
      target: { case: "workerLabel", value: "nas" },
      definition: {
        case: "resticBackup",
        value: {
          repositoryId: vault.repo.id,
          paths: ["/srv/photos"],
          tags: ["photos"],
          excludes: ["*.tmp", ".thumbnails"],
          excludeCaches: true,
        } as never,
      },
      repo: vault,
    }),
    sched({
      id: ids.weeklyRetention,
      name: "Weekly retention: photos",
      cron: "0 4 * * 0",
      target: { case: "workerId", value: fx.ids.nas },
      definition: {
        case: "resticForget",
        value: {
          repositoryId: vault.repo.id,
          policy: { keepDaily: 7, keepWeekly: 4, keepMonthly: 12 },
          prune: true,
        } as never,
      },
      repo: vault,
    }),
    sched({
      id: ids.monthlyCheck,
      name: "Monthly deep check: photos",
      cron: "0 5 1 * *",
      target: { case: "workerId", value: fx.ids.nas },
      definition: {
        case: "resticCheck",
        value: { repositoryId: vault.repo.id, readDataPercent: 10 } as never,
      },
      repo: vault,
    }),
    sched({
      id: ids.officeBackup,
      name: "Office documents backup",
      cron: "0 * * * *",
      target: { case: "workerId", value: fx.ids.office },
      definition: {
        case: "resticBackup",
        value: {
          repositoryId: office.repo.id,
          paths: ["C:\\Users\\ada\\Documents"],
          tags: ["office"],
          useFsSnapshot: true,
        } as never,
      },
      repo: office,
    }),
    sched({
      id: ids.oldLaptopBackup,
      name: "Old laptop weekly backup",
      cron: "0 3 * * 6",
      target: { case: "workerId", value: fx.ids.nas },
      definition: {
        case: "resticBackup",
        value: { repositoryId: oldLaptop.repo.id, paths: ["/srv/backup"] } as never,
      },
      repo: oldLaptop,
      paused: "repository_deleting",
    }),
    sched({
      id: ids.archiveCheck,
      name: "Archive 2019 yearly check",
      cron: "0 6 1 1 *",
      target: { case: "workerId", value: fx.ids.nas },
      definition: {
        case: "resticCheck",
        value: { repositoryId: archive.repo.id, readDataPercent: 0 } as never,
      },
      repo: archive,
      paused: "repository_deleting",
    }),
  ];

  return { repositories, schedules, jobs };
}

/** M5Deps is what the milestone 5 services share with the rest of the mock. */
export interface M5Deps {
  now: number;
  data: M5Data;
  me: () => User;
  jobs: Job[];
  workers: Worker[];
  schedules: Schedule[];
  remotes: () => Remote[];
  invalid: (v: Violation[]) => ConnectError;
  sleep: (ms: number) => Promise<void>;
  /** probeDelay is how long a probe takes (default 300 ms; tests use 0). */
  probeDelay?: number;
  /** timers turns the simulated progress of new jobs on (default on; tests turn it off). */
  timers?: boolean;
}

/** M5 is what the rest of the mock uses from milestone 5. */
export interface M5 {
  /** register adds ResticService and ProbeService to a mock router. */
  register: (router: ConnectRouter) => void;
  /**
   * check returns the problems of a restic definition, like the API: an
   * unknown repository, a kind users cannot start, a restore in a schedule,
   * VSS on a worker that is not Windows. It throws FAILED_PRECONDITION for
   * a repository whose data is being deleted.
   */
  check: (def: JobDefinition | undefined, forSchedule: boolean, workerId?: string) => Violation[];
  /** describe names a restic definition: its kind, title and repository. */
  describe: (def: JobDefinition | undefined) =>
    | {
        kind: string;
        title: string;
        repository: MessageInit<JobRepositoryRef>;
        remotes: { id: string; name: string; backendType: string }[];
      }
    | undefined;
  /** start runs a queued restic job: it waits for older jobs on the repository, then finishes. */
  start: (job: Job) => void;
  /** settle applies finished deletion jobs to their repositories. */
  settle: () => void;
}

const active = (s: JobStatus) =>
  s === JobStatus.QUEUED || s === JobStatus.DISPATCHED || s === JobStatus.RUNNING;

const cases: Record<string, string> = {
  resticBackup: "restic_backup",
  resticForget: "restic_forget",
  resticPrune: "restic_prune",
  resticCheck: "restic_check",
  resticRestore: "restic_restore",
  resticDeleteData: "restic_delete_data",
};

/** createM5 builds the milestone 5 part of the mock over shared state. */
export function createM5(d: M5Deps): M5 {
  const repos = d.data.repositories;
  const delay = () => d.sleep(d.probeDelay ?? 300);
  const trees = workerTrees();
  const remotes = remoteTrees();
  let seq = 0;
  const newId = (p: string) =>
    `${p}-${Date.now().toString(16).slice(-8).padStart(8, "0")}-4000-8000-${(++seq).toString(16).padStart(12, "0")}`;

  const mine = () => {
    const u = d.me();
    return repos.filter((r) => !r.deleted && r.ownerId === u.id);
  };
  const find = (id: string): RepoState => {
    const r = mine().find((x) => x.repo.id === id);
    if (!r) throw new ConnectError("repository not found", Code.NotFound);
    return r;
  };
  const deletingError = (r: RepoState) =>
    new ConnectError(
      `the repository's data is being deleted; cancel job ${r.repo.deletionJobId} first`,
      Code.FailedPrecondition,
    );
  const isLocal = (remoteId: string) =>
    !!fx.backends.find((b) => b.id === d.remotes().find((x) => x.id === remoteId)?.backendType)
      ?.capabilities?.local;

  // pickWorker returns the worker of a probe: the chosen one, which must
  // be reachable and current, or else the first usable one by name.
  const pickWorker = (workerId: string): Worker => {
    if (workerId) {
      const w = d.workers.find((x) => x.id === workerId);
      if (!w) throw new ConnectError("worker not found", Code.NotFound);
      if (w.status !== WorkerStatus.ONLINE)
        throw new ConnectError(`worker ${w.name} is offline`, Code.Unavailable);
      if (!w.info?.features.includes("probe"))
        throw new ConnectError(
          `worker ${w.name} is outdated: upgrade it to browse files and use restic`,
          Code.FailedPrecondition,
        );
      return w;
    }
    // Like the controller: a usable worker, by name; the demo's NAS first.
    const usable = d.workers
      .filter((x) => x.status === WorkerStatus.ONLINE && x.info?.features.includes("probe"))
      .sort((x, y) => x.name.localeCompare(y.name));
    const w = usable.find((x) => x.id === fx.ids.nas) ?? usable[0];
    if (!w)
      throw new ConnectError("no online worker can do this right now", Code.FailedPrecondition);
    return w;
  };
  const ref = (w: Worker) => ({ id: w.id, name: w.name });

  const stats = (r: RepoState): Repository => {
    const s = r.repo.stats!;
    const latest = r.snapshots[0];
    s.snapshotCount = r.snapshots.length;
    s.latestSnapshotAt = latest?.time;
    s.latestSnapshotBytes = latest?.summary?.totalBytesProcessed ?? 0n;
    s.snapshotsRefreshedAt = r.refreshedAt === undefined ? undefined : ts(r.refreshedAt);
    s.snapshotsStale = r.stale || r.refreshedAt === undefined;
    s.scheduleCount = d.schedules.filter((x) => x.repository?.id === r.repo.id).length;
    return r.repo;
  };

  const settle = () => {
    for (const r of repos) {
      if (!r.repo.deleting) continue;
      const j = d.jobs.find((x) => x.id === r.repo.deletionJobId);
      if (!j || active(j.status)) continue;
      r.repo.deleting = false;
      if (j.status === JobStatus.SUCCEEDED) {
        r.deleted = true;
        for (const x of d.jobs) if (x.repository?.id === r.repo.id) x.repository.deleted = true;
      } else if (j.status === JobStatus.CANCELLED) {
        r.repo.deletionError =
          "the deletion was cancelled; some repository files may already be gone";
      } else {
        r.repo.deletionError = `deleting the data failed: ${j.error?.message ?? "the job did not finish"}`;
      }
    }
  };

  const blocker = (j: Job): Job | undefined => {
    const repoId = j.repository?.id;
    if (!repoId) return undefined;
    const mineAt = timestampMs(j.createdAt ?? ts(0));
    return d.jobs
      .filter(
        (x) =>
          x.id !== j.id &&
          x.repository?.id === repoId &&
          active(x.status) &&
          timestampMs(x.createdAt ?? ts(0)) <= mineAt,
      )
      .at(-1);
  };

  const finish = (j: Job) => {
    if (j.status !== JobStatus.RUNNING) return;
    j.status = JobStatus.SUCCEEDED;
    j.finishedAt = ts(Date.now());
    const r = repos.find((x) => x.repo.id === j.repository?.id);
    const op = j.definition?.operation;
    if (!r || !op) return;
    r.stale = true;
    if (op.case === "resticBackup" && !op.value.dryRun) {
      const [snap] = snapshots(Date.now() + 1, {
        count: 1,
        every: DAY,
        first: 0,
        host: d.workers.find((w) => w.id === j.workerId)?.info?.hostname ?? "worker",
        user: "syncr",
        paths: op.value.paths,
        tags: op.value.tags,
        bytes: Number(r.snapshots[0]?.summary?.totalBytesProcessed ?? 2 * GiB),
        files: Number(r.snapshots[0]?.summary?.totalFilesProcessed ?? 1200),
        added: [8 * MiB, 240 * MiB],
        seed: Date.now() & 0xffff,
      });
      r.snapshots.unshift(snap);
      const sum = snap.summary!;
      j.output = {
        $typeName: "syncr.v1.JobOutput",
        output: {
          case: "resticBackup",
          value: {
            $typeName: "syncr.v1.ResticBackupOutput",
            snapshotId: snap.id,
            dataAddedBytes: sum.dataAddedBytes,
            filesNew: sum.filesNew,
            filesChanged: sum.filesChanged,
            filesUnmodified: sum.filesUnmodified,
            dirsNew: sum.dirsNew,
            dirsChanged: sum.dirsChanged,
            dirsUnmodified: sum.dirsUnmodified,
            dataAddedPackedBytes: sum.dataAddedPackedBytes,
            totalFilesProcessed: sum.totalFilesProcessed,
            totalBytesProcessed: sum.totalBytesProcessed,
          },
        },
      };
      j.details = [
        { $typeName: "syncr.v1.JobDetail", label: "Snapshot", value: snap.id.slice(0, 8) },
      ];
      r.repo.stats!.lastBackup = {
        $typeName: "syncr.v1.RepositoryJobRef",
        jobId: j.id,
        status: j.status,
        createdAt: j.createdAt,
        finishedAt: j.finishedAt,
      };
    } else if (op.case === "resticCheck") {
      j.output = {
        $typeName: "syncr.v1.JobOutput",
        output: {
          case: "resticCheck",
          value: {
            $typeName: "syncr.v1.ResticCheckOutput",
            numErrors: 0n,
            suggestRepairIndex: false,
            suggestPrune: false,
            brokenPacks: [],
            readDataPercent: op.value.readDataPercent,
          },
        },
      };
      r.repo.stats!.lastCheck = {
        $typeName: "syncr.v1.RepositoryJobRef",
        jobId: j.id,
        status: j.status,
        createdAt: j.createdAt,
        finishedAt: j.finishedAt,
      };
    } else if (op.case === "resticDeleteData") {
      j.output = {
        $typeName: "syncr.v1.JobOutput",
        output: {
          case: "resticDeleteData",
          value: {
            $typeName: "syncr.v1.ResticDeleteDataOutput",
            deletedObjects: 1284n,
            removedParts: ["locks", "snapshots", "index", "data", "keys", "config"],
            directoryRemoved: r.repo.path !== "",
            alreadyGone: false,
          },
        },
      };
    }
    settle();
  };

  const start = (j: Job) => {
    const b = blocker(j);
    j.waitingForJobId = b?.id ?? "";
    if (d.timers === false) return;
    const step = () => {
      if (j.status !== JobStatus.QUEUED) {
        settle();
        return;
      }
      const wait = blocker(j);
      j.waitingForJobId = wait?.id ?? "";
      if (wait) {
        setTimeout(step, 1000);
        return;
      }
      j.status = JobStatus.RUNNING;
      j.dispatchedAt = ts(Date.now());
      j.startedAt = ts(Date.now());
      setTimeout(() => finish(j), 6000);
    };
    setTimeout(step, 1500);
  };

  const describe: M5["describe"] = (def) => {
    const op = def?.operation;
    if (!op?.case || !(op.case in cases) || op.case === "rcloneTransfer") return undefined;
    const repoId = (op.value as { repositoryId: string }).repositoryId;
    const r = repos.find((x) => x.repo.id === repoId);
    const name = r?.repo.name ?? "a repository";
    let title = `${cases[op.case]} ${name}`;
    switch (op.case) {
      case "resticBackup":
        title = `Back up ${op.value.paths.join(", ")} → ${name}`;
        break;
      case "resticForget":
        title = `Apply retention to ${name}`;
        break;
      case "resticPrune":
        title = `Prune ${name}`;
        break;
      case "resticCheck":
        title = `Check ${name}`;
        break;
      case "resticRestore":
        title = `Restore ${op.value.subfolder || (op.value.paths.length ? `${op.value.paths.length} items` : `snapshot ${op.value.snapshotId.slice(0, 8)}`)} from ${name} → ${op.value.target}`;
        break;
      case "resticDeleteData":
        title = `Delete the data of ${name}`;
        break;
    }
    return {
      kind: cases[op.case],
      title,
      repository: { id: repoId, name, deleted: false },
      remotes: r?.repo.remote
        ? [
            {
              id: r.repo.remote.id,
              name: r.repo.remote.name,
              backendType: r.repo.remote.backendType,
            },
          ]
        : [],
    };
  };

  const check: M5["check"] = (def, forSchedule, workerId) => {
    const op = def?.operation;
    if (!op?.case || !(op.case in cases) || op.case === "rcloneTransfer") return [];
    const field = `definition.${cases[op.case]}`;
    if (op.case === "resticDeleteData")
      return [{ field, message: "is started by deleting the repository" }];
    if (op.case === "resticRestore" && forSchedule)
      return [{ field, message: "restores cannot be scheduled" }];
    const v: Violation[] = [];
    const repoId = (op.value as { repositoryId: string }).repositoryId;
    const r = mine().find((x) => x.repo.id === repoId);
    if (!r) v.push({ field: `${field}.repository_id`, message: "choose one of your repositories" });
    else if (r.repo.deleting)
      throw new ConnectError("the repository's data is being deleted", Code.FailedPrecondition);
    if (op.case === "resticBackup") {
      if (op.value.paths.length === 0)
        v.push({ field: `${field}.paths`, message: "add at least one path" });
      const w = d.workers.find((x) => x.id === workerId);
      if (op.value.useFsSnapshot && w?.info && w.info.os !== "windows")
        v.push({
          field: `${field}.use_fs_snapshot`,
          message: "file system snapshots need a Windows worker",
        });
    }
    if (op.case === "resticForget") {
      const p = op.value.policy;
      const any =
        !!p &&
        (p.keepLast + p.keepHourly + p.keepDaily + p.keepWeekly + p.keepMonthly + p.keepYearly >
          0 ||
          p.keepWithin !== "" ||
          p.keepTags.length > 0);
      if (!any) v.push({ field: `${field}.policy`, message: "set at least one keep rule" });
    }
    if (op.case === "resticRestore") {
      if (!op.value.target)
        v.push({ field: `${field}.target`, message: "choose the folder to restore into" });
      if (r && !r.snapshots.some((s) => s.id === op.value.snapshotId))
        v.push({ field: `${field}.snapshot_id`, message: "does not exist" });
    }
    return v;
  };

  const register = (router: ConnectRouter) => {
    router.service(ResticService, {
      listRepositories: (req) => {
        settle();
        return {
          repositories: mine()
            .filter((r) => !req.remoteId || r.repo.remoteId === req.remoteId)
            .map(stats)
            .sort((a, b) => a.name.localeCompare(b.name)),
        };
      },
      getRepository: (req) => {
        settle();
        return { repository: stats(find(req.repositoryId)) };
      },
      createRepository: async (req) => {
        const u = d.me();
        const input = req.input;
        if (!input) throw d.invalid([{ field: "input", message: "is required" }]);
        const v: Violation[] = [];
        const name = input.name.trim();
        if (!name) v.push({ field: "name", message: "is required" });
        else if (name.length > 64)
          v.push({ field: "name", message: "is longer than 64 characters" });
        const remote = u.id === fx.ids.admin && d.remotes().find((x) => x.id === input.remoteId);
        const backend = remote ? fx.backends.find((b) => b.id === remote.backendType) : undefined;
        if (!remote) v.push({ field: "remote_id", message: "choose one of your remotes" });
        else if (!backend?.capabilities?.resticRepository)
          v.push({
            field: "remote_id",
            message: "this kind of remote cannot hold a restic repository",
          });
        if (!input.password) v.push({ field: "password", message: "is required" });
        if (input.mode !== RepositoryCreateMode.INIT && input.mode !== RepositoryCreateMode.CONNECT)
          v.push({ field: "mode", message: "choose Create or Connect" });
        if (remote && backend?.capabilities?.local && !input.workerId)
          v.push({
            field: "worker_id",
            message: "choose the worker whose disk holds this repository",
          });
        if (v.length) throw d.invalid(v);
        if (!remote) throw d.invalid(v);
        const path = input.path.replace(/^\/+|\/+$/g, "");
        if (mine().some((r) => r.repo.name.toLowerCase() === name.toLowerCase()))
          throw new ConnectError("a repository with this name already exists", Code.AlreadyExists);
        if (mine().some((r) => r.repo.remoteId === remote.id && r.repo.path === path))
          throw new ConnectError(
            "a repository at this location already exists",
            Code.AlreadyExists,
          );
        const w = pickWorker(input.workerId);
        await delay();
        const at = walk(remotes[remote.id] ?? {}, path ? path.split("/") : []);
        const exists = !!at && "config" in at;
        if (input.mode === RepositoryCreateMode.INIT && exists)
          throw d.invalid([
            { field: "mode", message: "a repository already exists here; choose Connect" },
          ]);
        if (input.mode === RepositoryCreateMode.CONNECT && !exists)
          throw d.invalid([{ field: "path", message: "no restic repository at this location" }]);
        if (input.mode === RepositoryCreateMode.CONNECT && input.password.startsWith("wrong"))
          throw d.invalid([{ field: "password", message: "is not this repository's password" }]);
        const state: RepoState = {
          ownerId: u.id,
          snapshots: [],
          tree: {},
          refreshedAt: undefined,
          stale: true,
          deleted: false,
          repo: create(RepositorySchema, {
            id: newId("7d3a1c9e"),
            name,
            description: input.description.trim(),
            remoteId: remote.id,
            path,
            remote: { id: remote.id, name: remote.name, backendType: remote.backendType },
            resticId: hex(rand(Date.now() & 0xfffff), 64),
            formatVersion: 2,
            createdAt: ts(Date.now()),
            updatedAt: ts(Date.now()),
            version: 1n,
            stats: { snapshotsStale: true },
          }),
        };
        repos.push(state);
        return { repository: stats(state), worker: ref(w) };
      },
      updateRepository: async (req) => {
        const r = find(req.repositoryId);
        if (r.repo.deleting) throw deletingError(r);
        if (req.version !== r.repo.version)
          throw new ConnectError("the repository was changed in the meantime", Code.Aborted);
        const name = req.name.trim();
        if (!name) throw d.invalid([{ field: "name", message: "is required" }]);
        if (mine().some((x) => x !== r && x.repo.name.toLowerCase() === name.toLowerCase()))
          throw new ConnectError("a repository with this name already exists", Code.AlreadyExists);
        if (req.password) {
          if (isLocal(r.repo.remoteId) && !req.workerId)
            throw d.invalid([
              {
                field: "worker_id",
                message: "choose the worker whose disk holds this repository",
              },
            ]);
          pickWorker(req.workerId);
          await delay();
          if (req.password.startsWith("wrong"))
            throw d.invalid([{ field: "password", message: "is not this repository's password" }]);
        }
        r.repo.name = name;
        r.repo.description = req.description.trim();
        r.repo.version++;
        r.repo.updatedAt = ts(Date.now());
        for (const j of d.jobs) if (j.repository?.id === r.repo.id) j.repository.name = name;
        for (const s of d.schedules) if (s.repository?.id === r.repo.id) s.repository.name = name;
        return { repository: stats(r) };
      },
      deleteRepository: (req) => {
        const u = d.me();
        const r = find(req.repositoryId);
        if (!req.deleteData) {
          if (r.repo.deleting) throw deletingError(r);
          r.deleted = true;
          for (const j of d.jobs) if (j.repository?.id === r.repo.id) j.repository.deleted = true;
          for (const s of d.schedules) {
            if (s.repository?.id !== r.repo.id) continue;
            s.repository.deleted = true;
            s.enabled = false;
            s.pausedReason = "repository_unavailable";
            s.nextRunAt = undefined;
          }
          return {};
        }
        if (req.confirmName !== r.repo.name)
          throw d.invalid([
            { field: "confirm_name", message: "type the repository's name to confirm" },
          ]);
        if (r.repo.deleting) {
          const j = d.jobs.find((x) => x.id === r.repo.deletionJobId);
          const w = d.workers.find((x) => x.id === j?.workerId);
          return { deletionJobId: r.repo.deletionJobId, worker: w ? ref(w) : undefined };
        }
        if (isLocal(r.repo.remoteId) && !req.workerId)
          throw d.invalid([
            { field: "worker_id", message: "choose the worker whose disk holds this repository" },
          ]);
        const w = pickWorker(req.workerId);
        if (!w.info?.features.includes("restic.delete"))
          throw new ConnectError(
            `worker ${w.name} is outdated: upgrade it to delete repository data`,
            Code.FailedPrecondition,
          );
        const def = {
          operation: { case: "resticDeleteData" as const, value: { repositoryId: r.repo.id } },
        };
        const j = create(JobSchema, {
          id: newId("e5f4a3b2"),
          workerId: w.id,
          kind: "restic_delete_data",
          status: JobStatus.QUEUED,
          ownerId: u.id,
          createdAt: ts(Date.now()),
          definition: def,
          title: `Delete the data of ${r.repo.name}`,
          repository: { id: r.repo.id, name: r.repo.name },
          remotes: r.repo.remote ? [r.repo.remote] : [],
        });
        d.jobs.unshift(j);
        r.repo.deleting = true;
        r.repo.deletionJobId = j.id;
        r.repo.deletionRequestedAt = ts(Date.now());
        r.repo.deletionError = "";
        for (const s of d.schedules) {
          if (s.repository?.id !== r.repo.id || !s.enabled) continue;
          s.enabled = false;
          s.pausedReason = "repository_deleting";
          s.nextRunAt = undefined;
        }
        start(j);
        return { deletionJobId: j.id, worker: ref(w) };
      },
      listSnapshots: async (req) => {
        const r = find(req.repositoryId);
        let worker: Worker | undefined;
        if (req.refresh) {
          if (r.repo.deleting) throw deletingError(r);
          worker = pickWorker(req.workerId);
          await delay();
          r.refreshedAt = Date.now();
          r.stale = false;
        }
        return {
          snapshots: r.snapshots,
          refreshedAt: r.refreshedAt === undefined ? undefined : ts(r.refreshedAt),
          stale: r.stale || r.refreshedAt === undefined,
          truncated: false,
          refreshError: "",
          worker: worker ? ref(worker) : undefined,
        };
      },
      browseSnapshot: async (req) => {
        const r = find(req.repositoryId);
        if (r.repo.deleting) throw deletingError(r);
        const w = pickWorker(req.workerId);
        await delay();
        if (!r.snapshots.some((s) => s.id === req.snapshotId || s.id.startsWith(req.snapshotId)))
          throw d.invalid([{ field: "path", message: "does not exist" }]);
        const parts = req.path.split("/").filter(Boolean);
        const at = walk(r.tree, parts);
        if (!at) throw d.invalid([{ field: "path", message: "does not exist" }]);
        const base = `/${parts.join("/")}`;
        return {
          path: base,
          entries: entries(at, d.now, (n) => (base === "/" ? `/${n}` : `${base}/${n}`)),
          truncated: false,
          worker: ref(w),
        };
      },
    });

    const listLocal = (w: Worker, path: string, dirsOnly: boolean): ProbeEntry[] => {
      const roots = trees[w.id] ?? {};
      const windows = w.info?.os === "windows";
      const sep = windows ? "\\" : "/";
      if (path === "") {
        return (w.info?.localRoots ?? []).map((root) =>
          create(ProbeEntrySchema, {
            name: root,
            path: root,
            type: ProbeEntryType.DIR,
            mode: "drwxr-xr-x",
          }),
        );
      }
      const norm = (p: string) => (windows ? p.toLowerCase() : p).replace(/[\\/]+$/, "");
      const root = (w.info?.localRoots ?? []).find(
        (x) => norm(path) === norm(x) || norm(path).startsWith(norm(x) + sep),
      );
      if (!root)
        throw d.invalid([{ field: "path", message: "is outside the worker's allowed folders" }]);
      const rest = path.slice(root.length).split(sep).filter(Boolean);
      const at = walk(roots[root] ?? {}, rest);
      if (!at) throw d.invalid([{ field: "path", message: "does not exist" }]);
      const base = [root.replace(/[\\/]+$/, ""), ...rest].join(sep);
      return entries(at, d.now, (n) => `${base}${sep}${n}`, dirsOnly);
    };

    router.service(ProbeService, {
      listWorkerFiles: async (req) => {
        d.me();
        if (!req.workerId) throw d.invalid([{ field: "worker_id", message: "is required" }]);
        const w = pickWorker(req.workerId);
        await delay();
        return {
          path: req.path,
          entries: listLocal(w, req.path, req.dirsOnly),
          truncated: false,
          worker: ref(w),
        };
      },
      listRemoteFiles: async (req) => {
        const u = d.me();
        const remote = u.id === fx.ids.admin && d.remotes().find((x) => x.id === req.remoteId);
        if (!remote) throw new ConnectError("remote not found", Code.NotFound);
        if (isLocal(remote.id) && !req.workerId)
          throw d.invalid([
            { field: "worker_id", message: "choose the worker whose disk holds this remote" },
          ]);
        const w = pickWorker(req.workerId);
        await delay();
        const parts = req.path.split("/").filter(Boolean);
        const at = walk(remotes[remote.id] ?? { data: {} }, parts);
        if (!at) throw d.invalid([{ field: "path", message: "does not exist" }]);
        const base = parts.join("/");
        return {
          path: base,
          entries: entries(at, d.now, (n) => (base ? `${base}/${n}` : n), req.dirsOnly),
          truncated: false,
          worker: ref(w),
        };
      },
      testRemote: async (req) => {
        const u = d.me();
        const stored = req.remoteId
          ? u.id === fx.ids.admin && d.remotes().find((x) => x.id === req.remoteId)
          : undefined;
        if (req.remoteId && !stored) throw new ConnectError("remote not found", Code.NotFound);
        if (!stored && !req.input) throw d.invalid([{ field: "input", message: "is required" }]);
        const backendType = (stored || undefined)?.backendType ?? req.input?.backendType ?? "";
        const local = !!fx.backends.find((b) => b.id === backendType)?.capabilities?.local;
        if (local && !req.workerId)
          throw d.invalid([
            { field: "worker_id", message: "choose the worker whose disk holds this remote" },
          ]);
        const chosen = req.workerId ? d.workers.find((x) => x.id === req.workerId) : undefined;
        if (chosen && chosen.status !== WorkerStatus.ONLINE)
          return {
            ok: false,
            error: `worker ${chosen.name} is offline`,
            errorCode: "unavailable",
            worker: ref(chosen),
          };
        const usable = d.workers.filter(
          (x) => x.status === WorkerStatus.ONLINE && x.info?.features.includes("probe"),
        );
        if (!chosen && usable.length === 0)
          return {
            ok: false,
            error: "no online worker can test remotes right now",
            errorCode: "no_worker",
          };
        const w = chosen ?? pickWorker("");
        await delay();
        const id = stored ? stored.id : "";
        if (id === fx.ids.nextcloud)
          return {
            ok: false,
            error: "couldn't list the remote: dial tcp 192.0.2.40:443: connect: connection refused",
            errorCode: "execution_failed",
            worker: ref(w),
            latency: durationFromMs(1840),
          };
        const tree = remotes[id] ?? { data: {}, "README.txt": 1200 };
        const names = Object.keys(tree);
        return {
          ok: true,
          worker: ref(w),
          latency: durationFromMs(id === fx.ids.b2 ? 412 : 96),
          entries: names.length,
          sample: names.slice(0, 5),
          truncated: false,
          usage:
            id === fx.ids.minio || !stored
              ? { unsupported: true }
              : local
                ? {
                    total: BigInt(8 * 1024 * GiB),
                    used: BigInt(Math.round(5.2 * 1024 * GiB)),
                    free: BigInt(Math.round(2.8 * 1024 * GiB)),
                  }
                : {
                    total: BigInt(1024 * GiB),
                    used: BigInt(Math.round(318.6 * GiB)),
                    free: BigInt(Math.round(705.4 * GiB)),
                    objects: 184_230n,
                  },
        };
      },
    });
  };

  return { register, check, describe, start, settle };
}
