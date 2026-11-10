import { Link, useNavigate } from "@tanstack/react-router";
import { createColumnHelper } from "@tanstack/react-table";
import { ArrowRight, CalendarClock, Hourglass } from "lucide-react";
import { useMemo } from "react";

import { DataTable, type Pager } from "@/components/app/data-table";
import { RelativeTime } from "@/components/app/relative-time";
import { StatusBadge } from "@/components/app/status-badge";
import { JobStatus, type Job, type Worker } from "@/gen/syncr/v1/api_pb";
import type { User } from "@/gen/syncr/v1/users_pb";
import { useNow } from "@/hooks/use-now";
import { formatBytes, formatDuration, percent, tsDate } from "@/lib/format";
import { isTerminal, kindLabel, transferModeLabel } from "@/lib/status";

import { Endpoint } from "./endpoint";
import { endpoint, jobElapsed, jobProgress, jobTitle, transferOf } from "./model";

const col = createColumnHelper<Job>();

/** JobsTableProps configures JobsTable. */
export interface JobsTableProps {
  jobs: Job[] | undefined;
  isLoading?: boolean;
  error?: unknown;
  onRetry?: () => void;
  pager?: Pager;
  workers: Map<string, Worker>;
  /** owners, when given, adds an owner column (admin "all users" view). */
  owners?: Map<string, User>;
  hideWorker?: boolean;
  /** hideSchedule drops the schedule column (e.g. on a schedule's page). */
  hideSchedule?: boolean;
  empty?: React.ReactNode;
}

/** ScheduleChip names the schedule that launched a job, linking to it. */
export function ScheduleChip({ job }: { job: Job }) {
  if (!job.scheduleId) return <span className="text-[13px] text-muted-foreground">By hand</span>;
  const name = job.schedule?.name || "Deleted schedule";
  return (
    <Link
      to="/schedules/$scheduleId"
      params={{ scheduleId: job.scheduleId }}
      onClick={(e) => e.stopPropagation()}
      className="inline-flex max-w-36 items-center gap-1.5 rounded-md bg-secondary px-1.5 py-0.5 text-[12.5px] text-secondary-foreground hover:bg-accent"
      title={name}
    >
      <CalendarClock className="size-3.5 shrink-0 text-primary" aria-hidden />
      <span className="truncate">{name}</span>
    </Link>
  );
}

/**
 * WaitingChip marks a queued job that waits for an older job on the same
 * repository, and links to that job.
 */
export function WaitingChip({ job }: { job: Job }) {
  if (job.status !== JobStatus.QUEUED || !job.waitingForJobId) return null;
  return (
    <Link
      to="/jobs/$jobId"
      params={{ jobId: job.waitingForJobId }}
      onClick={(e) => e.stopPropagation()}
      className="inline-flex w-fit items-center gap-1 rounded-md bg-warning-soft px-1.5 py-0.5 text-[12px] font-medium whitespace-nowrap text-warning hover:underline"
      title="An older job on the same repository runs first. Open it."
    >
      <Hourglass className="size-3 shrink-0" aria-hidden />
      Waiting for repository
    </Link>
  );
}

function Transferred({ job }: { job: Job }) {
  const p = jobProgress(job);
  const pct = percent(p.bytes, p.total);
  const active = job.status === JobStatus.RUNNING;
  if (!job.stats) return <span className="text-muted-foreground">–</span>;
  return (
    <div className="flex min-w-28 flex-col gap-1">
      <span className="tabular text-[13px]">
        {formatBytes(p.bytes)}
        {p.total > 0 && p.total !== p.bytes && (
          <span className="text-muted-foreground"> of {formatBytes(p.total)}</span>
        )}
      </span>
      {active && pct !== undefined && (
        <span
          className="h-1 w-full overflow-hidden rounded-full bg-live-soft"
          aria-label={`${Math.round(pct)}%`}
        >
          <span className="block h-full rounded-full bg-live" style={{ width: `${pct}%` }} />
        </span>
      )}
    </div>
  );
}

/**
 * Duration shows how long a job ran, counting up while it runs. The clock
 * lives here and not in the table: column definitions that changed with
 * every tick would make the table mount every cell anew (losing focus and
 * selection inside the rows) every few seconds.
 */
