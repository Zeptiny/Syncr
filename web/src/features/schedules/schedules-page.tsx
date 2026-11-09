import { useQuery } from "@connectrpc/connect-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { createColumnHelper } from "@tanstack/react-table";
import { CalendarClock, MoreHorizontal, Pause, Pencil, Play, Plus, Tag } from "lucide-react";
import { useMemo } from "react";

import { DataTable } from "@/components/app/data-table";
import { EmptyState } from "@/components/app/empty-state";
import { PageHeader } from "@/components/app/page-header";
import { RelativeTime } from "@/components/app/relative-time";
import { toneDot } from "@/lib/tones";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Switch } from "@/components/ui/switch";
import { WorkerAdminService, type Worker } from "@/gen/syncr/v1/api_pb";
import { ScheduleService, type Schedule } from "@/gen/syncr/v1/schedules_pb";
import { cronText } from "@/lib/cron";
import { tsDate } from "@/lib/format";
import { workerStatusMeta } from "@/lib/status";
import { formatInZone } from "@/lib/timezones";
import { cn } from "@/lib/utils";

import { useScheduleActions } from "./actions";
import { targetLabel } from "./model";
import { RunOutcome } from "./run-outcome";

const col = createColumnHelper<Schedule>();

/** TargetChip shows where a schedule runs: a worker with its status, or a label. */
export function TargetChip({
  schedule,
  workers,
}: {
  schedule: Schedule;
  workers: Map<string, Worker>;
}) {
  const t = targetLabel(schedule.target, workers);
  if (t.kind === "label") {
    return (
      <span className="inline-flex items-center gap-1 rounded bg-secondary px-1.5 py-0.5 font-mono text-[12px] text-secondary-foreground">
        <Tag className="size-3 opacity-60" aria-hidden />
        {t.text}
      </span>
    );
  }
  const meta = t.worker ? workerStatusMeta(t.worker.status) : undefined;
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 text-[13px]",
        !t.worker && "text-muted-foreground line-through",
      )}
    >
      {meta && (
        <span className={cn("size-1.5 rounded-full", toneDot[meta.tone])} aria-label={meta.label} />
      )}
      {t.text}
    </span>
  );
}

/** NextRun shows when a schedule runs next, or that it is paused. */
export function NextRun({ schedule }: { schedule: Schedule }) {
  if (!schedule.enabled) {
    return (
      <span className="text-[13px] text-muted-foreground">
        {schedule.pausedReason ? "Paused automatically" : "Paused"}
      </span>
    );
  }
  const at = tsDate(schedule.nextRunAt);
  if (!at) return <span className="text-[13px] text-muted-foreground">Never</span>;
  return (
    <div className="flex flex-col">
      <RelativeTime date={at} className="text-[13px] font-medium" />
      <span className="tabular text-xs whitespace-nowrap text-muted-foreground">
        {formatInZone(at, schedule.timezone, false)}
      </span>
    </div>
  );
}

/** SchedulesPage lists the user's schedules. */
export function SchedulesPage() {
  const navigate = useNavigate();
  const q = useQuery(ScheduleService.method.listSchedules, {}, { refetchInterval: 30_000 });
  const workers = useQuery(WorkerAdminService.method.listWorkers, {});
  const { runNow, setEnabled } = useScheduleActions();
  const workerMap = useMemo(
    () => new Map((workers.data?.workers ?? []).map((w) => [w.id, w])),
    [workers.data],
  );

  const columns = useMemo(
    () => [
      col.accessor("name", {
        header: "Schedule",
        cell: ({ row: { original: s } }) => (
          <div className="flex min-w-0 flex-col">
            <span className="truncate font-medium">{s.name}</span>
            <span className="truncate text-xs text-muted-foreground" title={s.cron}>
              {cronText(s.cron)}
            </span>
          </div>
        ),
        meta: { className: "min-w-[14rem] max-w-[22rem]" },
      }),
      col.display({
        id: "next",
        header: "Next run",
        cell: ({ row }) => <NextRun schedule={row.original} />,
      }),
      col.accessor("timezone", {
        header: "Time zone",
        cell: (c) => <span className="text-[13px] whitespace-nowrap">{c.getValue()}</span>,
      }),
      col.display({
        id: "target",
        header: "Runs on",
        cell: ({ row }) => <TargetChip schedule={row.original} workers={workerMap} />,
      }),
      col.display({
        id: "last",
        header: "Last run",
        cell: ({ row: { original: s } }) =>
          s.lastRun ? (
            <div className="flex items-center gap-2">
              <RunOutcome run={s.lastRun} />
              <RelativeTime
                date={tsDate(s.lastRun.scheduledFor)}
                className="text-xs whitespace-nowrap text-muted-foreground"
              />
            </div>
          ) : (
            <span className="text-[13px] text-muted-foreground">Not yet</span>
          ),
      }),
      col.display({
        id: "enabled",
        header: "On",
        cell: ({ row: { original: s } }) => (
          // The switch must not open the row.
          <span onClick={(e) => e.stopPropagation()} onKeyDown={(e) => e.stopPropagation()}>
            <Switch
              checked={s.enabled}
              onCheckedChange={(on) => void setEnabled(s, on)}
              aria-label={s.enabled ? `Pause ${s.name}` : `Resume ${s.name}`}
            />
          </span>
        ),
      }),
      col.display({
        id: "actions",
        header: () => <span className="sr-only">Actions</span>,
        cell: ({ row: { original: s } }) => (
          <span onClick={(e) => e.stopPropagation()} onKeyDown={(e) => e.stopPropagation()}>
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${s.name}`}>
                  <MoreHorizontal aria-hidden />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                <DropdownMenuItem onSelect={() => void runNow(s)}>
                  <Play aria-hidden />
                  Run now
                </DropdownMenuItem>
                <DropdownMenuItem onSelect={() => void setEnabled(s, !s.enabled)}>
                  {s.enabled ? <Pause aria-hidden /> : <Play aria-hidden />}
                  {s.enabled ? "Pause" : "Resume"}
                </DropdownMenuItem>
                <DropdownMenuSeparator />
                <DropdownMenuItem asChild>
                  <Link to="/schedules/$scheduleId/edit" params={{ scheduleId: s.id }}>
                    <Pencil aria-hidden />
                    Edit
                  </Link>
                </DropdownMenuItem>
              </DropdownMenuContent>
            </DropdownMenu>
          </span>
        ),
        meta: { className: "w-12 text-right" },
      }),
    ],
    [workerMap, runNow, setEnabled],
  );

  return (
    <>
      <PageHeader
        title="Schedules"
        description="Jobs that run by themselves, at the times you set, on a worker or on any worker with a label."
        actions={
          <Button asChild>
            <Link to="/schedules/new">
              <Plus aria-hidden />
              New schedule
            </Link>
          </Button>
        }
      />
      <DataTable
        aria-label="Schedules"
        columns={columns}
        data={q.data?.schedules}
        isLoading={q.isPending}
        error={q.error ?? undefined}
        onRetry={() => void q.refetch()}
        getRowId={(s) => s.id}
        rowLabel={(s) => s.name}
        onRowClick={(s) =>
          void navigate({ to: "/schedules/$scheduleId", params: { scheduleId: s.id } })
        }
        empty={
          <EmptyState
            icon={CalendarClock}
            title="No schedules yet"
            description="A schedule runs a job by itself: every night, every hour, or whenever you like. If its worker is asleep, it can run when it wakes up."
            action={
              <Button asChild>
                <Link to="/schedules/new">New schedule</Link>
              </Button>
            }
          />
        }
      />
    </>
  );
}
