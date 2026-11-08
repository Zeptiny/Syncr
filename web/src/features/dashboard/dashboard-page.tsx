import { useQuery } from "@connectrpc/connect-query";
import { useNavigate } from "@tanstack/react-router";
import { useMemo } from "react";

import { shapeActivity, type BucketUnit } from "@/components/app/charts/activity";
import { ActivityChart } from "@/components/app/charts/activity-chart";
import { BytesChart } from "@/components/app/charts/bytes-chart";
import { ErrorState } from "@/components/app/error-state";
import { PageHeader } from "@/components/app/page-header";
import { Panel } from "@/components/app/panel";
import { StatCard, StatGrid } from "@/components/app/stat-card";
import { Label } from "@/components/ui/label";
import { Skeleton } from "@/components/ui/skeleton";
import { Switch } from "@/components/ui/switch";
import { DashboardRange, DashboardService } from "@/gen/syncr/v1/dashboard_pb";
import { ScheduleService } from "@/gen/syncr/v1/schedules_pb";
import { useSession } from "@/hooks/use-session";
import { formatBytes, formatCount, toNumber, tsDate } from "@/lib/format";
import { browserTimeZone } from "@/lib/timezones";
import { cn } from "@/lib/utils";

import { RecentFailures, UpcomingRuns, WorkerHealthPanel } from "./panels";
import type { DashboardRangeKey, DashboardSearch } from "./search";

const ranges: { key: DashboardRangeKey; label: string; range: DashboardRange; noun: string }[] = [
  { key: "24h", label: "24 hours", range: DashboardRange.DAY, noun: "the last 24 hours" },
  { key: "7d", label: "7 days", range: DashboardRange.WEEK, noun: "the last 7 days" },
  { key: "30d", label: "30 days", range: DashboardRange.MONTH, noun: "the last 30 days" },
];

function RangeToggle({
  value,
  onChange,
}: {
  value: DashboardRangeKey;
  onChange: (v: DashboardRangeKey) => void;
}) {
  return (
    <div
      role="radiogroup"
      aria-label="Period"
      className="inline-flex rounded-lg border bg-card p-0.5"
    >
      {ranges.map((r) => (
        <button
          key={r.key}
          type="button"
          role="radio"
          aria-checked={value === r.key}
          onClick={() => onChange(r.key)}
          className={cn(
            "rounded-md px-3 py-1 text-[13px] font-medium transition-colors",
            value === r.key
              ? "bg-primary text-primary-foreground"
              : "text-muted-foreground hover:text-foreground",
          )}
        >
          {r.label}
        </button>
      ))}
    </div>
  );
}

/** formatRate renders a success rate in [0, 1] as "96.4%". */
export function formatSuccessRate(rate: number, finished: number): string {
  if (!finished) return "–";
  const pct = rate * 100;
  return `${pct >= 99.95 || pct === 0 ? pct.toFixed(0) : pct.toFixed(1)}%`;
}

/**
 * DashboardPage is the landing page: how jobs went over a period, what runs
 * next, which workers are up and what needs attention.
 */
