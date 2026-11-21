import { useMemo, useState } from "react";
import uPlot from "uplot";

import { formatDuration } from "@/lib/format";
import { useTheme } from "@/lib/theme";
import { cn } from "@/lib/utils";

import {
  averages,
  bucketLabel,
  overall,
  type ActivityPoint,
  type BucketUnit,
  durationCount,
  type DurationMetric,
} from "./activity";
import { axisBase, bucketSplits, cssVar, secondIncrs, withAlpha } from "./uplot-theme";
import { useUPlot } from "./use-uplot";

const hourFmt = new Intl.DateTimeFormat("en-GB", { hour: "2-digit", minute: "2-digit" });
const dayFmt = new Intl.DateTimeFormat("en-GB", { day: "numeric", month: "short" });

const metrics: Record<DurationMetric, { label: string; empty: string; noun: string }> = {
  runTime: {
    label: "Average run time",
    empty: "No job ran in this period.",
    noun: "jobs that ran",
  },
  queueWait: {
    label: "Average queue wait",
    empty: "No job was picked up in this period.",
    noun: "jobs picked up",
  },
};

/** DurationChartProps configures DurationChart. */
export interface DurationChartProps {
  points: ActivityPoint[];
  unit: BucketUnit;
  metric: DurationMetric;
  height?: number;
  className?: string;
}

/**
 * DurationChart plots one duration per bucket, the average run time or
 * queue wait of the jobs that finished in it, as a line with a light area.
 * Buckets without such jobs leave a gap. One series, so the header names it
 * and shows the hovered bucket's value (or the period's average).
 */
export function DurationChart({
  points,
  unit,
  metric,
  height = 120,
  className,
}: DurationChartProps) {
  const { resolved } = useTheme();
  const [hover, setHover] = useState<number | null>(null);
  const m = metrics[metric];
  const half = unit === "hour" ? 1800 : 43_200;
  const avgs = useMemo(() => averages(points, metric), [points, metric]);
  const data = useMemo<uPlot.AlignedData>(
    () => [points.map((p) => p.start / 1000 + half), avgs],
    [points, avgs, half],
  );
  const whole = overall(points, metric);
  const empty = whole.n === 0;

  const box = useUPlot(
    (width) => {
      if (empty) return undefined;
      const color = cssVar("--chart-2");
      return {
        width,
        height,
        padding: [10, 6, 0, 4],
        legend: { show: false },
        cursor: { drag: { x: false, y: false }, points: { size: 8 }, y: false },
        scales: {
          x: { time: true, range: (_u, min, max) => [min - half, max + half] },
          y: { range: (_u, _min, max) => [0, Math.max(1, max) * 1.15] },
        },
        series: [
          {},
          {
            label: m.label,
            stroke: color,
            width: 2,
            fill: withAlpha(color, resolved === "dark" ? 0.2 : 0.12),
            spanGaps: false,
            points: {
              show: (u) => (u.data[1] as (number | null)[]).filter((v) => v !== null).length < 3,
              size: 8,
            },
          },
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
            size: 60,
            space: 30,
            incrs: secondIncrs,
            values: (_u, vals) => vals.map((v) => formatDuration(v)),
          },
        ],
        hooks: { setCursor: [(u) => setHover(u.cursor.idx ?? null)] },
      };
    },
    data,
    [height, resolved, empty, unit, half, metric],
  );

  const p = hover !== null ? points[hover] : undefined;
  const hovered = hover !== null ? avgs[hover] : undefined;
  if (empty) {
    return (
      <div className={cn("flex flex-col gap-3", className)}>
        <div className="text-[13px] text-muted-foreground">{m.label}</div>
        <div className="grid h-20 place-items-center rounded-md border border-dashed text-[13px] text-muted-foreground">
          {m.empty}
        </div>
      </div>
    );
  }
  return (
    <div className={cn("flex flex-col gap-3", className)}>
      <div className="flex flex-col gap-0.5">
        <div className="flex items-center gap-1.5 text-[13px]">
          <span className="text-muted-foreground">{m.label}</span>
          <span className="tabular font-medium">
            {p ? (hovered == null ? "–" : formatDuration(hovered)) : formatDuration(whole.avg)}
          </span>
        </div>
        <div className="tabular truncate text-xs text-muted-foreground">
          {p
            ? `${bucketLabel(p.start, unit)} · ${p[durationCount[metric]]} ${m.noun}`
            : `Whole period · ${whole.n} ${m.noun}`}
        </div>
      </div>
      <div
        ref={box}
        className="w-full"
        style={{ height }}
        role="img"
        aria-label={`${m.label}: ${formatDuration(whole.avg)} over ${whole.n} ${m.noun}`}
        onMouseLeave={() => setHover(null)}
      />
    </div>
  );
}
