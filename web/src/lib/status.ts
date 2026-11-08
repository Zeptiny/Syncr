import { JobStatus, WorkerStatus } from "@/gen/syncr/v1/api_pb";
import { JobErrorCode, RcloneTransferMode, WorkerMode } from "@/gen/syncr/v1/link_pb";

/** Tone is the colour family of a status. */
export type Tone = "live" | "success" | "danger" | "warning" | "neutral";

/** StatusMeta describes how a status is shown. */
export interface StatusMeta {
  label: string;
  tone: Tone;
  /** pulse marks a status where something is happening right now. */
  pulse?: boolean;
}

const jobStatus: Record<JobStatus, StatusMeta> = {
  [JobStatus.UNSPECIFIED]: { label: "Unknown", tone: "neutral" },
  [JobStatus.QUEUED]: { label: "Queued", tone: "neutral" },
  [JobStatus.DISPATCHED]: { label: "Starting", tone: "live" },
  [JobStatus.RUNNING]: { label: "Running", tone: "live", pulse: true },
  [JobStatus.SUCCEEDED]: { label: "Succeeded", tone: "success" },
  [JobStatus.FAILED]: { label: "Failed", tone: "danger" },
  [JobStatus.CANCELLED]: { label: "Cancelled", tone: "neutral" },
  [JobStatus.INTERRUPTED]: { label: "Interrupted", tone: "warning" },
};

/** jobStatusMeta returns the label and tone of a job status. */
export function jobStatusMeta(status: JobStatus, cancelRequested = false): StatusMeta {
  if (cancelRequested && !isTerminal(status)) return { label: "Cancelling", tone: "warning" };
  return jobStatus[status] ?? jobStatus[JobStatus.UNSPECIFIED];
}

/** The statuses a job filter offers, in lifecycle order. */
export const jobStatuses: JobStatus[] = [
  JobStatus.QUEUED,
  JobStatus.DISPATCHED,
  JobStatus.RUNNING,
  JobStatus.SUCCEEDED,
  JobStatus.FAILED,
  JobStatus.CANCELLED,
  JobStatus.INTERRUPTED,
];

/** isTerminal reports whether a job status is final. */
export function isTerminal(status: JobStatus): boolean {
  return (
    status === JobStatus.SUCCEEDED ||
    status === JobStatus.FAILED ||
    status === JobStatus.CANCELLED ||
    status === JobStatus.INTERRUPTED
  );
}

const workerStatus: Record<WorkerStatus, StatusMeta> = {
  [WorkerStatus.UNSPECIFIED]: { label: "Unknown", tone: "neutral" },
  [WorkerStatus.PENDING]: { label: "Pending", tone: "warning" },
  [WorkerStatus.ONLINE]: { label: "Online", tone: "success" },
  [WorkerStatus.OFFLINE]: { label: "Offline", tone: "neutral" },
};

/** workerStatusMeta returns the label and tone of a worker status. */
export function workerStatusMeta(status: WorkerStatus): StatusMeta {
  return workerStatus[status] ?? workerStatus[WorkerStatus.UNSPECIFIED];
}

/** workerModeLabel names a worker's link mode. */
export function workerModeLabel(mode: WorkerMode): string {
  switch (mode) {
    case WorkerMode.ACTIVE:
      return "Active";
    case WorkerMode.PASSIVE:
      return "Passive";
    default:
      return "Unknown";
  }
}

/** transferModes lists the rclone transfer modes with their meaning. */
export const transferModes: { mode: RcloneTransferMode; label: string; help: string }[] = [
  {
    mode: RcloneTransferMode.COPY,
    label: "Copy",
    help: "Copy new and changed files. Nothing is deleted.",
  },
  {
    mode: RcloneTransferMode.SYNC,
    label: "Sync",
    help: "Make the destination identical to the source, deleting extra files.",
  },
  {
    mode: RcloneTransferMode.MOVE,
    label: "Move",
    help: "Copy, then delete the transferred files from the source.",
  },
];

/** transferModeLabel names a transfer mode. */
export function transferModeLabel(mode: RcloneTransferMode): string {
  return transferModes.find((m) => m.mode === mode)?.label ?? "Transfer";
}

/** kindLabel names a job kind. */
export function kindLabel(kind: string): string {
  switch (kind) {
    case "rclone_transfer":
      return "Transfer";
    default:
      return kind.replaceAll("_", " ");
  }
}

const errorCodes: Partial<Record<JobErrorCode, string>> = {
  [JobErrorCode.INVALID_SPEC]: "Invalid job",
  [JobErrorCode.UNSUPPORTED_KIND]: "Unsupported job kind",
  [JobErrorCode.PATH_NOT_ALLOWED]: "Path not allowed",
  [JobErrorCode.TOOL_UNAVAILABLE]: "Tool unavailable",
  [JobErrorCode.EXECUTION_FAILED]: "Execution failed",
  [JobErrorCode.INTERNAL]: "Worker error",
  [JobErrorCode.NOT_FOUND]: "Not found",
  [JobErrorCode.TEMPORARY]: "Temporary failure",
  [JobErrorCode.LIMIT_REACHED]: "Limit reached",
  [JobErrorCode.REPOSITORY_NOT_FOUND]: "Repository not found",
  [JobErrorCode.REPOSITORY_LOCKED]: "Repository locked",
  [JobErrorCode.WRONG_PASSWORD]: "Wrong password",
  [JobErrorCode.CANCELLED]: "Cancelled",
  [JobErrorCode.WORKER_SHUTDOWN]: "Worker shut down",
  [JobErrorCode.DISPATCH_TIMEOUT]: "Worker didn't pick it up",
  [JobErrorCode.WORKER_RESTARTED]: "Worker restarted",
  [JobErrorCode.WORKER_LOST]: "Worker lost the job",
  [JobErrorCode.WORKER_REMOVED]: "Worker removed",
  [JobErrorCode.REMOTE_UNAVAILABLE]: "Remote unavailable",
};

/** errorCodeLabel names a job error code. */
export function errorCodeLabel(code: JobErrorCode): string {
  return errorCodes[code] ?? "Error";
}