export function DashboardPage({ search }: { search: DashboardSearch }) {
  const navigate = useNavigate();
  const { isAdmin } = useSession();
  const key = search.range ?? "24h";
  const range = ranges.find((r) => r.key === key) ?? ranges[0];
  const allUsers = isAdmin && !!search.all;
  const timezone = useMemo(() => browserTimeZone(), []);
  const q = useQuery(
    DashboardService.method.getDashboard,
    { range: range.range, timezone, allUsers },
    { placeholderData: (prev) => prev, refetchInterval: 30_000 },
  );
  const schedules = useQuery(ScheduleService.method.listSchedules, {});
  const scheduleMap = useMemo(
    () => new Map((schedules.data?.schedules ?? []).map((s) => [s.id, s])),
    [schedules.data],
  );
  const setSearch = (patch: Partial<DashboardSearch>) =>
    void navigate({ to: "/", search: (prev) => ({ ...prev, ...patch }), replace: true });

  const d = q.data;
  const unit: BucketUnit = d?.bucketUnit === "day" ? "day" : "hour";
  const points = useMemo(
    () =>
      d ? shapeActivity(d.activity, { from: tsDate(d.from), until: tsDate(d.until), unit }) : [],
    [d, unit],
  );
  const t = d?.totals;
  const succeeded = toNumber(t?.succeeded);
  const failed = toNumber(t?.failed);
  const interrupted = toNumber(t?.interrupted);
  const cancelled = toNumber(t?.cancelled);
  const finished = succeeded + failed + interrupted;
  const running = toNumber(t?.running);
  const queued = toNumber(t?.queued);
  const missed = toNumber(t?.missedRuns);
  const rate = t?.successRate ?? 0;

  return (
    <>
      <PageHeader
        title="Dashboard"
        description={`How your jobs went in ${range.noun}, and what runs next.`}
        actions={
          <>
            {isAdmin && (
              <Label className="mr-1 flex items-center gap-2 text-[13px] font-normal">
                <Switch
                  checked={!!search.all}
                  onCheckedChange={(c) => setSearch({ all: c || undefined })}
                />
                All users
              </Label>
            )}
            <RangeToggle
              value={key}
              onChange={(v) => setSearch({ range: v === "24h" ? undefined : v })}
            />
          </>
        }
      />
      {q.error && !d ? (
        <Panel>
          <ErrorState error={q.error} onRetry={() => void q.refetch()} />
        </Panel>
      ) : (
        <div className="flex flex-col gap-6">
          {!d ? (
            <Skeleton className="h-[86px] w-full rounded-lg" />
          ) : (
            <StatGrid>
              <StatCard
                label="Success rate"
                value={formatSuccessRate(rate, finished)}
                tone={finished && rate < 0.9 ? "danger" : "default"}
                sub={
                  finished
                    ? `${formatCount(succeeded)} of ${formatCount(finished)} finished jobs`
                    : "No finished jobs"
                }
              />
              <StatCard
                label="Jobs run"
                value={formatCount(finished + cancelled)}
                sub={
                  failed + interrupted
                    ? `${failed} failed, ${interrupted} interrupted`
                    : cancelled
                      ? `${cancelled} cancelled`
                      : "None failed"
                }
              />
              <StatCard
                label="Transferred"
                value={formatBytes(t?.bytes)}
                sub={`${formatCount(t?.files)} files`}
              />
              <StatCard
                label="Running now"
                value={formatCount(running)}
                tone={running ? "live" : "default"}
                sub={queued ? `${queued} queued` : "Nothing queued"}
              />
              <StatCard
                label="Missed runs"
                value={formatCount(missed)}
                tone={missed ? "warning" : "default"}
                sub={missed ? "Worker offline or overlap" : "None missed"}
              />
            </StatGrid>
          )}

          <div className="grid grid-cols-1 gap-6 xl:grid-cols-[minmax(0,2fr)_minmax(0,1fr)]">
            <Panel
              title="Activity"
              description={`Finished jobs per ${unit === "hour" ? "hour" : "day"}, by outcome.`}
            >
              {!d ? (
                <Skeleton className="h-[260px] w-full" />
              ) : (
                <div className="flex flex-col gap-6">
                  <ActivityChart points={points} unit={unit} />
                  <div className="border-t pt-5">
                    <BytesChart points={points} unit={unit} />
                  </div>
                </div>
              )}
            </Panel>
            <div className="flex flex-col gap-6">
              <Panel title="Up next" flush>
                {!d ? (
                  <Skeleton className="m-5 h-40" />
                ) : (
                  <UpcomingRuns runs={d.upcoming} schedules={scheduleMap} />
                )}
              </Panel>
              <Panel title="Workers" flush>
                {!d ? <Skeleton className="m-5 h-32" /> : <WorkerHealthPanel health={d.workers} />}
              </Panel>
            </div>
          </div>

          <Panel
            title="Needs attention"
            description="The latest failed and interrupted jobs, and runs that were missed or could not start."
            flush
          >
            {!d ? (
              <Skeleton className="m-5 h-32" />
            ) : (
              <RecentFailures
                jobs={d.recentFailures}
                runs={d.recentMissed}
                schedules={scheduleMap}
              />
            )}
          </Panel>
        </div>
      )}
    </>
  );
}
