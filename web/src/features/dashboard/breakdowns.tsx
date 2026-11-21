import { Link } from "@tanstack/react-router";
import { CircleCheck } from "lucide-react";

import { bucketLabel, type ActivityPoint, type BucketUnit } from "@/components/app/charts/activity";
import { RelativeTime } from "@/components/app/relative-time";
import { StatusPill } from "@/components/app/status-badge";
import { JobStatus } from "@/gen/syncr/v1/api_pb";
import type {
  ActivityGroup,
  FailureReason,
  RepositoryGrowth,
  ScheduleHealth,
} from "@/gen/syncr/v1/dashboard_pb";
import { ScheduleRunStatus, type ScheduleRun } from "@/gen/syncr/v1/schedules_pb";
import {
  durationSeconds,
  formatBytes,
  formatCount,
  formatDuration,
  toNumber,
  tsDate,
} from "@/lib/format";
import { errorCodeLabel, kindLabel, type StatusMeta } from "@/lib/status";
import { cn } from "@/lib/utils";

import type { DashboardRangeKey } from "./search";

function Empty({ children }: { children: React.ReactNode }) {
  return <p className="px-5 py-8 text-center text-[13px] text-muted-foreground">{children}</p>;
}

/** GroupStats are an ActivityGroup's figures as plain numbers. */
export interface GroupStats {
  jobs: number;
  succeeded: number;
  failed: number;
  interrupted: number;
  cancelled: number;
  /** successRate is succeeded / (succeeded + failed + interrupted); undefined without such jobs. */
  successRate: number | undefined;
  bytes: number;
  /** avgRun and avgWait are averages in seconds; undefined without jobs that ran or waited. */
  avgRun: number | undefined;
  avgWait: number | undefined;
}

/** groupStats computes the figures of g. */
export function groupStats(g: ActivityGroup): GroupStats {
  const succeeded = toNumber(g.succeeded);
  const failed = toNumber(g.failed);
  const interrupted = toNumber(g.interrupted);
  const cancelled = toNumber(g.cancelled);
  const finished = succeeded + failed + interrupted;
  const started = toNumber(g.started);
  const dispatched = toNumber(g.dispatched);
  return {
    jobs: finished + cancelled,
    succeeded,
    failed,
    interrupted,
    cancelled,
    successRate: finished ? succeeded / finished : undefined,
    bytes: toNumber(g.bytes),
    avgRun: started ? durationSeconds(g.runTime) / started : undefined,
    avgWait: dispatched ? durationSeconds(g.queueWait) / dispatched : undefined,
  };
}

/** formatRate renders a rate in [0, 1] as "96%" or "99.4%". */
export function formatRate(rate: number | undefined): string {
  if (rate === undefined) return "–";
  const pct = rate * 100;
  return `${pct >= 99.95 || pct === 0 || Number.isInteger(pct) ? pct.toFixed(0) : pct.toFixed(1)}%`;
}

const outcomes = [
  { key: "succeeded", label: "succeeded", cls: "bg-success" },
  { key: "failed", label: "failed", cls: "bg-danger" },
  { key: "interrupted", label: "interrupted", cls: "bg-warning" },
  { key: "cancelled", label: "cancelled", cls: "bg-neutral/50" },
] as const;

/**
 * OutcomeBar draws a group's jobs as one bar, split by outcome with a 2px
 * gap between the parts, as wide as the group's share of the largest one.
 */
export function OutcomeBar({ stats, max }: { stats: GroupStats; max: number }) {
  const parts = outcomes.filter((o) => stats[o.key] > 0);
  return (
    <div className="h-1.5 w-full" aria-hidden>
      <div
        className="flex h-full gap-0.5"
        style={{ width: `${max > 0 ? Math.max(2, (stats.jobs / max) * 100) : 0}%` }}
      >
        {parts.map((o) => (
          <span
            key={o.key}
            className={cn("h-full first:rounded-l-full last:rounded-r-full", o.cls)}
            style={{ flexGrow: stats[o.key] }}
            title={`${stats[o.key]} ${o.label}`}
          />
        ))}
      </div>
    </div>
  );
}

/** The jobs list's status filters: the failed and interrupted jobs, and
 * every finished job (what a group counts). */
const failedParam = `${JobStatus.FAILED},${JobStatus.INTERRUPTED}`;
const finishedParam = `${JobStatus.SUCCEEDED},${JobStatus.FAILED},${JobStatus.INTERRUPTED},${JobStatus.CANCELLED}`;

