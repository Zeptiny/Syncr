/**
 * Milestone 6 part of the dev mock (VITE_SYNCR_MOCK=1) and of the test
 * APIs: SFTP host keys fetched through a worker (ProbeService.
 * GetSftpHostKeys), worker upgrade commands (WorkerAdminService.
 * GetWorkerUpgradeCommands), outdated workers, job time limits and thinned
 * job samples (ListJobSamples.max_points and total).
 */
import { create } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, type ConnectRouter } from "@connectrpc/connect";

import {
  InstallPlatform,
  InstallScope,
  JobSchema,
  JobStatus,
  WorkerAdminService,
  WorkerSchema,
  WorkerStatus,
  type InstallCommand,
  type Job,
  type JobProgress,
  type Worker,
} from "@/gen/syncr/v1/api_pb";
import {
  JobErrorCode,
  RcloneTransferMode,
  SftpHostKeySchema,
  WorkerMode,
} from "@/gen/syncr/v1/link_pb";
import { ProbeService } from "@/gen/syncr/v1/probe_pb";
import { RemoteSchema, type Remote } from "@/gen/syncr/v1/remotes_pb";
import { UserRole, type User } from "@/gen/syncr/v1/users_pb";
import { thinSamples } from "@/hooks/use-watch-job";
import * as fx from "@/test/fixtures";

import { ids as m5ids } from "./m5";

const MIN = 60_000;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;
const GiB = 1024 ** 3;

/** CONTROLLER_VERSION is the mock controller's release. */
export const CONTROLLER_VERSION = "v2.0.0";

/** ids of the milestone 6 demo objects. */
export const ids = {
  verifiedBox: "9e7d6c5b-2a1f-4b3c-8d9e-0f1a2b3c4d61",
  timeLimitJob: "e5f4a3b2-1c0d-4e9f-8a7b-6c5d4e3f2a61",
  atticNas: "5a1e2b7c-1d3f-4e8a-9b0c-2d4f6a8b0c61",
};

/** hostKeys are the keys the mock SSH server offers (valid wire-format blobs). */
export const hostKeys = [
  create(SftpHostKeySchema, {
    algorithm: "ssh-ed25519",
    key: "AAAAC3NzaC1lZDI1NTE5AAAAIMUiJpUbcNynH9rHhq4iK4jdXA7Qg/xNVqitUwiNweax",
    fingerprintSha256: "SHA256:dkA939jgBPkIxhIk1YrFbVK6S3sjJRKuDnTe/EF7P54",
  }),
  create(SftpHostKeySchema, {
    algorithm: "ecdsa-sha2-nistp256",
    key: "AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBH9xAeBCuXqzBfh9cf86Gd6fiPvC0DUhn7/y6t//sQHCSwUg+aU5NxZuU/ADvvNXRTI5S3AoUmxGN7kbZLiU6XQ=",
    fingerprintSha256: "SHA256:Lu0iomEI1gJTzrNNAEPx/kPI7GZriuysj84Pproxz/I",
  }),
];

/** TIME_LIMIT_MESSAGE is what a worker reports when a job's time limit ends it. */
export const TIME_LIMIT_MESSAGE = "stopped after 6h0m0s: the job's time limit was reached";

/**
 * applyWorkers marks the demo's outdated workers: old-nas lacks the
 * probe features, and backup-host runs a release candidate while the
 * controller runs v2.0.0. Current workers also answer sftp_host_keys.
 */
export function applyWorkers(workers: Worker[]): void {
  for (const w of workers) {
    if (!w.info) continue;
    if (w.info.probeOps.length && !w.info.probeOps.includes("sftp_host_keys"))
      w.info.probeOps.push("sftp_host_keys");
    if (w.id === m5ids.oldNas) w.outdated = true;
    if (w.id === fx.ids.backupHost) {
      w.info.workerVersion = "v2.0.0-rc.1";
      w.outdated = true;
    }
  }
}

/**
 * atticWorker is an online worker the controller calls outdated: a release
 * candidate without the job.time_limit feature (the screenshots' upgrade
 * card).
 */
export function atticWorker(now: number): Worker {
  return create(WorkerSchema, {
    id: ids.atticNas,
    name: "attic-nas",
    labels: [],
    mode: WorkerMode.ACTIVE,
    status: WorkerStatus.ONLINE,
    lastSeen: timestampFromMs(now - 6_000),
    createdAt: timestampFromMs(now - 90 * DAY),
    outdated: true,
    info: {
      workerVersion: "v2.0.0-rc.1",
      os: "linux",
      arch: "amd64",
      hostname: "attic-nas",
      tools: [
        { name: "rclone", version: "v1.71.2" },
        { name: "restic", version: "0.18.1" },
      ],
      jobKinds: ["rclone_transfer", "restic_backup"],
      maxConcurrentJobs: 2,
      localRoots: ["/srv/attic"],
      features: ["probe", "restic.v2", "restic.delete"],
      reportedAt: timestampFromMs(now - 6_000),
    },
  });
}

