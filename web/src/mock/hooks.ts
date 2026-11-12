/**
 * Hooks part of the dev mock (VITE_SYNCR_MOCK=1) and of the test APIs: the
 * hooks nas-01's operator defined, jobs that ran them, and the controller's
 * checks of the hooks a definition names (RunJob and schedules on a
 * worker).
 */
import { create } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromMs } from "@bufbuild/protobuf/wkt";

import {
  JobSchema,
  JobStatus,
  type Job,
  type JobDefinition,
  type Worker,
} from "@/gen/syncr/v1/api_pb";
import { HookPhase, HookStatus, JobErrorCode } from "@/gen/syncr/v1/link_pb";
import * as fx from "@/test/fixtures";

import { ids as m5ids } from "./m5";

const MIN = 60_000;
const HOUR = 60 * MIN;
const GiB = 1024 ** 3;

/** FEATURE is the link feature of a worker that runs hooks. */
export const FEATURE = "job.hooks";

/** ids of the demo jobs with hooks. */
export const ids = {
  hookFailedBackup: "e5f4a3b2-1c0d-4e9f-8a7b-6c5d4e3f2b01",
  hookedBackup: "e5f4a3b2-1c0d-4e9f-8a7b-6c5d4e3f2b02",
};

/** nasHooks are the hooks nas-01 defines. */
export const nasHooks = [
  {
    name: "dump-paperless-db",
    description: "Dump the Paperless database to /srv/paperless/db",
    timeout: durationFromMs(15 * MIN),
  },
  {
    name: "ping-healthcheck",
    description: "Tell the uptime monitor that the backup ran",
    timeout: durationFromMs(30_000),
  },
  {
    name: "start-paperless",
    description: "Start the Paperless containers again",
    timeout: durationFromMs(2 * MIN),
  },
  {
    name: "stop-paperless",
    description: "Stop the Paperless containers, so their files stay still",
    timeout: durationFromMs(2 * MIN),
  },
];

/**
 * applyWorkers makes the up-to-date workers report the hooks feature (the
 * ones the controller calls outdated stay without it) and gives nas-01 its
 * hooks.
 */
export function applyWorkers(workers: Worker[]): void {
  for (const w of workers) {
    if (!w.info || w.outdated || w.info.features.length === 0) continue;
    if (!w.info.features.includes(FEATURE)) w.info.features.push(FEATURE);
    if (w.id === fx.ids.nas) {
      w.info.hooks = nasHooks.map((h) => ({ $typeName: "syncr.v1.HookInfo" as const, ...h }));
    }
  }
}

const hooks = {
  before: ["stop-paperless", "dump-paperless-db"],
  onSuccess: ["ping-healthcheck"],
  onFailure: [],
  after: ["start-paperless"],
};