/**
 * ActivityGroups lists the period's finished jobs by job kind or by
 * worker: each row a bar split by outcome, the success rate, the data
 * moved and the average run time and queue wait, linking to those jobs.
 */
export function ActivityGroups({
  groups,
  by,
  period,
  allUsers,
}: {
  groups: ActivityGroup[];
  by: "kind" | "worker";
  period: DashboardRangeKey;
  allUsers: boolean;
}) {
  if (!groups.length) {
    return (
      <Empty>
        {by === "kind"
          ? "Each job kind's results appear here once jobs finish."
          : "Each worker's results appear here once jobs finish."}
      </Empty>
    );
  }
  const rows = groups.map((g) => ({ g, s: groupStats(g) }));
  const max = Math.max(...rows.map((r) => r.s.jobs));
  const shown = rows.slice(0, 8);
  return (
    <ul className="divide-y">
      {shown.map(({ g, s }) => {
        const name = by === "kind" ? kindLabel(g.key) : g.name || g.key;
        return (
          <li key={g.key}>
            <Link
              to="/jobs"
              search={{
                ...(by === "kind" ? { kind: g.key } : { worker: g.key }),
                // Only the finished jobs, as the row counts them.
                status: finishedParam,
                period,
                all: allUsers || undefined,
              }}
              className="flex flex-col gap-1.5 px-5 py-2.5 hover:bg-accent/50"
              aria-label={`${name}: ${s.jobs} jobs, ${formatRate(s.successRate)} succeeded`}
            >
              <div className="flex items-baseline gap-2 text-[13px]">
                <span className="min-w-0 flex-1 truncate font-medium">{name}</span>
                <span className="tabular shrink-0 text-muted-foreground">
                  {formatCount(s.jobs)} {s.jobs === 1 ? "job" : "jobs"}
                </span>
                <span
                  className={cn(
                    "tabular w-12 shrink-0 text-right font-medium",
                    s.successRate !== undefined && s.successRate < 0.9 && "text-danger",
                  )}
                >
                  {formatRate(s.successRate)}
                </span>
              </div>
              <OutcomeBar stats={s} max={max} />
              <div className="tabular flex flex-wrap gap-x-3 text-xs text-muted-foreground">
                {s.failed + s.interrupted > 0 && (
                  <span>
                    {[
                      s.failed && `${s.failed} failed`,
                      s.interrupted && `${s.interrupted} interrupted`,
                    ]
                      .filter(Boolean)
                      .join(", ")}
                  </span>
                )}
                {s.bytes > 0 && <span>{formatBytes(s.bytes)}</span>}
                {s.avgRun !== undefined && <span>ran {formatDuration(s.avgRun)} on average</span>}
                {s.avgWait !== undefined && s.avgWait >= 1 && (
                  <span>waited {formatDuration(s.avgWait)}</span>
                )}
              </div>
            </Link>
          </li>
        );
      })}
      {rows.length > shown.length && (
        <li className="px-5 py-2 text-xs text-muted-foreground">
          {rows.length - shown.length} more with fewer jobs
        </li>
      )}
    </ul>
  );
}

/**
 * FailureReasons lists why the period's failed and interrupted jobs ended,
 * most frequent first, with the job kinds they were.
 */
export function FailureReasons({
  reasons,
  period,
  allUsers,
}: {
  reasons: FailureReason[];
  period: DashboardRangeKey;
  allUsers: boolean;
}) {
  if (!reasons.length) {
    return (
      <div className="flex flex-col items-center gap-2 px-5 py-8 text-center text-[13px] text-muted-foreground">
        <CircleCheck className="size-5 text-success" aria-hidden />
        No job failed in this period.
      </div>
    );
  }
  const count = (r: FailureReason) => toNumber(r.failed) + toNumber(r.interrupted);
  const max = Math.max(...reasons.map(count));
  return (
    <ul className="divide-y">
      {reasons.map((r) => {
        const n = count(r);
        const single = r.kinds.length === 1 ? r.kinds[0] : undefined;
        const opens = `Opens every failed and interrupted ${single ? `${kindLabel(single)} job` : "job"} of the period, whatever the cause.`;
        return (
          <li key={r.code}>
            <Link
              to="/jobs"
              search={{ status: failedParam, kind: single, period, all: allUsers || undefined }}
              className="flex flex-col gap-1.5 px-5 py-2.5 hover:bg-accent/50"
              // The jobs list cannot filter by cause: say what the link opens.
              aria-label={`${errorCodeLabel(r.code)}: ${formatCount(n)} jobs. ${opens}`}
              title={opens}
            >
              <div className="flex items-baseline gap-2 text-[13px]">
                <span className="min-w-0 flex-1 truncate font-medium">
                  {errorCodeLabel(r.code)}
                </span>
                <span className="tabular shrink-0 font-medium">{formatCount(n)}</span>
              </div>
              <div className="h-1.5 w-full" aria-hidden>
                <div
                  className="h-full rounded-full bg-danger/80"
                  style={{ width: `${Math.max(2, (n / max) * 100)}%` }}
                />
              </div>
              <div className="truncate text-xs text-muted-foreground">
                {r.kinds.map(kindLabel).join(", ")}
                {toNumber(r.interrupted) > 0 &&
                  ` · ${formatCount(r.interrupted)} interrupted${toNumber(r.failed) ? `, ${formatCount(r.failed)} failed` : ""}`}
              </div>
            </Link>
          </li>
        );
      })}
    </ul>
  );
}

