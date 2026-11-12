import { useMemo, useState } from "react";
import uPlot from "uplot";

import { useTheme } from "@/lib/theme";
import { cn } from "@/lib/utils";

import {
  activityStatuses,
  bucketLabel,
  stack,
  type ActivityPoint,
  type ActivityStatus,
  type BucketUnit,
} from "./activity";
import { axisBase, bucketSplits, cssVar, withAlpha } from "./uplot-theme";
import { useUPlot } from "./use-uplot";

const statusLabel: Record<ActivityStatus, string> = {
  succeeded: "Succeeded",
  failed: "Failed",
  interrupted: "Interrupted",
  cancelled: "Cancelled",
};

const statusToken: Record<ActivityStatus, string> = {
  succeeded: "--success",
  failed: "--danger",
  interrupted: "--warning",
  cancelled: "--neutral",
};

const swatch: Record<ActivityStatus, string> = {
  succeeded: "bg-success",
  failed: "bg-danger",
  interrupted: "bg-warning",
  cancelled: "bg-neutral/50",
};

const hourFmt = new Intl.DateTimeFormat("en-GB", { hour: "2-digit", minute: "2-digit" });
const dayFmt = new Intl.DateTimeFormat("en-GB", { day: "numeric", month: "short" });
/** bucketSeconds is the nominal bucket length (bars are centred in it). */
function bucketSeconds(unit: BucketUnit): number {
  return unit === "hour" ? 3600 : 86_400;
}

/** ActivityChartProps configures ActivityChart. */
export interface ActivityChartProps {
  points: ActivityPoint[];
  unit: BucketUnit;
  height?: number;
  className?: string;
}

/**
 * ActivityChart shows finished jobs per bucket as bars stacked by outcome,
 * coloured with the theme's status tokens. Hovering a bar shows its counts.
 */
export function ActivityChart({ points, unit, height = 220, className }: ActivityChartProps) {
  const { resolved } = useTheme();
  const [hover, setHover] = useState<number | null>(null);
  const data = useMemo<uPlot.AlignedData>(() => {
    const half = bucketSeconds(unit) / 2;
    const xs = points.map((p) => p.start / 1000 + half);
    // Tallest first: later series paint over earlier ones.
    const cumulative = stack(points).reverse();
    return [xs, ...cumulative];
  }, [points, unit]);
  const total = points.reduce(
    (acc, p) => acc + p.succeeded + p.failed + p.interrupted + p.cancelled,
    0,
  );
  const empty = total === 0;

  const box = useUPlot(
    (width) => {
      if (empty) return undefined;
      const bars = uPlot.paths.bars?.({ size: [0.72, 26], gap: 1 });
      const order = [...activityStatuses].reverse();
      return {
        width,
        height,
        padding: [10, 6, 0, 4],
        legend: { show: false },
        cursor: {
          drag: { x: false, y: false },
          points: { show: false },
          x: false,
          y: false,
        },
        focus: { alpha: 1 },
        scales: {
          x: {
            time: true,
            range: (_u, min, max) => [min - bucketSeconds(unit) / 2, max + bucketSeconds(unit) / 2],
          },
          y: { range: (_u, _min, max) => [0, Math.max(4, Math.ceil(max * 1.1))] },
        },
        series: [
          {},
          ...order.map((s) => {
            const color = cssVar(statusToken[s]);
            return {
              label: statusLabel[s],
              stroke: color,
              fill: s === "cancelled" ? withAlpha(color, resolved === "dark" ? 0.45 : 0.35) : color,
              width: 0,
              paths: bars,
              points: { show: false },
            } satisfies uPlot.Series;
          }),
        ],
        axes: [
          {
            ...axisBase(),
            grid: { show: false },
            size: 28,
            splits: (u) =>
              bucketSplits(
                u.data[0] as number[],
                u.bbox.width / devicePixelRatio,
                unit === "hour" ? 52 : 48,
              ),
            values: (_u, splits) =>
              splits.map((s) => (unit === "hour" ? hourFmt : dayFmt).format(new Date(s * 1000))),
          },
          {
            ...axisBase(),
            size: 36,
            space: 30,
            incrs: [1, 2, 5, 10, 20, 50, 100, 200, 500, 1000],
            values: (_u, vals) => vals.map((v) => String(v)),
          },
        ],
        hooks: { setCursor: [(u) => setHover(u.cursor.idx ?? null)] },
      };
    },
    data,
    [height, resolved, empty, unit],
  );

  const p = hover !== null ? points[hover] : undefined;
  const counts: Record<ActivityStatus, number> = p
    ? {
        succeeded: p.succeeded,
        failed: p.failed,
        interrupted: p.interrupted,
        cancelled: p.cancelled,
      }
    : {
        succeeded: points.reduce((a, x) => a + x.succeeded, 0),
        failed: points.reduce((a, x) => a + x.failed, 0),
        interrupted: points.reduce((a, x) => a + x.interrupted, 0),
        cancelled: points.reduce((a, x) => a + x.cancelled, 0),
      };

  if (empty) {
    return (
      <div
        className={cn(
          "grid place-items-center rounded-md border border-dashed text-[13px] text-muted-foreground",
          className,
        )}
        style={{ height: Math.min(height, 160) }}
      >
        No jobs finished in this period.
      </div>
    );
  }

  return (
    <div className={cn("flex flex-col gap-3", className)}>
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-[13px]">
        {activityStatuses.map((s) => (
          <span key={s} className="flex items-center gap-1.5">
            <span className={cn("size-2.5 rounded-sm", swatch[s])} aria-hidden />
            <span className="text-muted-foreground">{statusLabel[s]}</span>
            <span className="tabular font-medium">{counts[s]}</span>
          </span>
        ))}
        <span className="tabular ml-auto text-xs text-muted-foreground">
          {p ? bucketLabel(p.start, unit) : "Whole period"}
        </span>
      </div>
      <div
        ref={box}
        className="w-full"
        style={{ height }}
        role="img"
        aria-label={`Finished jobs per ${unit}: ${counts.succeeded} succeeded, ${counts.failed} failed, ${counts.interrupted} interrupted, ${counts.cancelled} cancelled`}
        onMouseLeave={() => setHover(null)}
      />
    </div>
  );
}