/** verifiedRemote is an sftp remote with pinned host keys ("Storage box" has none). */
export function verifiedRemote(now: number): Remote {
  return create(RemoteSchema, {
    id: ids.verifiedBox,
    name: "Offsite SSH",
    backendType: "sftp",
    options: {
      host: "backup.example.net",
      port: "22",
      user: "syncr",
      host_keys: hostKeys.map((k) => `${k.algorithm} ${k.key}`).join("\n"),
    },
    secretsSet: ["key_pem"],
    createdAt: timestampFromMs(now - 12 * DAY),
    updatedAt: timestampFromMs(now - 2 * DAY),
    version: 1n,
  });
}

/**
 * timeLimitJob is a transfer that ran into its 6-hour time limit and
 * failed, with a long, thinned progress history.
 */
export function timeLimitJob(now: number): { job: Job; samples: JobProgress[] } {
  const started = now - 9 * HOUR;
  const finished = started + 6 * HOUR;
  const bytes = 612 * GiB;
  const job = create(JobSchema, {
    id: ids.timeLimitJob,
    workerId: fx.ids.nas,
    kind: "rclone_transfer",
    status: JobStatus.FAILED,
    ownerId: fx.ids.admin,
    createdAt: timestampFromMs(started - 20_000),
    dispatchedAt: timestampFromMs(started - 2_000),
    startedAt: timestampFromMs(started),
    finishedAt: timestampFromMs(finished),
    stats: {
      bytes: BigInt(bytes),
      totalBytes: BigInt(1434 * GiB),
      files: 18_204n,
      totalFiles: 40_112n,
      checks: 18_190n,
      elapsed: durationFromMs(6 * HOUR),
    },
    error: { code: JobErrorCode.TIME_LIMIT, message: TIME_LIMIT_MESSAGE },
    timeLimit: durationFromMs(6 * HOUR),
    definition: {
      maxRuntime: durationFromMs(6 * HOUR),
      operation: {
        case: "rcloneTransfer",
        value: {
          mode: RcloneTransferMode.SYNC,
          source: { remoteId: fx.ids.photos, path: "raw" },
          destination: { remoteId: fx.ids.b2, path: "photos-raw" },
          options: {},
        },
      },
    },
    remotes: [
      { id: fx.ids.photos, name: "Photos", backendType: "local" },
      { id: fx.ids.b2, name: "B2 offsite", backendType: "b2" },
    ],
    details: [{ label: "Time limit", value: "6h" }],
  });
  return { job, samples: fx.samples(finished, 6 * HOUR, bytes) };
}

/** listSamples answers ListJobSamples like the controller: thinned to maxPoints, with the total. */
export function listSamples(all: JobProgress[], maxPoints: number) {
  if (maxPoints < 0)
    throw new ConnectError("max_points must not be negative", Code.InvalidArgument);
  const m = Math.min(maxPoints || 1000, 5000);
  return { samples: thinSamples(all, m), total: all.length };
}

/**
 * upgradeCommands renders a worker's upgrade commands like the
 * controller's install.go for a release build.
 */
export function upgradeCommands(w: Worker, version = CONTROLLER_VERSION): InstallCommand[] {
  const os = w.info?.os;
  const arch = w.info?.arch ?? "";
  const keep =
    " Running jobs are interrupted by the restart. If the new version does not stay up, the old one is put back.";
  const note = ` Installs ${version}, the controller's version.`;
  const v = ` --version '${version}'`;
  const cmd = (
    platform: InstallPlatform,
    scope: InstallScope,
    title: string,
    command: string,
    text: string,
  ) =>
    ({
      $typeName: "syncr.v1.InstallCommand",
      platform,
      scope,
      shell: platform === InstallPlatform.WINDOWS ? "powershell" : "sh",
      title,
      command,
      note: text + note + keep,
    }) satisfies InstallCommand;
  if (os === "linux" && ["amd64", "arm64", ""].includes(arch)) {
    return [
      cmd(
        InstallPlatform.LINUX,
        InstallScope.SYSTEM,
        "Linux (system service)",
        `sudo /usr/local/bin/syncr-worker upgrade${v}`,
        "Needs sudo. Replaces /usr/local/bin/syncr-worker and restarts the system service.",
      ),
      cmd(
        InstallPlatform.LINUX,
        InstallScope.USER,
        "Linux (current user)",
        `"$HOME/.local/bin/syncr-worker" upgrade --user${v}`,
        "Run as the user the worker runs as. Replaces ~/.local/bin/syncr-worker and restarts that user's service.",
      ),
    ];
  }
  if (os === "windows" && ["amd64", ""].includes(arch)) {
    return [
      cmd(
        InstallPlatform.WINDOWS,
        InstallScope.SYSTEM,
        "Windows (system service)",
        `$d = Join-Path $env:ProgramFiles 'Syncr'\n& "$d\\syncr-worker.exe" upgrade${v}`,
        "Run in an elevated PowerShell (Run as administrator). Replaces the worker in Program Files and restarts the Windows service.",
      ),
      cmd(
        InstallPlatform.WINDOWS,
        InstallScope.USER,
        "Windows (current user)",
        `$d = Join-Path $env:LOCALAPPDATA 'Programs\\Syncr'\n& "$d\\syncr-worker.exe" upgrade --user${v}`,
        "Run in a normal PowerShell as the user the worker runs as. Replaces the worker in that user's programs folder and restarts its scheduled task.",
      ),
    ];
  }
  return [];
}