/** RunState is how a schedule's newest run went, for the health list. */
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

/** scheduleCounts sums the schedules up for the panel's header. */
export function scheduleCounts(list: ScheduleHealth[]): {
  healthy: number;
  failing: number;
  paused: number;
  neverRan: number;
} {
  const out = { healthy: 0, failing: 0, paused: 0, neverRan: 0 };
  for (const s of list) {
    const st = runState(s.latestRun);
    if (st?.problem) out.failing++;
    else if (!s.enabled) out.paused++;
    else if (!s.latestRun) out.neverRan++;
    else out.healthy++;
  }
  return out;
}

/**
 * ScheduleHealthList shows the caller's schedules as the server orders them
 * (those whose newest run did not succeed first) with when each last
 * succeeded, and the missed runs of the period.
 */
export function ScheduleHealthList({
  schedules,
  missed,
  rangeNoun,
}: {
  schedules: ScheduleHealth[];
  missed: number;
  rangeNoun: string;
}) {
  if (!schedules.length) {
    return (
      <Empty>
        No schedules yet.{" "}
        <Link to="/schedules/new" className="text-primary hover:underline">
          Add one
        </Link>{" "}
        to run a job by itself.
      </Empty>
    );
  }
  const c = scheduleCounts(schedules);
  const parts = [
    { n: c.healthy, label: "healthy", cls: "bg-success" },
    { n: c.failing, label: "need attention", cls: "bg-danger" },
    { n: c.neverRan, label: "not run yet", cls: "bg-neutral/45" },
    { n: c.paused, label: "paused", cls: "bg-neutral/25" },
  ];
  return (
    <div className="flex flex-col">
      <div className="px-5 pt-4 pb-3">
        <div className="flex items-baseline gap-1.5">
          <span className="tabular text-2xl font-semibold tracking-[-0.01em]">{c.healthy}</span>
          <span className="text-[13px] text-muted-foreground">of {schedules.length} healthy</span>
        </div>
        <div className="mt-2 flex h-1.5 gap-0.5 overflow-hidden rounded-full" aria-hidden>
          {parts
            .filter((p) => p.n > 0)
            .map((p) => (
              <span key={p.label} className={p.cls} style={{ flexGrow: p.n }} />
            ))}
        </div>
        <div className="mt-2 flex flex-wrap gap-x-3 text-xs text-muted-foreground">
          {parts
            .filter((p) => p.n > 0)
            .map((p) => (
              <span key={p.label} className="tabular">
                {p.n} {p.label}
              </span>
            ))}
          <span className={cn("tabular", missed > 0 && "text-warning")}>
            {missed
              ? `${formatCount(missed)} runs missed in ${rangeNoun}`
              : `no runs missed in ${rangeNoun}`}
          </span>
        </div>
      </div>
      <ul className="divide-y border-t">
        {schedules.slice(0, 6).map((s) => {
          const st = runState(s.latestRun);
          const success = tsDate(s.lastSuccessAt);
          return (
            <li key={s.scheduleId}>
              <Link
                to="/schedules/$scheduleId"
                params={{ scheduleId: s.scheduleId }}
                className="flex items-center gap-3 px-5 py-2 hover:bg-accent/50"
              >
                <div className="min-w-0 flex-1">
                  <div className="truncate text-[13px] font-medium">{s.scheduleName}</div>
                  <div className="truncate text-xs text-muted-foreground">
                    {kindLabel(s.kind)}
                    {!s.enabled && " · paused"}
                    {" · "}
                    {success ? (
                      <>
                        last succeeded <RelativeTime date={success} />
                      </>
                    ) : (
                      "never succeeded"
                    )}
                  </div>
                </div>
                {st ? (
                  <StatusPill meta={st.meta} className="shrink-0" />
                ) : (
                  <span className="shrink-0 text-xs text-muted-foreground">not run yet</span>
                )}
              </Link>
            </li>
          );
        })}
      </ul>
    </div>
  );
}

