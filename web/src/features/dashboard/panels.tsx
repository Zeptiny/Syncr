import { Link } from "@tanstack/react-router";
import { CalendarClock, CircleCheck, Tag } from "lucide-react";

import { RelativeTime } from "@/components/app/relative-time";
import { ScheduleRunStatusBadge, StatusBadge } from "@/components/app/status-badge";
import { toneDot } from "@/lib/tones";
import { WorkerStatus, type Job, type Worker } from "@/gen/syncr/v1/api_pb";
import type { UpcomingRun, WorkerHealth } from "@/gen/syncr/v1/dashboard_pb";
import type { Schedule, ScheduleRun } from "@/gen/syncr/v1/schedules_pb";
import { formatDateTime, tsDate } from "@/lib/format";
import { errorCodeLabel, runReasonLabel, workerStatusMeta } from "@/lib/status";
import { sortWorkers } from "@/lib/workers";
import { formatInZone, zoneLabel } from "@/lib/timezones";
import { cn } from "@/lib/utils";
import { jobTitle } from "@/features/jobs/model";

function Empty({ children }: { children: React.ReactNode }) {
  return <p className="px-5 py-8 text-center text-[13px] text-muted-foreground">{children}</p>;
}

const viewerTime = new Intl.DateTimeFormat("en-GB", {
  weekday: "short",
  day: "numeric",
  month: "short",
  hour: "2-digit",
  minute: "2-digit",
  timeZoneName: "short",
});

/**
 * runTime is when a run is due as the schedules pages show it: wall time in
 * the schedule's zone, with the zone and the viewer's time on hover. A run
 * of a schedule the viewer cannot list (another user's, for an admin
 * viewing all users) is shown in the viewer's zone, named.
 */
function runTime(at: Date, schedule: Schedule | undefined): { text: string; title: string } {
  if (!schedule?.timezone) return { text: viewerTime.format(at), title: formatDateTime(at) };
  return {
    text: formatInZone(at, schedule.timezone, false),
    title: `In ${zoneLabel(schedule.timezone)}. Your time: ${formatDateTime(at)}`,
  };
}

/**
 * UpcomingRuns lists the next scheduled runs with their target worker's
 * status. Times are in each schedule's zone, like the schedules pages
 * (schedules is the viewer's schedules by ID).
 */
export function UpcomingRuns({
  runs,
  schedules,
}: {
  runs: UpcomingRun[];
  schedules: Map<string, Schedule>;
}) {
  if (!runs.length) {
    return (
      <Empty>
        No runs are scheduled.{" "}
        <Link to="/schedules/new" className="text-primary hover:underline">
          Add a schedule
        </Link>{" "}
        to run a job by itself.
      </Empty>
    );
  }
  return (
    <ul className="divide-y">
      {runs.map((r) => {
        const at = tsDate(r.at);
        const when = at ? runTime(at, schedules.get(r.scheduleId)) : undefined;
        const t = r.target?.target;
        const meta = workerStatusMeta(r.workerStatus);
        return (
          <li key={r.scheduleId}>
            <Link
              to="/schedules/$scheduleId"
              params={{ scheduleId: r.scheduleId }}
              className="flex items-center gap-3 px-5 py-2.5 hover:bg-accent/50"
            >
              <div className="min-w-0 flex-1">
                <div className="truncate text-[13px] font-medium">{r.scheduleName}</div>
                <div className="flex min-w-0 items-center gap-1.5 text-xs text-muted-foreground">
                  {t?.case === "workerLabel" && (
                    <span className="inline-flex shrink-0 items-center gap-1 rounded bg-secondary px-1 font-mono text-[11px] text-secondary-foreground">
                      <Tag className="size-2.5 opacity-60" aria-hidden />
                      {t.value}
                    </span>
                  )}
                  {r.workerName ? (
                    <span className="flex min-w-0 items-center gap-1">
                      <span
                        className={cn("size-1.5 shrink-0 rounded-full", toneDot[meta.tone])}
                        aria-hidden
                      />
                      <span className="truncate">
                        {r.workerName} {meta.label.toLowerCase()}
                      </span>
                    </span>
                  ) : (
                    <span className="text-warning">no worker</span>
                  )}
                </div>
              </div>
              <div className="shrink-0 text-right">
                <RelativeTime date={at} className="tabular block text-[13px] font-medium" />
                <span
                  className="tabular text-xs whitespace-nowrap text-muted-foreground"
                  title={when?.title}
                >
                  {when?.text}
                </span>
              </div>
            </Link>
          </li>
        );
      })}
    </ul>
  );
}

