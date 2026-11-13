import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import {
  AlertTriangle,
  CalendarClock,
  CircleSlash,
  HardDrive,
  Pencil,
  RotateCw,
  TriangleAlert,
} from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { ConfirmDialog } from "@/components/app/confirm-dialog";
import { CopyButton } from "@/components/app/copy-button";
import { ErrorState } from "@/components/app/error-state";
import { KeyValueList } from "@/components/app/key-value-list";
import { PageHeader } from "@/components/app/page-header";
import { Panel } from "@/components/app/panel";
import { ProgressChart } from "@/components/app/progress-chart";
import { RelativeTime } from "@/components/app/relative-time";
import { StatCard, StatGrid } from "@/components/app/stat-card";
import { StatusBadge } from "@/components/app/status-badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { JobService, JobStatus, WorkerAdminService, type Job } from "@/gen/syncr/v1/api_pb";
import { JobErrorCode } from "@/gen/syncr/v1/link_pb";
import { ResticService } from "@/gen/syncr/v1/restic_pb";
import { UserService } from "@/gen/syncr/v1/users_pb";
import { useNow } from "@/hooks/use-now";
import { useSession } from "@/hooks/use-session";
import { useWatchJob, type JobSample, type WatchResult } from "@/hooks/use-watch-job";
import {
  durationSeconds,
  formatBytes,
  formatCount,
  formatDateTime,
  formatDuration,
  formatEta,
  formatLimit,
  formatRate,
  percent,
  rateBetween,
  shortId,
  toNumber,
  tsDate,
} from "@/lib/format";
import { errorMessage } from "@/lib/errors";
import { invalidateService } from "@/lib/invalidate";
import {
  errorCodeLabel,
  isResticKind,
  isTerminal,
  kindLabel,
  transferModeLabel,
} from "@/lib/status";
import { cn } from "@/lib/utils";

import {
  bisyncOf,
  bisyncOptionSummary,
  conflictsSummary,
  withoutOneShot,
} from "./bisync-definition";
import { BisyncRecovery } from "./bisync-recovery";
import { isEditableKind } from "./definition";
import { HookRunsPanel } from "./hook-runs";
import { hookSummary, hooksState } from "./hooks";
import { JobOutputCards, ResticBand } from "./job-output";
import { ScheduleChip, WaitingChip } from "./jobs-table";
import {
  jobElapsed,
  jobProgress,
  jobRoute,
  jobTitle,
  transferOf,
  type EndpointLabel,
} from "./model";

/** currentRate smooths the throughput over the last few samples. */
export function currentRate(samples: JobSample[], window = 3): number {
  if (samples.length < 2) return 0;
  const last = samples[samples.length - 1];
  const first = samples[Math.max(0, samples.length - 1 - window)];
  return rateBetween(first, last);
}

function EndpointCard({ label, e, worker }: { label: string; e: EndpointLabel; worker?: string }) {
  return (
    <div className="min-w-0 rounded-lg border bg-card px-4 py-3.5">
      <div className="text-xs text-muted-foreground">{label}</div>
      {e.local ? (
        <>
          <div className="mt-1 flex items-center gap-2 font-semibold">
            <HardDrive className="size-4 shrink-0 text-muted-foreground" aria-hidden />
            <span className="truncate">{worker ?? "Worker"} filesystem</span>
          </div>
          <div className="mt-0.5 truncate font-mono text-[12.5px]">{e.path || "/"}</div>
        </>
      ) : (
        <>
          <div
            className={cn(
              "mt-1 truncate font-semibold",
              e.deleted && "text-muted-foreground line-through",
            )}
          >
            {e.name}
            {e.deleted && <span className="ml-2 text-xs font-normal no-underline">deleted</span>}
          </div>
          <div className="mt-0.5 truncate font-mono text-[12.5px] text-muted-foreground">
            {e.path || "/"}
          </div>
        </>
      )}
    </div>
  );
}

/**
 * RouteBand is the job's two locations (a transfer's source and
 * destination, a two-way sync's folders) joined by its progress: the one
 * place the page shows movement while a job runs.
 */