/** M6Deps is the shared state the milestone 6 handlers read. */
export interface M6Deps {
  me: () => User;
  workers: Worker[];
  invalid: (v: { field: string; message: string }[]) => ConnectError;
  sleep: (ms: number) => Promise<void>;
  /** probeDelay is how long a probe takes (default 400 ms; tests use 0). */
  probeDelay?: number;
}

const hostRE = /^[A-Za-z0-9]([A-Za-z0-9.:_-]*[A-Za-z0-9])?$/;

/**
 * registerM6 adds GetSftpHostKeys and GetWorkerUpgradeCommands to a mock
 * router. Register it after the services it extends: the last handler of
 * a method wins.
 *
 * The mock SSH server: a host ending in ".invalid" cannot be reached, and
 * "web.example.net" answers but does not speak SSH (both
 * FAILED_PRECONDITION); every other host offers hostKeys.
 */
export function registerM6(router: ConnectRouter, d: M6Deps): void {
  const delay = () => d.sleep(d.probeDelay ?? 400);
  router.rpc(ProbeService.method.getSftpHostKeys, async (req) => {
    d.me();
    const host = req.host.trim();
    if (!host) throw d.invalid([{ field: "host", message: "is required" }]);
    if (host.startsWith("-") || !hostRE.test(host))
      throw d.invalid([{ field: "host", message: "is not a valid host name or address" }]);
    if (req.port < 0 || req.port > 65535)
      throw d.invalid([{ field: "port", message: "must be between 1 and 65535" }]);
    const port = req.port || 22;
    let w: Worker | undefined;
    if (req.workerId) {
      w = d.workers.find((x) => x.id === req.workerId);
      if (!w) throw d.invalid([{ field: "worker_id", message: "no such worker" }]);
      if (w.status !== WorkerStatus.ONLINE)
        throw new ConnectError(`worker ${w.name} is offline`, Code.Unavailable);
    } else {
      // Like the controller: a usable worker, by name; the demo's NAS first.
      const usable = d.workers
        .filter((x) => x.status === WorkerStatus.ONLINE && x.info?.features.includes("probe"))
        .sort((a, b) => a.name.localeCompare(b.name));
      w = usable.find((x) => x.id === fx.ids.nas) ?? usable[0];
      if (!w)
        throw new ConnectError("no online worker can do this right now", Code.FailedPrecondition);
    }
    if (!w.info?.probeOps.includes("sftp_host_keys"))
      throw new ConnectError(
        `worker ${w.name} is outdated: upgrade it to fetch host keys`,
        Code.FailedPrecondition,
      );
    await delay();
    if (host.endsWith(".invalid"))
      throw new ConnectError(
        `dial tcp: lookup ${host}: no such host (through ${w.name})`,
        Code.FailedPrecondition,
      );
    if (host === "web.example.net")
      throw new ConnectError(
        `${host}:${port} does not speak SSH: ssh: handshake failed: EOF`,
        Code.FailedPrecondition,
      );
    return { keys: hostKeys, worker: { id: w.id, name: w.name } };
  });
  router.rpc(WorkerAdminService.method.getWorkerUpgradeCommands, (req) => {
    if (d.me().role !== UserRole.ADMIN)
      throw new ConnectError("administrators only", Code.PermissionDenied);
    const w = d.workers.find((x) => x.id === req.workerId);
    if (!w) throw new ConnectError("worker not found", Code.NotFound);
    return { commands: upgradeCommands(w) };
  });
}