/** WorkerHealthPanel shows how many workers are online, with the list. */
export function WorkerHealthPanel({ health }: { health: WorkerHealth | undefined }) {
  const workers: Worker[] = sortWorkers(health?.workers ?? []);
  const total = (health?.online ?? 0) + (health?.offline ?? 0) + (health?.pending ?? 0);
  if (!total) {
    return (
      <Empty>
        No workers yet.{" "}
        <Link to="/workers/new" className="text-primary hover:underline">
          Add one
        </Link>{" "}
        to start moving files.
      </Empty>
    );
  }
  const parts = [
    { n: health?.online ?? 0, label: "online", cls: "bg-success" },
    { n: health?.offline ?? 0, label: "offline", cls: "bg-neutral/45" },
    { n: health?.pending ?? 0, label: "pending", cls: "bg-warning" },
  ];
  return (
    <div className="flex flex-col">
      <div className="px-5 pt-4 pb-3">
        <div className="flex items-baseline gap-1.5">
          <span className="tabular text-2xl font-semibold tracking-[-0.01em]">
            {health?.online ?? 0}
          </span>
          <span className="text-[13px] text-muted-foreground">of {total} online</span>
        </div>
        <div className="mt-2 flex h-1.5 gap-0.5 overflow-hidden rounded-full" aria-hidden>
          {parts
            .filter((p) => p.n > 0)
            .map((p) => (
              <span key={p.label} className={p.cls} style={{ flexGrow: p.n }} />
            ))}
        </div>
        <div className="mt-2 flex gap-3 text-xs text-muted-foreground">
          {parts.map((p) => (
            <span key={p.label} className="tabular">
              {p.n} {p.label}
            </span>
          ))}
        </div>
      </div>
      <ul className="divide-y border-t">
        {workers.slice(0, 6).map((w) => (
          <li key={w.id}>
            <Link
              to="/workers/$workerId"
              params={{ workerId: w.id }}
              className="flex items-center gap-2.5 px-5 py-2 text-[13px] hover:bg-accent/50"
            >
              <span
                className={cn(
                  "size-2 shrink-0 rounded-full",
                  toneDot[workerStatusMeta(w.status).tone],
                )}
                aria-label={workerStatusMeta(w.status).label}
              />
              <span className="min-w-0 flex-1 truncate font-medium">{w.name}</span>
              <span className="shrink-0 text-xs text-muted-foreground">
                {w.status === WorkerStatus.ONLINE ? (
                  "online"
                ) : w.lastSeen ? (
                  <>
                    seen <RelativeTime date={tsDate(w.lastSeen)} />
                  </>
                ) : (
                  "never connected"
                )}
              </span>
            </Link>
          </li>
        ))}
      </ul>
    </div>
  );
}

type Problem =
  | { kind: "job"; at: Date | undefined; job: Job }
  | { kind: "run"; at: Date | undefined; run: ScheduleRun };

/**
 * RecentFailures merges the latest failed or interrupted jobs with the
 * latest missed or failed schedule runs, newest first.
 */
export function RecentFailures({
  jobs,
  runs,
  schedules,
}: {
  jobs: Job[];
  runs: ScheduleRun[];
  schedules: Map<string, Schedule>;
}) {
  const items: Problem[] = [
    ...jobs.map((job): Problem => ({
      kind: "job",
      at: tsDate(job.finishedAt ?? job.createdAt),
      job,
    })),
    ...runs.map((run): Problem => ({ kind: "run", at: tsDate(run.scheduledFor), run })),
  ].sort((a, b) => (b.at?.getTime() ?? 0) - (a.at?.getTime() ?? 0));
  if (!items.length) {
    return (
      <div className="flex flex-col items-center gap-2 px-5 py-8 text-center text-[13px] text-muted-foreground">
        <CircleCheck className="size-5 text-success" aria-hidden />
        Nothing failed or was missed lately.
      </div>
    );
  }
  return (
    <ul className="divide-y">
      {items.slice(0, 8).map((p) =>
        p.kind === "job" ? (
          <li key={`j${p.job.id}`}>
            <Link
              to="/jobs/$jobId"
              params={{ jobId: p.job.id }}
              className="flex items-start gap-3 px-5 py-2.5 hover:bg-accent/50"
            >
              <StatusBadge job={p.job.status} className="mt-0.5 w-[6.5rem] justify-center" />
              <div className="min-w-0 flex-1">
                <div className="truncate text-[13px] font-medium">{jobTitle(p.job)}</div>
                <div className="truncate text-xs text-muted-foreground">
                  {p.job.schedule?.name && (
                    <span className="inline-flex items-center gap-1">
                      <CalendarClock className="size-3" aria-hidden />
                      {p.job.schedule.name}:{" "}
                    </span>
                  )}
                  {p.job.error
                    ? `${errorCodeLabel(p.job.error.code)}. ${p.job.error.message}`
                    : "No details."}
                </div>
              </div>
              <RelativeTime date={p.at} className="shrink-0 text-xs text-muted-foreground" />
            </Link>
          </li>
        ) : (
          <li key={`r${p.run.id}`}>
            <Link
              to="/schedules/$scheduleId"
              params={{ scheduleId: p.run.scheduleId }}
              className="flex items-start gap-3 px-5 py-2.5 hover:bg-accent/50"
              title={p.at ? `Due ${formatDateTime(p.at)}` : undefined}
            >
              <ScheduleRunStatusBadge
                status={p.run.status}
                className="mt-0.5 w-[6.5rem] justify-center"
              />
              <div className="min-w-0 flex-1">
                <div className="truncate text-[13px] font-medium">
                  {schedules.get(p.run.scheduleId)?.name ?? "A schedule"}
                </div>
                <div className="truncate text-xs text-muted-foreground">
                  {runReasonLabel(p.run.reason) || "The run did not start."}
                  {p.run.coalesced > 0 && `, ${p.run.coalesced} earlier runs merged`}
                </div>
              </div>
              <RelativeTime date={p.at} className="shrink-0 text-xs text-muted-foreground" />
            </Link>
          </li>
        ),
      )}
    </ul>
  );
}