function RouteBand({
  job,
  worker,
  p,
}: {
  job: Job;
  worker?: string;
  p: ReturnType<typeof jobProgress>;
}) {
  const route = jobRoute(job);
  const pct = job.status === JobStatus.SUCCEEDED ? 100 : percent(p.bytes, p.total);
  const running = job.status === JobStatus.RUNNING;
  const tone =
    job.status === JobStatus.SUCCEEDED
      ? "bg-success"
      : job.status === JobStatus.FAILED
        ? "bg-danger"
        : job.status === JobStatus.INTERRUPTED
          ? "bg-warning"
          : running || job.status === JobStatus.DISPATCHED
            ? "bg-live"
            : "bg-neutral";
  if (!route) return null;
  return (
    <div className="grid items-center gap-3 md:grid-cols-[minmax(0,1fr)_minmax(9rem,1.1fr)_minmax(0,1fr)]">
      <EndpointCard label={route.aLabel} e={route.a} worker={worker} />
      <div
        className="flex flex-col items-center gap-1.5 px-1"
        aria-label={pct === undefined ? "Progress unknown" : `${Math.round(pct)}% transferred`}
      >
        <span className="tabular text-xs text-muted-foreground">
          {pct === undefined ? route.mode : `${route.mode}, ${Math.floor(pct)}%`}
        </span>
        <div className="relative h-1.5 w-full overflow-hidden rounded-full bg-border">
          <div
            className={cn("h-full rounded-full transition-[width] duration-700", tone)}
            style={{
              width: `${pct ?? (running ? 100 : 0)}%`,
              opacity: pct === undefined ? 0.35 : 1,
            }}
          />
          {running && (
            <div className="absolute inset-0 animate-[route-flow_1.6s_linear_infinite] bg-[linear-gradient(90deg,transparent_0%,rgba(255,255,255,0.55)_50%,transparent_100%)] bg-[length:40%_100%] bg-no-repeat motion-reduce:hidden" />
          )}
        </div>
        <span className="tabular text-xs text-muted-foreground">
          {formatBytes(p.bytes)}
          {p.total > 0 && ` of ${formatBytes(p.total)}`}
        </span>
      </div>
      <EndpointCard label={route.bLabel} e={route.b} worker={worker} />
    </div>
  );
}

function Connection({ watch, terminal }: { watch: WatchResult; terminal: boolean }) {
  if (terminal || watch.state === "ended") {
    return <span className="text-xs text-muted-foreground">Finished</span>;
  }
  if (watch.state === "failed") {
    return (
      <span className="inline-flex items-center gap-2 text-xs text-danger" role="alert">
        <span className="size-1.5 rounded-full bg-current" aria-hidden />
        Live updates stopped: {errorMessage(watch.error)}
        <Button variant="outline" size="sm" className="h-6 px-2 text-xs" onClick={watch.retry}>
          Retry
        </Button>
      </span>
    );
  }
  const live = watch.state === "live";
  return (
    <span
      className={cn(
        "inline-flex items-center gap-1.5 text-xs",
        live ? "text-live" : "text-warning",
      )}
      role="status"
    >
      <span
        className={cn("size-1.5 rounded-full bg-current", live && "animate-pulse")}
        aria-hidden
      />
      {live ? "Live" : watch.state === "connecting" ? "Connecting…" : "Reconnecting…"}
    </span>
  );
}

function Timeline({ job }: { job: Job }) {
  const events: { label: string; at: Date | undefined; tone?: string }[] = [
    { label: "Queued", at: tsDate(job.createdAt) },
    { label: "Handed to the worker", at: tsDate(job.dispatchedAt) },
    { label: "Started", at: tsDate(job.startedAt) },
  ];
  if (isTerminal(job.status)) {
    const label =
      job.status === JobStatus.SUCCEEDED
        ? "Finished"
        : job.status === JobStatus.FAILED
          ? "Failed"
          : job.status === JobStatus.CANCELLED
            ? "Cancelled"
            : "Interrupted";
    events.push({ label, at: tsDate(job.finishedAt) });
  }
  return (
    <ol className="relative flex flex-col gap-4 pl-5">
      <span className="absolute top-1.5 bottom-1.5 left-[5px] w-px bg-border" aria-hidden />
      {events.map((e) => (
        <li key={e.label} className="relative">
          <span
            className={cn(
              "absolute top-1 -left-5 size-[11px] rounded-full border-2",
              e.at ? "border-primary bg-card" : "border-border bg-card",
            )}
            aria-hidden
          />
          <div className={cn("text-[13px] font-medium", !e.at && "text-muted-foreground")}>
            {e.label}
          </div>
          <div className="tabular text-xs text-muted-foreground">
            {e.at ? formatDateTime(e.at) : "Not yet"}
          </div>
        </li>
      ))}
    </ol>
  );
}

