import type { Job, JobLocation, RcloneTransferDefinition } from "@/gen/syncr/v1/api_pb";
import { durationSeconds, toNumber, tsDate } from "@/lib/format";
import { isTerminal } from "@/lib/status";

/** transferOf returns the job's transfer definition, if it is one. */
export function transferOf(job: Job): RcloneTransferDefinition | undefined {
  const op = job.definition?.operation;
  return op?.case === "rcloneTransfer" ? op.value : undefined;
}

/** EndpointLabel is a job location ready for display. */
export interface EndpointLabel {
  /** name is the remote's name; undefined for the worker filesystem. */
  name: string | undefined;
  backendType: string | undefined;
  path: string;
  deleted: boolean;
  local: boolean;
}

/** endpoint labels a job location using the job's remote refs. */
export function endpoint(job: Job, loc: JobLocation | undefined): EndpointLabel {
  if (!loc || !loc.remoteId) {
    return {
      name: undefined,
      backendType: undefined,
      path: loc?.path ?? "",
      deleted: false,
      local: true,
    };
  }
  const ref = job.remotes.find((r) => r.id === loc.remoteId);
  return {
    name: ref?.name ?? "Unknown remote",
    backendType: ref?.backendType,
    path: loc.path,
    deleted: ref?.deleted ?? false,
    local: false,
  };
}

/** jobTitle is a one-line description: "Photos → B2 offsite". */
export function jobTitle(job: Job): string {
  const t = transferOf(job);
  if (!t) return job.kind;
  const name = (e: EndpointLabel) => e.name ?? (e.path || "Worker filesystem");
  return `${name(endpoint(job, t.source))} → ${name(endpoint(job, t.destination))}`;
}

/** jobElapsed is how long the job has run (or ran), in seconds. */
export function jobElapsed(job: Job, now = Date.now()): number | undefined {
  const started = tsDate(job.startedAt);
  if (!started) return undefined;
  const finished = tsDate(job.finishedAt);
  if (finished) return (finished.getTime() - started.getTime()) / 1000;
  if (isTerminal(job.status)) return durationSeconds(job.stats?.elapsed) || undefined;
  return Math.max(0, (now - started.getTime()) / 1000);
}

/** jobProgress returns bytes done and total (0 if unknown). */
export function jobProgress(job: Job): {
  bytes: number;
  total: number;
  files: number;
  totalFiles: number;
} {
  return {
    bytes: toNumber(job.stats?.bytes),
    total: toNumber(job.stats?.totalBytes),
    files: toNumber(job.stats?.files),
    totalFiles: toNumber(job.stats?.totalFiles),
  };
}