function backup(now: number, p: { id: string; ago: number; failed: boolean }): Job {
  const started = now - p.ago;
  const dump = 41_000;
  const run = 6 * MIN + 12_000;
  const finished = started + 3_200 + dump + run + (p.failed ? 2 * MIN : 4_900);
  const at = (ms: number) => timestampFromMs(started + ms);
  return create(JobSchema, {
    id: p.id,
    workerId: fx.ids.nas,
    kind: "restic_backup",
    status: p.failed ? JobStatus.FAILED : JobStatus.SUCCEEDED,
    ownerId: fx.ids.admin,
    createdAt: timestampFromMs(started - 6_000),
    dispatchedAt: timestampFromMs(started - 1_500),
    startedAt: timestampFromMs(started),
    finishedAt: timestampFromMs(finished),
    title: "Back up /srv/paperless → NAS documents",
    repository: { id: m5ids.nasDocs, name: "NAS documents", deleted: false },
    remotes: [{ id: fx.ids.minio, name: "MinIO lab", backendType: "s3" }],
    exitCode: 0,
    stats: {
      bytes: BigInt(Math.round(38.4 * GiB)),
      totalBytes: BigInt(Math.round(38.4 * GiB)),
      files: 61_204n,
      totalFiles: 61_204n,
      elapsed: durationFromMs(run),
    },
    definition: {
      hooks,
      operation: {
        case: "resticBackup",
        value: {
          repositoryId: m5ids.nasDocs,
          paths: ["/srv/paperless"],
          tags: ["documents"],
        },
      },
    },
    error: p.failed
      ? {
          code: JobErrorCode.HOOK_FAILED,
          message: 'hook "start-paperless" (after) timed out after 2m0s',
        }
      : undefined,
    output: {
      output: {
        case: "resticBackup",
        value: {
          snapshotId: p.failed
            ? "7c1e02d9a4f35b68c0de1f2a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e"
            : "2b9d44f0c7a18e36d5f2a1b0c9e8d7f6a5b4c3d2e1f0a9b8c7d6e5f4a3b2c1d0",
          dataAddedBytes: BigInt(Math.round(0.6 * GiB)),
          filesNew: 212n,
          filesChanged: 1_840n,
          filesUnmodified: 59_152n,
          totalFilesProcessed: 61_204n,
          totalBytesProcessed: BigInt(Math.round(38.4 * GiB)),
        },
      },
    },
    details: [
      { label: "Snapshot", value: p.failed ? "7c1e02d9" : "2b9d44f0" },
      { label: "Files", value: "212 new, 1,840 changed, 59,152 unmodified" },
      { label: "Data added", value: "614 MiB" },
    ],
    hookRuns: [
      {
        name: "stop-paperless",
        phase: HookPhase.BEFORE,
        status: HookStatus.SUCCEEDED,
        startedAt: at(0),
        duration: durationFromMs(3_200),
        output: "paperless-web\npaperless-worker",
      },
      {
        name: "dump-paperless-db",
        phase: HookPhase.BEFORE,
        status: HookStatus.SUCCEEDED,
        startedAt: at(3_200),
        duration: durationFromMs(dump),
        output:
          'pg_dump: dumping contents of table "public.documents_document"\npg_dump: dumping contents of table "public.documents_note"\nwrote /srv/paperless/db/paperless.dump (312 MiB)',
        outputTruncated: true,
      },
      {
        name: "ping-healthcheck",
        phase: HookPhase.ON_SUCCESS,
        status: HookStatus.SUCCEEDED,
        startedAt: at(3_200 + dump + run),
        duration: durationFromMs(380),
        output: "OK",
      },
      p.failed
        ? {
            name: "start-paperless",
            phase: HookPhase.AFTER,
            status: HookStatus.TIMED_OUT,
            exitCode: -1,
            startedAt: at(3_200 + dump + run + 380),
            duration: durationFromMs(2 * MIN),
            message: "timed out after 2m0s",
            output:
              "paperless-worker\nError response from daemon: driver failed programming external connectivity on endpoint paperless-web: Bind for 0.0.0.0:8000 failed: port is already allocated",
          }
        : {
            name: "start-paperless",
            phase: HookPhase.AFTER,
            status: HookStatus.SUCCEEDED,
            startedAt: at(3_200 + dump + run + 380),
            duration: durationFromMs(4_500),
            output: "paperless-web\npaperless-worker",
          },
    ],
  });
}

/**
 * jobs returns two backups of the NAS documents that stop the document app, dump its database,
 * back up and start the app again: last night's, and one whose start hook timed
 * out after the backup was written (failed by HOOK_FAILED, with its
 * snapshot).
 */
export function jobs(now: number): Job[] {
  return [
    backup(now, { id: ids.hookedBackup, ago: 7 * HOUR, failed: false }),
    backup(now, { id: ids.hookFailedBackup, ago: 31 * HOUR, failed: true }),
  ];
}

/** Violation is one field problem, as the controller reports it. */
export interface Violation {
  field: string;
  message: string;
}

const phases = [
  ["before", "before"],
  ["onSuccess", "on_success"],
  ["onFailure", "on_failure"],
  ["after", "after"],
] as const;

/**
 * check returns the problems of the hooks a definition names, like the
 * API: names that are none, and, for a job on one worker that has reported
 * in, a worker that cannot run hooks or does not define one. A label's
 * workers are not checked (workerId empty).
 */
export function check(
  def: JobDefinition | undefined,
  workers: Worker[],
  workerId: string | undefined,
): Violation[] {
  const h = def?.hooks;
  if (!h) return [];
  const out: Violation[] = [];
  const named = new Map<string, string>(); // name → the first field naming it
  for (const [key, field] of phases) {
    h[key].forEach((name, i) => {
      const path = `definition.hooks.${field}[${i}]`;
      if (!/^[a-z0-9][a-z0-9._-]{0,62}$/.test(name)) {
        out.push({
          field: path,
          message:
            "is not a hook name: 1 to 63 lower-case letters, digits, '.', '_' and '-', starting with a letter or digit",
        });
      } else if (h[key].indexOf(name) < i) {
        out.push({ field: path, message: "is listed twice" });
      } else if (!named.has(name)) {
        named.set(name, path);
      }
    });
  }
  const w = workers.find((x) => x.id === workerId);
  if (out.length || !w?.info || named.size === 0) return out;
  if (!w.info.features.includes(FEATURE)) {
    return [
      {
        field: "definition.hooks",
        message: `cannot be used on worker "${w.name}" yet: it must be upgraded to run hooks`,
      },
    ];
  }
  const defined = new Set(w.info.hooks.map((x) => x.name));
  for (const [name, field] of named) {
    if (!defined.has(name)) {
      out.push({
        field,
        message: `worker "${w.name}" defines no hook named "${name}": its operator adds hooks in the worker's configuration`,
      });
    }
  }
  return out;
}
