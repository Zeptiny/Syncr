import { useNavigate } from "@tanstack/react-router";
import { createColumnHelper } from "@tanstack/react-table";
import { ArrowRight } from "lucide-react";
import { useMemo } from "react";

import { DataTable, type Pager } from "@/components/app/data-table";
import { RelativeTime } from "@/components/app/relative-time";
import { StatusBadge } from "@/components/app/status-badge";
import { JobStatus, type Job, type Worker } from "@/gen/syncr/v1/api_pb";
import type { User } from "@/gen/syncr/v1/users_pb";
import { useNow } from "@/hooks/use-now";
import { formatBytes, formatDuration, percent, tsDate } from "@/lib/format";
import { transferModeLabel } from "@/lib/status";

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
  empty?: React.ReactNode;
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
  empty,
}: JobsTableProps) {
  const navigate = useNavigate();
  const now = useNow(5000).getTime();
  const columns = useMemo(
    () => [
      col.accessor("status", {
        header: "Status",
        cell: (c) => (
          <StatusBadge job={c.getValue()} cancelRequested={c.row.original.cancelRequested} />
        ),
        meta: { className: "w-32" },
      }),
      col.display({
        id: "transfer",
        header: "Transfer",
        cell: ({ row: { original: job } }) => {
          const t = transferOf(job);
          if (!t) return <span>{job.kind}</span>;
          return (
            <div className="flex min-w-0 flex-col gap-0.5">
              <div className="flex min-w-0 items-center gap-2">
                <Endpoint e={endpoint(job, t.source)} className="max-w-[45%]" />
                <ArrowRight className="size-3.5 shrink-0 text-muted-foreground" aria-label="to" />
                <Endpoint e={endpoint(job, t.destination)} className="max-w-[45%]" />
              </div>
              <span className="text-xs text-muted-foreground">{transferModeLabel(t.mode)}</span>
            </div>
          );
        },
        meta: { className: "min-w-[18rem] max-w-[30rem]" },
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
        cell: ({ row }) => {
          const s = jobElapsed(row.original, now);
          return (
            <span className="tabular text-[13px] text-muted-foreground">
              {s === undefined ? "–" : formatDuration(s)}
            </span>
          );
        },
        meta: { className: "text-right" },
      }),
    ],
    [workers, owners, hideWorker, now],
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
