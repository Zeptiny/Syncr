import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import {
  ArrowRight,
  CirclePause,
  Copy,
  ListFilter,
  Pause,
  Pencil,
  Play,
  Trash2,
} from "lucide-react";
import { useMemo, useState } from "react";
import { toast } from "sonner";

import { ChannelIcon } from "@/components/app/channel-icon";
import { ConfirmDialog } from "@/components/app/confirm-dialog";
import { ErrorState } from "@/components/app/error-state";
import { KeyValueList } from "@/components/app/key-value-list";
import { NextRunsList } from "@/components/app/next-runs-list";
import { PageHeader } from "@/components/app/page-header";
import { Panel } from "@/components/app/panel";
import { RelativeTime } from "@/components/app/relative-time";
import { StatusPill } from "@/components/app/status-badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { WorkerAdminService, type Job } from "@/gen/syncr/v1/api_pb";
import { DashboardService } from "@/gen/syncr/v1/dashboard_pb";
import { ScheduleService, type Schedule } from "@/gen/syncr/v1/schedules_pb";
import { useKeysetPager } from "@/hooks/use-keyset-pager";
import { cronText } from "@/lib/cron";
import { formatDateTime, tsDate } from "@/lib/format";
import { invalidateService } from "@/lib/invalidate";
import { eventMeta, pausedReasonLabel, transferModeLabel } from "@/lib/status";
import { hasOffsetChange, zoneLabel } from "@/lib/timezones";

import { Endpoint } from "../jobs/endpoint";
import { endpoint, transferOf } from "../jobs/model";
import { useScheduleActions } from "./actions";
import { missedSummary, overlapLabel } from "./model";
import { ScheduleRunsTable } from "./runs-table";
import { TargetChip } from "./schedules-page";

// A schedule stores a definition, not a job; Endpoint helpers take a job.
function asJob(s: Schedule): Job {
  return { definition: s.definition, remotes: s.remotes } as Job;
}

function JobSummary({ schedule }: { schedule: Schedule }) {
  const job = asJob(schedule);
  const t = transferOf(job);
  if (!t) return <span className="text-muted-foreground">No transfer</span>;
  return (
    <div className="flex min-w-0 flex-col gap-1">
      <div className="flex min-w-0 flex-wrap items-center gap-2">
        <Endpoint e={endpoint(job, t.source)} />
        <ArrowRight className="size-3.5 shrink-0 text-muted-foreground" aria-label="to" />
        <Endpoint e={endpoint(job, t.destination)} />
      </div>
      <span className="text-xs text-muted-foreground">
        {transferModeLabel(t.mode)}
        {t.options?.dryRun ? ", dry run" : ""}
        {t.options?.checksum ? ", compares checksums" : ""}
        {t.options?.filters.length
          ? `, ${t.options.filters.length} ${t.options.filters.length === 1 ? "filter" : "filters"}`
          : ""}
      </span>
    </div>
  );
}

