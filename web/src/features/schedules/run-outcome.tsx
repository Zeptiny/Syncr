import { ScheduleRunStatusBadge, StatusBadge } from "@/components/app/status-badge";
import { JobStatus } from "@/gen/syncr/v1/api_pb";
import { ScheduleRunStatus, type ScheduleRun } from "@/gen/syncr/v1/schedules_pb";

/**
 * RunOutcome shows how a run ended: its job's status once it launched a
 * job, otherwise the run's own status (waiting, missed, failed).
 */
export function RunOutcome({ run, className }: { run: ScheduleRun; className?: string }) {
  if (run.status === ScheduleRunStatus.LAUNCHED && run.jobStatus !== JobStatus.UNSPECIFIED) {
    return <StatusBadge job={run.jobStatus} className={className} />;
  }
  return <ScheduleRunStatusBadge status={run.status} className={className} />;
}
