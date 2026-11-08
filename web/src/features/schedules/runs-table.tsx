import { Link } from "@tanstack/react-router";
import { createColumnHelper } from "@tanstack/react-table";
import { Hand } from "lucide-react";
import { useMemo } from "react";

import { DataTable, type Pager } from "@/components/app/data-table";
import { ScheduleRunStatusBadge, StatusBadge } from "@/components/app/status-badge";
import { JobStatus } from "@/gen/syncr/v1/api_pb";
import {
  ScheduleRunStatus,
  ScheduleRunTrigger,
  type ScheduleRun,
} from "@/gen/syncr/v1/schedules_pb";
import { formatDateTime, shortId, tsDate } from "@/lib/format";
import { runReasonLabel } from "@/lib/status";
import { formatInZone } from "@/lib/timezones";

const col = createColumnHelper<ScheduleRun>();

/** runDetail explains a run in a few words: why it waits, was missed or failed. */
export function runDetail(r: ScheduleRun): string {
  const parts: string[] = [];
  if (r.status === ScheduleRunStatus.PENDING) {
    parts.push(
      r.reason ? `Waiting: ${runReasonLabel(r.reason).toLowerCase()}` : "Waiting to start",
    );
  } else if (r.reason) {
    parts.push(runReasonLabel(r.reason));
  }
  if (r.coalesced > 0)
    parts.push(`${r.coalesced} earlier ${r.coalesced === 1 ? "run" : "runs"} merged into this one`);
  return parts.join(". ");
}

/** ScheduleRunsTable lists a schedule's runs, newest first. */
export function ScheduleRunsTable({
  runs,
  timezone,
  isLoading,
  error,
  onRetry,
  pager,
}: {
  runs: ScheduleRun[] | undefined;
  timezone: string;
  isLoading?: boolean;
  error?: unknown;
  onRetry?: () => void;
  pager?: Pager;
}) {
  const columns = useMemo(
    () => [
      col.accessor((r) => tsDate(r.scheduledFor), {
        id: "due",
        header: "Due",
        cell: (c) => {
          const at = c.getValue();
          return at ? (
            <time
              dateTime={at.toISOString()}
              title={`Your time: ${formatDateTime(at)}`}
              className="tabular text-[13px] whitespace-nowrap"
            >
              {formatInZone(at, timezone)}
            </time>
          ) : (
            "–"
          );
        },
      }),
      col.accessor("trigger", {
        header: "Started by",
        cell: (c) =>
          c.getValue() === ScheduleRunTrigger.MANUAL ? (
            <span className="inline-flex items-center gap-1 text-[13px]">
              <Hand className="size-3.5 text-muted-foreground" aria-hidden />
              Run now
            </span>
          ) : (
            <span className="text-[13px] text-muted-foreground">Schedule</span>
          ),
      }),
      col.accessor("status", {
        header: "Run",
        cell: (c) => <ScheduleRunStatusBadge status={c.getValue()} />,
      }),
      col.display({
        id: "detail",
        header: "Details",
        cell: ({ row: { original: r } }) => {
          const text = runDetail(r);
          return (
            <span className="block max-w-[22rem] truncate text-[13px]" title={r.message || text}>
              {text || <span className="text-muted-foreground">–</span>}
              {r.message && text && <span className="text-muted-foreground">. {r.message}</span>}
            </span>
          );
        },
        meta: { className: "max-w-[24rem]" },
      }),
      col.display({
        id: "job",
        header: "Job",
        cell: ({ row: { original: r } }) =>
          r.jobId ? (
            <Link
              to="/jobs/$jobId"
              params={{ jobId: r.jobId }}
              className="inline-flex items-center gap-2 hover:underline"
            >
              {r.jobStatus !== JobStatus.UNSPECIFIED && <StatusBadge job={r.jobStatus} />}
              <span className="font-mono text-xs text-muted-foreground">{shortId(r.jobId)}</span>
            </Link>
          ) : (
            <span className="text-[13px] text-muted-foreground">No job</span>
          ),
        meta: { className: "text-right" },
      }),
    ],
    [timezone],
  );
  return (
    <DataTable
      aria-label="Runs"
      columns={columns}
      data={runs}
      isLoading={isLoading}
      error={error}
      onRetry={onRetry}
      pager={pager}
      getRowId={(r) => r.id}
      empty={
        <p className="p-8 text-center text-[13px] text-muted-foreground">
          No runs yet. The first one is due at the next run time.
        </p>
      }
    />
  );
}
