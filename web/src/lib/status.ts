import { JobStatus, WorkerStatus } from "@/gen/syncr/v1/api_pb";
import { JobErrorCode, RcloneTransferMode, WorkerMode } from "@/gen/syncr/v1/link_pb";
import { DeliveryStatus, NotificationEvent } from "@/gen/syncr/v1/notify_pb";
import { ScheduleRunStatus } from "@/gen/syncr/v1/schedules_pb";

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

/**
 * jobKinds are the kinds the jobs list can filter by. Restores and data
 * deletions are listed here, though no form can start them.
 */
export const jobKinds: string[] = [
  "rclone_transfer",
  "restic_backup",
  "restic_forget",
  "restic_check",
  "restic_prune",
  "restic_restore",
  "restic_delete_data",
];

/** isResticKind reports whether a job kind works on a restic repository. */
export function isResticKind(kind: string): boolean {
  return kind.startsWith("restic_");
}

/** kindLabel names a job kind. */
export function kindLabel(kind: string): string {
  switch (kind) {
    case "rclone_transfer":
      return "Transfer";
    case "restic_backup":
      return "Backup";
    case "restic_forget":
      return "Retention";
    case "restic_prune":
      return "Prune";
    case "restic_check":
      return "Check";
    case "restic_restore":
      return "Restore";
    case "restic_delete_data":
      return "Delete data";
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
  [JobErrorCode.ALREADY_EXISTS]: "Already exists",
  [JobErrorCode.BUSY]: "Worker busy",
  [JobErrorCode.TIMEOUT]: "Timed out",
  [JobErrorCode.REPOSITORY_MISMATCH]: "Another repository is there",
  [JobErrorCode.REPOSITORY_UNAVAILABLE]: "Repository unavailable",
  [JobErrorCode.WORKER_OUTDATED]: "Worker outdated",
  [JobErrorCode.TIME_LIMIT]: "Time limit reached",
  [JobErrorCode.HOOK_FAILED]: "A hook failed",
};

/** errorCodeLabel names a job error code. */
export function errorCodeLabel(code: JobErrorCode): string {
  return errorCodes[code] ?? "Error";
}

const runStatus: Record<ScheduleRunStatus, StatusMeta> = {
  [ScheduleRunStatus.UNSPECIFIED]: { label: "Unknown", tone: "neutral" },
  [ScheduleRunStatus.PENDING]: { label: "Waiting", tone: "warning" },
  [ScheduleRunStatus.LAUNCHING]: { label: "Starting", tone: "live", pulse: true },
  [ScheduleRunStatus.LAUNCHED]: { label: "Launched", tone: "success" },
  [ScheduleRunStatus.MISSED]: { label: "Missed", tone: "warning" },
  [ScheduleRunStatus.FAILED]: { label: "Failed", tone: "danger" },
  [ScheduleRunStatus.CANCELLED]: { label: "Cancelled", tone: "neutral" },
};

/** scheduleRunStatusMeta returns the label and tone of a schedule run status. */
export function scheduleRunStatusMeta(status: ScheduleRunStatus): StatusMeta {
  return runStatus[status] ?? runStatus[ScheduleRunStatus.UNSPECIFIED];
}

const deliveryStatus: Record<DeliveryStatus, StatusMeta> = {
  [DeliveryStatus.UNSPECIFIED]: { label: "Unknown", tone: "neutral" },
  [DeliveryStatus.PENDING]: { label: "Pending", tone: "neutral" },
  [DeliveryStatus.SENDING]: { label: "Sending", tone: "live", pulse: true },
  [DeliveryStatus.DELIVERED]: { label: "Delivered", tone: "success" },
  [DeliveryStatus.DEAD]: { label: "Gave up", tone: "danger" },
};

/**
 * deliveryStatusMeta returns the label and tone of a delivery. A pending
 * delivery that already failed an attempt is shown as retrying.
 */
export function deliveryStatusMeta(status: DeliveryStatus, attempts = 0): StatusMeta {
  if (status === DeliveryStatus.PENDING && attempts > 0)
    return { label: "Retrying", tone: "warning" };
  return deliveryStatus[status] ?? deliveryStatus[DeliveryStatus.UNSPECIFIED];
}

/** The events a schedule link can notify, in the order the UI shows them. */
export const scheduleEvents: NotificationEvent[] = [
  NotificationEvent.SUCCEEDED,
  NotificationEvent.FAILED,
  NotificationEvent.INTERRUPTED,
  NotificationEvent.MISSED,
];

/** The events a contact can get for jobs run by hand (never "missed"). */
export const manualJobEvents: NotificationEvent[] = [
  NotificationEvent.SUCCEEDED,
  NotificationEvent.FAILED,
  NotificationEvent.INTERRUPTED,
];

/** The events a new schedule link starts with. */
export const defaultLinkEvents: NotificationEvent[] = [
  NotificationEvent.FAILED,
  NotificationEvent.INTERRUPTED,
  NotificationEvent.MISSED,
];

const events: Record<NotificationEvent, { label: string; tone: Tone; help: string }> = {
  [NotificationEvent.UNSPECIFIED]: { label: "Unknown", tone: "neutral", help: "" },
  [NotificationEvent.SUCCEEDED]: {
    label: "Succeeded",
    tone: "success",
    help: "The job finished without errors.",
  },
  [NotificationEvent.FAILED]: {
    label: "Failed",
    tone: "danger",
    help: "The job failed, or the run could not start.",
  },
  [NotificationEvent.INTERRUPTED]: {
    label: "Interrupted",
    tone: "warning",
    help: "The worker stopped or lost the job.",
  },
  [NotificationEvent.MISSED]: {
    label: "Missed",
    tone: "warning",
    help: "The run never started: its worker stayed offline, or the last run was still going.",
  },
  [NotificationEvent.TEST]: { label: "Test", tone: "neutral", help: "A test notification." },
};

/** eventMeta describes a notification event. */
export function eventMeta(e: NotificationEvent): { label: string; tone: Tone; help: string } {
  return events[e] ?? events[NotificationEvent.UNSPECIFIED];
}

const runReasons: Record<string, string> = {
  worker_offline: "The worker was offline",
  overlap: "The previous run was still going",
  controller_unavailable: "The controller was down",
  remote_unavailable: "A remote it uses is gone",
  invalid_definition: "The job is no longer valid",
  worker_removed: "Its worker was removed",
  worker_outdated: "Its worker cannot run this job",
  no_worker_with_label: "No worker has the label",
  owner_disabled: "The owner's account is disabled",
  disabled: "The schedule was paused",
  repository_unavailable: "Its repository is gone",
  repository_deleting: "Its repository's data is being deleted",
};

/** runReasonLabel explains a schedule run's reason code. */
export function runReasonLabel(reason: string): string {
  if (!reason) return "";
  return runReasons[reason] ?? reason.replaceAll("_", " ");
}

/** pausedReasonLabel explains why a schedule was paused automatically. */
export function pausedReasonLabel(reason: string): string {
  switch (reason) {
    case "worker_removed":
      return "Its worker was removed. Choose another worker or a label, then resume it.";
    case "worker_outdated":
      return "Its worker cannot run this job: it needs an upgrade. Upgrade the worker or choose another, then resume it.";
    case "remote_unavailable":
      return "A remote it uses was deleted. Edit the job, then resume it.";
    case "invalid_definition":
      return "Its job is no longer valid. Edit it, then resume it.";
    case "owner_disabled":
      return "Its owner's account was disabled.";
    case "repository_deleting":
      return "The data of its repository is being deleted, or a deletion was started. Resume it only if the repository is kept.";
    case "repository_unavailable":
      return "Its repository was removed. Choose another repository, then resume it.";
    default:
      return reason ? runReasonLabel(reason) + "." : "";
  }
}
