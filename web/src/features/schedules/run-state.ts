import { JobStatus } from "@/gen/syncr/v1/api_pb";
import { ScheduleRunStatus, type ScheduleRun } from "@/gen/syncr/v1/schedules_pb";
import type { StatusMeta } from "@/lib/status";

/**
 * RunState is how a schedule's newest run went, for the dashboard's health
 * list and the schedule list.
 */
export interface RunState {
  meta: StatusMeta;
  /** problem marks a run that did not succeed and is not going on. */
  problem: boolean;
}

/**
 * runState says how run went: its job's outcome once it launched, else the
 * run's own status (missed, could not start, cancelled, still waiting).
 */
export function runState(run: ScheduleRun | undefined): RunState | undefined {
  if (!run) return undefined;
  switch (run.status) {
    case ScheduleRunStatus.PENDING:
    case ScheduleRunStatus.LAUNCHING:
      return { meta: { label: "Starting", tone: "live", pulse: true }, problem: false };
    case ScheduleRunStatus.MISSED:
      return { meta: { label: "Missed", tone: "warning" }, problem: true };
    case ScheduleRunStatus.FAILED:
      return { meta: { label: "Didn't start", tone: "danger" }, problem: true };
    case ScheduleRunStatus.CANCELLED:
      return { meta: { label: "Cancelled", tone: "neutral" }, problem: true };
    case ScheduleRunStatus.LAUNCHED:
      switch (run.jobStatus) {
        case JobStatus.QUEUED:
        case JobStatus.DISPATCHED:
        case JobStatus.RUNNING:
          return { meta: { label: "Running", tone: "live", pulse: true }, problem: false };
        case JobStatus.SUCCEEDED:
          return { meta: { label: "Succeeded", tone: "success" }, problem: false };
        case JobStatus.FAILED:
          return { meta: { label: "Failed", tone: "danger" }, problem: true };
        case JobStatus.INTERRUPTED:
          return { meta: { label: "Interrupted", tone: "warning" }, problem: true };
        case JobStatus.CANCELLED:
          return { meta: { label: "Cancelled", tone: "neutral" }, problem: true };
      }
  }
  return undefined;
}