/** ScheduleDetailPage shows a schedule, its next runs and its run history. */
export function ScheduleDetailPage({ scheduleId }: { scheduleId: string }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const q = useQuery(
    ScheduleService.method.getSchedule,
    { scheduleId },
    { refetchInterval: 15_000 },
  );
  const s = q.data?.schedule;
  const preview = useQuery(
    ScheduleService.method.previewSchedule,
    { cron: s?.cron ?? "", timezone: s?.timezone ?? "", count: 5 },
    { enabled: !!s, staleTime: 60_000 },
  );
  const pager = useKeysetPager(scheduleId);
  const runs = useQuery(
    ScheduleService.method.listScheduleRuns,
    { scheduleId, pageSize: 15, pageToken: pager.token },
    { placeholderData: (prev) => prev, refetchInterval: 10_000 },
  );
  const workers = useQuery(WorkerAdminService.method.listWorkers, {});
  const workerMap = useMemo(
    () => new Map((workers.data?.workers ?? []).map((w) => [w.id, w])),
    [workers.data],
  );
  const actions = useScheduleActions();
  const del = useMutation(ScheduleService.method.deleteSchedule);
  const [deleting, setDeleting] = useState(false);
  const next = useMemo(
    () => (preview.data?.nextRuns ?? []).map((t) => timestampDate(t)),
    [preview.data],
  );

  if (q.error) return <ErrorState error={q.error} onRetry={() => void q.refetch()} />;
  if (!s) {
    return (
      <div className="flex flex-col gap-4">
        <Skeleton className="h-8 w-72" />
        <Skeleton className="h-40 w-full" />
        <Skeleton className="h-72 w-full" />
      </div>
    );
  }

  return (
    <>
      <PageHeader
        back={{ to: "/schedules", label: "Schedules" }}
        title={s.name}
        meta={
          <>
            <StatusPill
              meta={
                s.enabled
                  ? { label: "Active", tone: "success" }
                  : { label: "Paused", tone: "neutral" }
              }
            />
            <span className="text-[13px] text-muted-foreground">
              {cronText(s.cron)}, {zoneLabel(s.timezone)}
            </span>
            <code className="rounded bg-muted px-1.5 py-0.5 text-xs">{s.cron}</code>
          </>
        }
        description={s.description || undefined}
        actions={
          <>
            <Button variant="outline" asChild>
              <Link to="/schedules/$scheduleId/edit" params={{ scheduleId: s.id }}>
                <Pencil aria-hidden />
                Edit
              </Link>
            </Button>
            <Button
              variant="outline"
              disabled={actions.pending}
              onClick={() => void actions.setEnabled(s, !s.enabled)}
            >
              {s.enabled ? <Pause aria-hidden /> : <Play aria-hidden />}
              {s.enabled ? "Pause" : "Resume"}
            </Button>
            <Button disabled={actions.pending} onClick={() => void actions.runNow(s)}>
              <Play aria-hidden />
              Run now
            </Button>
          </>
        }
      />

      {!s.enabled && (
        <div
          role="status"
          className="mb-6 flex flex-col gap-3 rounded-lg border border-warning/30 bg-warning-soft px-4 py-3.5 sm:flex-row sm:items-center"
        >
          <CirclePause className="hidden size-5 shrink-0 text-warning sm:block" aria-hidden />
          <div className="min-w-0 flex-1 text-[13px]">
            <div className="font-semibold text-warning">
              {s.pausedReason ? "Syncr paused this schedule" : "This schedule is paused"}
            </div>
            <p className="mt-0.5 text-foreground">
              {s.pausedReason
                ? pausedReasonLabel(s.pausedReason)
                : "No runs start until you resume it. Run now still works."}
            </p>
          </div>
          <div className="flex shrink-0 gap-2">
            {s.pausedReason && (
              <Button variant="outline" size="sm" className="bg-card" asChild>
                <Link to="/schedules/$scheduleId/edit" params={{ scheduleId: s.id }}>
                  Edit
                </Link>
              </Button>
            )}
            <Button size="sm" onClick={() => void actions.setEnabled(s, true)}>
              Resume
            </Button>
          </div>
        </div>
      )}

      <div className="flex flex-col gap-6">
        <div className="grid grid-cols-1 gap-6 lg:grid-cols-[minmax(0,2fr)_minmax(0,3fr)]">
          <Panel
            title="Next runs"
            description={
              s.enabled
                ? `In ${zoneLabel(s.timezone)}. Hover a time to see it in yours.`
                : "When it would run, once resumed."
            }
            actions={
              next.length > 1 && hasOffsetChange(next, s.timezone) ? (
                <span className="rounded-full bg-warning-soft px-2 py-0.5 text-[11px] font-medium text-warning">
                  Clocks change
                </span>
              ) : undefined
            }
          >
            {preview.isPending ? (
              <Skeleton className="h-36 w-full" />
            ) : next.length ? (
              <NextRunsList runs={next} timezone={s.timezone} />
            ) : (
              <p className="text-[13px] text-muted-foreground">
                This expression has no upcoming runs.
              </p>
            )}
          </Panel>
          <Panel title="Settings">
            <KeyValueList
              items={[
                { label: "Job", value: <JobSummary schedule={s} /> },
                { label: "Runs on", value: <TargetChip schedule={s} workers={workerMap} /> },
                { label: "Worker offline", value: missedSummary(s.missedRun) },
                { label: "Last run still going", value: overlapLabel(s.overlap) },
                {
                  label: "Notifies",
                  value: s.contacts.length ? (
                    <ul className="flex flex-col gap-1.5">
                      {s.contacts.map((c) => (
                        <li key={c.contactId} className="flex flex-wrap items-center gap-2">
                          <ChannelIcon type={c.channelType} className="size-5 [&_svg]:size-3" />
                          <Link
                            to="/notifications/$contactId"
                            params={{ contactId: c.contactId }}
                            className="font-medium hover:underline"
                          >
                            {c.contactName}
                          </Link>
                          <span className="text-xs text-muted-foreground">
                            {c.events.map((e) => eventMeta(e).label.toLowerCase()).join(", ")}
                          </span>
                        </li>
                      ))}
                    </ul>
                  ) : (
                    <span className="text-muted-foreground">Nobody</span>
                  ),
                },
                {
                  label: "Last fired",
                  value: <RelativeTime date={tsDate(s.lastFiredAt)} empty="Not yet" />,
                },
                { label: "Created", value: formatDateTime(tsDate(s.createdAt)) },
              ]}
            />
          </Panel>
        </div>

        <section className="flex flex-col gap-3">
          <div className="flex flex-wrap items-end justify-between gap-2">
            <div>
              <h2 className="text-[15px] font-semibold">Run history</h2>
              <p className="text-[13px] text-muted-foreground">
                Every time this schedule was due, and what happened.
              </p>
            </div>
            <Button variant="ghost" size="sm" asChild>
              <Link to="/jobs" search={{ schedule: s.id }}>
                <ListFilter aria-hidden />
                Its jobs
              </Link>
            </Button>
          </div>
          <ScheduleRunsTable
            runs={runs.data?.runs}
            timezone={s.timezone}
            isLoading={runs.isPending}
            error={runs.error ?? undefined}
            onRetry={() => void runs.refetch()}
            pager={{
              page: pager.page,
              canPrev: pager.canPrev,
              canNext: !!runs.data?.nextPageToken,
              onPrev: pager.prev,
              onNext: () => pager.next(runs.data?.nextPageToken ?? ""),
            }}
          />
        </section>

        <div className="flex flex-wrap justify-between gap-2 border-t pt-5">
          <Button variant="ghost" asChild>
            <Link to="/schedules/new" search={{ from: s.id }}>
              <Copy aria-hidden />
              Duplicate
            </Link>
          </Button>
          <Button
            variant="ghost"
            className="text-danger hover:text-danger"
            onClick={() => setDeleting(true)}
          >
            <Trash2 aria-hidden />
            Delete schedule
          </Button>
        </div>
      </div>

      <ConfirmDialog
        open={deleting}
        onOpenChange={setDeleting}
        destructive
        title={`Delete ${s.name}?`}
        description="Its run history is deleted with it. Jobs it already ran stay in the job list."
        confirmLabel="Delete schedule"
        onConfirm={async () => {
          await del.mutateAsync({ scheduleId: s.id });
          await Promise.all([
            invalidateService(queryClient, ScheduleService),
            invalidateService(queryClient, DashboardService),
          ]);
          toast.success(`Deleted ${s.name}`);
          await navigate({ to: "/schedules" });
        }}
      />
    </>
  );
}