/** JobDetailPage follows one job live, with its chart, stats and outcome. */
export function JobDetailPage({ jobId }: { jobId: string }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { user, isAdmin } = useSession();
  const q = useQuery(JobService.method.getJob, { jobId });
  const watch = useWatchJob(jobId);
  const workers = useQuery(WorkerAdminService.method.listWorkers, {});
  const job = q.data?.job ?? watch.job;
  const foreign = !!job && !!user && job.ownerId !== user.id;
  const users = useQuery(UserService.method.listUsers, {}, { enabled: isAdmin && foreign });
  const cancel = useMutation(JobService.method.cancelJob);
  const rerun = useMutation(JobService.method.runJob);
  const [cancelling, setCancelling] = useState(false);
  // Only an unfinished job's clock moves: a finished page stays still.
  const now = useNow(1000, !!job && !isTerminal(job.status)).getTime();

  if (q.error && !job) return <ErrorState error={q.error} onRetry={() => void q.refetch()} />;
  if (!job) {
    return (
      <div className="flex flex-col gap-4">
        <Skeleton className="h-8 w-80" />
        <Skeleton className="h-24 w-full" />
        <Skeleton className="h-72 w-full" />
      </div>
    );
  }

  const terminal = isTerminal(job.status);
  const transfer = transferOf(job);
  const bisync = bisyncOf(job.definition);
  const worker = workers.data?.workers.find((w) => w.id === job.workerId);
  // Progress messages carry fresher counters than the last job message.
  const latest = terminal ? undefined : watch.samples.at(-1);
  const stored = jobProgress(job);
  const p = latest
    ? {
        ...stored,
        bytes: Math.max(stored.bytes, latest.bytes),
        files: Math.max(stored.files, latest.files),
      }
    : stored;
  const elapsed = jobElapsed(job, now);
  const rate = terminal ? (elapsed ? p.bytes / elapsed : 0) : currentRate(watch.samples);
  const errors = toNumber(job.stats?.errors);
  const owner = foreign ? users.data?.users.find((u) => u.id === job.ownerId) : undefined;

  const limit = durationSeconds(job.timeLimit);
  const timedOut = job.error?.code === JobErrorCode.TIME_LIMIT;
  const hookFailed = job.error?.code === JobErrorCode.HOOK_FAILED;
  const hooks = hookSummary(hooksState(job.definition));

  const runAgain = async () => {
    // A two-way sync's resync or force was for its one run: run the pair normally.
    const res = await rerun.mutateAsync({
      workerId: job.workerId,
      definition: withoutOneShot(job.definition),
    });
    await invalidateService(queryClient, JobService);
    // A restic job changes its repository's state (running jobs, snapshots).
    if (isResticKind(job.kind)) await invalidateService(queryClient, ResticService);
    toast.success("Job queued again");
    if (res.job) await navigate({ to: "/jobs/$jobId", params: { jobId: res.job.id } });
  };

  return (
    <>
      <PageHeader
        back={{ to: "/jobs", label: "Jobs" }}
        title={jobTitle(job)}
        meta={
          <>
            <StatusBadge job={job.status} cancelRequested={job.cancelRequested} />
            <span className="text-[13px] text-muted-foreground">
              {kindLabel(job.kind)} on {worker?.name ?? "a removed worker"}, queued{" "}
              <RelativeTime date={tsDate(job.createdAt)} />
              {foreign && owner && <> by {owner.displayName || owner.username}</>}
            </span>
            <WaitingChip job={job} />
            {job.scheduleId && <ScheduleChip job={job} />}
            <span
              className="inline-flex items-center gap-1 font-mono text-xs text-muted-foreground"
              title={job.id}
            >
              {shortId(job.id)}
              <CopyButton
                value={job.id}
                label="Copy ID"
                variant="ghost"
                className="h-6 px-1.5 text-xs"
              />
            </span>
          </>
        }
        actions={
          <>
            {!terminal && (
              <Button
                variant="outline"
                className="text-danger hover:text-danger"
                disabled={job.cancelRequested}
                onClick={() => setCancelling(true)}
              >
                <CircleSlash aria-hidden />
                {job.cancelRequested ? "Cancelling…" : "Cancel job"}
              </Button>
            )}
            {!foreign && !job.scheduleId && isEditableKind(job.kind) && (
              <Button variant="outline" asChild>
                <Link to="/schedules/new" search={{ job: job.id }}>
                  <CalendarClock aria-hidden />
                  Schedule
                </Link>
              </Button>
            )}
            {!foreign && isEditableKind(job.kind) && (
              <Button variant="outline" asChild>
                <Link to="/jobs/new" search={{ from: job.id }}>
                  <Pencil aria-hidden />
                  Edit and run
                </Link>
              </Button>
            )}
            {terminal && !foreign && isEditableKind(job.kind) && (
              <Button
                onClick={() => void runAgain().catch((e: unknown) => toast.error(String(e)))}
                disabled={rerun.isPending}
              >
                <RotateCw aria-hidden />
                Run again
              </Button>
            )}
          </>
        }
      />

      <div className="flex flex-col gap-6">
        <RouteBand job={job} worker={worker?.name} p={p} />
        <ResticBand job={job} worker={worker?.name} />

        <StatGrid>
          <StatCard
            label={transfer || bisync ? "Transferred" : "Processed"}
            value={formatBytes(p.bytes)}
            sub={
              p.total > 0
                ? `of ${formatBytes(p.total)}`
                : terminal
                  ? undefined
                  : "Total not known yet"
            }
          />
          <StatCard
            label="Files"
            value={formatCount(p.files)}
            sub={
              p.totalFiles > 0
                ? `of ${formatCount(p.totalFiles)}`
                : `${formatCount(job.stats?.checks)} checked`
            }
          />
          <StatCard
            label={terminal ? "Average speed" : "Speed"}
            value={formatRate(rate)}
            tone={!terminal && rate > 0 ? "live" : "default"}
            sub={terminal ? undefined : "Last 15 seconds"}
          />
          <StatCard
            label={terminal ? "Duration" : "Time left"}
            value={
              terminal
                ? elapsed === undefined
                  ? "–"
                  : formatDuration(elapsed)
                : formatEta(p.total - p.bytes, rate)
            }
            sub={
              !terminal && elapsed !== undefined
                ? `${formatDuration(elapsed)} elapsed${limit > 0 ? `, limit ${formatLimit(limit)}` : ""}`
                : timedOut && limit > 0
                  ? `Time limit ${formatLimit(limit)}`
                  : undefined
            }
          />
          <StatCard
            label="Errors"
            value={formatCount(errors)}
            tone={errors > 0 ? "danger" : "default"}
            sub={job.requeues > 0 ? `Requeued ${job.requeues}×` : undefined}
          />
        </StatGrid>

        {job.error && (
          <div
            role="alert"
            className="flex gap-3 rounded-lg border border-danger/30 bg-danger-soft px-4 py-3.5"
          >
            <AlertTriangle className="mt-0.5 size-4 shrink-0 text-danger" aria-hidden />
            <div className="min-w-0">
              <div className="text-[13px] font-semibold text-danger">
                {errorCodeLabel(job.error.code)}
                {job.exitCode !== undefined && !hookFailed && (
                  <span className="font-normal"> (exit code {job.exitCode})</span>
                )}
              </div>
              {hookFailed && (
                <p className="mt-0.5 text-[13px] leading-5">
                  {job.exitCode !== undefined
                    ? "The job itself finished: what it did is kept. A hook that ran after it did not succeed."
                    : "A hook that runs before the job did not succeed, so the job never ran."}{" "}
                  The hooks below show what happened.
                </p>
              )}
              {timedOut && (
                <p className="mt-0.5 text-[13px] leading-5">
                  It ran longer than its time limit
                  {limit > 0 ? ` of ${formatLimit(limit)}` : ""}, so the worker stopped it. Raise
                  the limit (Edit and run) if the job needs more time.
                </p>
              )}
              <p className="mt-0.5 font-mono text-[12.5px] leading-5 break-words">
                {job.error.message}
              </p>
              <BisyncRecovery job={job} canRun={!foreign} workers={workers.data?.workers} />
            </div>
          </div>
        )}
        {job.warnings.length > 0 && (
          <div className="flex gap-3 rounded-lg border border-warning/30 bg-warning-soft px-4 py-3.5">
            <TriangleAlert className="mt-0.5 size-4 shrink-0 text-warning" aria-hidden />
            <div className="min-w-0">
              <div className="text-[13px] font-semibold text-warning">
                {job.warnings.length === 1 ? "1 warning" : `${job.warnings.length} warnings`}
              </div>
              <ul className="mt-0.5 flex flex-col gap-0.5 text-[13px]">
                {job.warnings.map((w, i) => (
                  <li key={i}>{w}</li>
                ))}
              </ul>
            </div>
          </div>
        )}

        <JobOutputCards job={job} />
        <HookRunsPanel job={job} />

        <Panel title="Throughput" actions={<Connection watch={watch} terminal={terminal} />}>
          <ProgressChart
            samples={watch.samples}
            live={!terminal}
            empty={
              terminal && watch.history !== "loading"
                ? "No progress history (it is kept for a limited time)"
                : undefined
            }
            note={
              watch.thinned
                ? watch.total > watch.samples.length
                  ? `Thinned: ${formatCount(watch.samples.length)} of ${formatCount(watch.total)} progress samples shown, evenly spread.`
                  : "Thinned: older progress samples are left out, evenly spread."
                : undefined
            }
          />
        </Panel>

        <div className="grid gap-6 lg:grid-cols-[minmax(0,1fr)_minmax(0,2fr)]">
          <Panel title="Timeline">
            <Timeline job={job} />
          </Panel>
          <Panel title="Details">
            <KeyValueList
              items={[
                { label: "Job ID", value: job.id, mono: true },
                {
                  label: "Worker",
                  value: worker ? (
                    <Link
                      to="/workers/$workerId"
                      params={{ workerId: worker.id }}
                      className="text-primary hover:underline"
                    >
                      {worker.name}
                    </Link>
                  ) : (
                    "Removed"
                  ),
                },
                {
                  label: "Time limit",
                  value: limit > 0 ? formatLimit(limit) : "None",
                },
                ...(hooks ? [{ label: "Hooks", value: hooks }] : []),
                {
                  label: "Started by",
                  value: job.scheduleId ? (
                    <Link
                      to="/schedules/$scheduleId"
                      params={{ scheduleId: job.scheduleId }}
                      className="text-primary hover:underline"
                    >
                      {job.schedule?.name || "A deleted schedule"}
                    </Link>
                  ) : (
                    "Run by hand"
                  ),
                },
                ...(transfer
                  ? [
                      { label: "Mode", value: transferModeLabel(transfer.mode) },
                      { label: "Options", value: optionSummary(job) },
                    ]
                  : bisync
                    ? [
                        { label: "Kind", value: kindLabel(job.kind) },
                        ...(bisync.resync || bisync.force
                          ? [
                              {
                                label: "This run",
                                value: [
                                  bisync.resync && "Resync",
                                  bisync.force && "Delete limit ignored",
                                ]
                                  .filter(Boolean)
                                  .join(", "),
                              },
                            ]
                          : []),
                        { label: "Options", value: bisyncOptionSummary(bisync) },
                        { label: "Conflicts", value: conflictsSummary(bisync) },
                      ]
                    : [
                        { label: "Kind", value: kindLabel(job.kind) },
                        ...(job.repository
                          ? [
                              {
                                label: "Repository",
                                value: job.repository.deleted
                                  ? `${job.repository.name} (removed)`
                                  : job.repository.name,
                              },
                            ]
                          : []),
                      ]),
                ...(job.stats
                  ? [
                      {
                        label: "Checks and deletes",
                        value: `${formatCount(job.stats.checks)} checked, ${formatCount(job.stats.deletes)} deleted, ${formatCount(job.stats.renames)} renamed`,
                      },
                    ]
                  : []),
              ]}
            />
          </Panel>
        </div>
      </div>

      <ConfirmDialog
        open={cancelling}
        onOpenChange={setCancelling}
        destructive
        title="Cancel this job?"
        description={
          job.status === JobStatus.QUEUED
            ? "It hasn't started, so it is cancelled at once."
            : "The worker is asked to stop. Files already transferred stay where they are."
        }
        confirmLabel="Cancel job"
        onConfirm={async () => {
          await cancel.mutateAsync({ jobId: job.id });
          await invalidateService(queryClient, JobService);
        }}
      />
    </>
  );
}

/** optionSummary lists the options that differ from rclone's defaults. */
function optionSummary(job: Job): string {
  const o = transferOf(job)?.options;
  if (!o) return "Defaults";
  const parts: string[] = [];
  if (o.dryRun) parts.push("dry run");
  if (o.checksum) parts.push("compare by checksum");
  if (o.sizeOnly) parts.push("compare by size");
  if (o.transfers) parts.push(`${o.transfers} transfers`);
  if (o.checkers) parts.push(`${o.checkers} checkers`);
  if (o.bandwidthLimitBytesPerSecond)
    parts.push(`limit ${formatRate(toNumber(o.bandwidthLimitBytesPerSecond))}`);
  if (o.ignoreExisting) parts.push("skip existing");
  if (o.update) parts.push("skip newer");
  if (o.trackRenames) parts.push("detect renames");
  if (o.fastList) parts.push("fast listing");
  if (o.filters.length)
    parts.push(`${o.filters.length} ${o.filters.length === 1 ? "filter" : "filters"}`);
  return parts.length ? parts.join(", ") : "Defaults";
}