/** signedBytes renders a size change: "+120 MiB", "−2.00 GiB", "no change". */
export function signedBytes(delta: number): string {
  if (delta === 0) return "no change";
  return `${delta > 0 ? "+" : "−"}${formatBytes(Math.abs(delta))}`;
}

/**
 * AddedBars is a small bar chart of what a repository's backups added per
 * bucket, on its own scale; each bar shows its bucket and size on hover.
 */
function AddedBars({
  added,
  points,
  unit,
}: {
  added: bigint[];
  points: ActivityPoint[];
  unit: BucketUnit;
}) {
  const values = added.map(toNumber);
  const max = Math.max(0, ...values);
  if (max === 0) return <div className="h-8 w-full rounded-sm border border-dashed" aria-hidden />;
  return (
    <div className="flex h-8 w-full items-end gap-px" aria-hidden>
      {values.map((v, i) => (
        <span
          key={points[i]?.start ?? i}
          className={cn("min-w-px flex-1 rounded-t-[2px]", v > 0 ? "bg-chart-2" : "bg-border")}
          style={{ height: v > 0 ? `${Math.max(8, (v / max) * 100)}%` : "1px" }}
          title={`${points[i] ? bucketLabel(points[i].start, unit) : ""}: ${formatBytes(v)} added`}
        />
      ))}
    </div>
  );
}

/**
 * RepositoryGrowthList shows the caller's restic repositories with what
 * their successful backups of the period added (per bucket, as small bars)
 * and how the size of what they back up changed.
 */
export function RepositoryGrowthList({
  repositories,
  points,
  unit,
}: {
  repositories: RepositoryGrowth[];
  points: ActivityPoint[];
  unit: BucketUnit;
}) {
  if (!repositories.length) {
    return (
      <Empty>
        No restic repositories yet.{" "}
        <Link to="/repositories/new" className="text-primary hover:underline">
          Add one
        </Link>{" "}
        to back up with restic.
      </Empty>
    );
  }
  return (
    <ul className="divide-y">
      {repositories.map((r) => {
        const backups = toNumber(r.backups);
        const first = toNumber(r.firstSourceBytes);
        const last = toNumber(r.lastSourceBytes);
        const latest = tsDate(r.latestSnapshotAt);
        return (
          <li key={r.repositoryId}>
            <Link
              to="/repositories/$repositoryId"
              params={{ repositoryId: r.repositoryId }}
              className="grid grid-cols-1 items-center gap-x-6 gap-y-2 px-5 py-3 hover:bg-accent/50 sm:grid-cols-[minmax(0,1fr)_minmax(0,1.25fr)_minmax(0,1.25fr)]"
            >
              <div className="min-w-0">
                <div className="truncate text-[13px] font-medium">{r.name}</div>
                <div className="truncate text-xs text-muted-foreground">
                  {formatCount(r.snapshotCount)} {r.snapshotCount === 1 ? "snapshot" : "snapshots"}
                  {latest && (
                    <>
                      {" · newest "}
                      <RelativeTime date={latest} />
                    </>
                  )}
                </div>
              </div>
              <div className="min-w-0">
                <div className="tabular truncate text-[13px] font-medium">
                  {backups
                    ? `${formatBytes(r.addedBytes)} added · ${formatCount(backups)} ${backups === 1 ? "backup" : "backups"}`
                    : "No backups in this period"}
                </div>
                <div className="tabular truncate text-xs text-muted-foreground">
                  {backups
                    ? `Source ${formatBytes(last)}${backups > 1 ? `, ${signedBytes(last - first)}` : ""}`
                    : "Nothing added"}
                  {r.truncated && " · newest backups only"}
                </div>
              </div>
              <AddedBars added={r.added} points={points} unit={unit} />
            </Link>
          </li>
        );
      })}
    </ul>
  );
}