function Duration({ job }: { job: Job }) {
  const running = !!job.startedAt && !job.finishedAt && !isTerminal(job.status);
  return running ? <RunningDuration job={job} /> : <DurationText seconds={jobElapsed(job)} />;
}

function RunningDuration({ job }: { job: Job }) {
  const now = useNow(5000).getTime();
  return <DurationText seconds={jobElapsed(job, now)} />;
}

function DurationText({ seconds }: { seconds: number | undefined }) {
  return (
    <span className="tabular text-[13px] whitespace-nowrap text-muted-foreground">
      {seconds === undefined ? "–" : formatDuration(seconds)}
    </span>
  );
}

/** JobsTable lists jobs; rows open the job. */
export function JobsTable({
  jobs,
  isLoading,
  error,
  onRetry,
  pager,
  workers,
  owners,
  hideWorker,
  hideSchedule,
  empty,
}: JobsTableProps) {
  const navigate = useNavigate();
  const columns = useMemo(
    () => [
      col.accessor("status", {
        header: "Status",
        cell: (c) => (
          <div className="flex flex-col items-start gap-1">
            <StatusBadge job={c.getValue()} cancelRequested={c.row.original.cancelRequested} />
            <WaitingChip job={c.row.original} />
          </div>
        ),
        meta: { className: "w-32" },
      }),
      col.display({
        id: "transfer",
        header: "Job",
        cell: ({ row: { original: job } }) => {
          const t = transferOf(job);
          if (!t)
            return (
              <div className="flex w-0 min-w-full flex-col gap-0.5">
                <span className="truncate font-medium" title={jobTitle(job)}>
                  {jobTitle(job)}
                </span>
                <span className="text-xs text-muted-foreground">
                  {kindLabel(job.kind)}
                  {job.repository?.deleted ? ", repository removed" : ""}
                </span>
              </div>
            );
          return (
            <div className="flex w-0 min-w-full flex-col gap-0.5">
              <div className="flex min-w-0 items-center gap-2">
                <Endpoint e={endpoint(job, t.source)} className="max-w-[45%]" />
                <ArrowRight className="size-3.5 shrink-0 text-muted-foreground" aria-label="to" />
                <Endpoint e={endpoint(job, t.destination)} className="max-w-[45%]" />
              </div>
              <span className="text-xs text-muted-foreground">{transferModeLabel(t.mode)}</span>
            </div>
          );
        },
        // The column takes the width the others leave. Its content never
        // widens it (w-0 min-w-full above): long names are cut instead of
        // pushing the table past its container.
        meta: { className: "w-full min-w-[18rem]" },
      }),
      ...(owners
        ? [
            col.accessor("ownerId", {
              header: "Owner",
              cell: (c) => {
                const u = owners.get(c.getValue());
                return <span className="text-[13px]">{u ? u.displayName || u.username : "–"}</span>;
              },
            }),
          ]
        : []),
      ...(hideWorker
        ? []
        : [
            col.accessor("workerId", {
              header: "Worker",
              cell: (c) => (
                <span className="text-[13px]">
                  {workers.get(c.getValue())?.name ?? "Removed worker"}
                </span>
              ),
            }),
          ]),
      ...(hideSchedule
        ? []
        : [
            col.display({
              id: "schedule",
              header: "Schedule",
              cell: ({ row }) => <ScheduleChip job={row.original} />,
            }),
          ]),
      col.display({
        id: "bytes",
        header: "Transferred",
        cell: ({ row }) => <Transferred job={row.original} />,
      }),
      col.accessor((j) => tsDate(j.createdAt), {
        id: "created",
        header: "Created",
        cell: (c) => <RelativeTime date={c.getValue()} className="text-[13px] whitespace-nowrap" />,
      }),
      col.display({
        id: "duration",
        header: "Duration",
        cell: ({ row }) => <Duration job={row.original} />,
        meta: { className: "text-right" },
      }),
    ],
    [workers, owners, hideWorker, hideSchedule],
  );
  return (
    <DataTable
      aria-label="Jobs"
      columns={columns}
      data={jobs}
      isLoading={isLoading}
      error={error}
      onRetry={onRetry}
      pager={pager}
      empty={empty}
      getRowId={(j) => j.id}
      rowLabel={(j) => jobTitle(j)}
      onRowClick={(j) => void navigate({ to: "/jobs/$jobId", params: { jobId: j.id } })}
    />
  );
}
