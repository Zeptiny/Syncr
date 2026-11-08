import { useMemo, useState } from "react";
import uPlot from "uplot";

import { formatBytes } from "@/lib/format";
import { useTheme } from "@/lib/theme";
import { cn } from "@/lib/utils";

import { bucketLabel, type ActivityPoint, type BucketUnit } from "./activity";
import { axisBase, bucketSplits, byteIncrs, cssVar, withAlpha } from "./uplot-theme";
import { useUPlot } from "./use-uplot";

const hourFmt = new Intl.DateTimeFormat("en-GB", { hour: "2-digit", minute: "2-digit" });
const dayFmt = new Intl.DateTimeFormat("en-GB", { day: "numeric", month: "short" });

/** BytesChartProps configures BytesChart. */
export interface BytesChartProps {
  points: ActivityPoint[];
  unit: BucketUnit;
  height?: number;
  className?: string;
}

/** BytesChart plots the bytes finished jobs transferred, per bucket, as an area. */
export function BytesChart({ points, unit, height = 150, className }: BytesChartProps) {
  const { resolved } = useTheme();
  const [hover, setHover] = useState<number | null>(null);
  const half = unit === "hour" ? 1800 : 43_200;
  const data = useMemo<uPlot.AlignedData>(
    () => [points.map((p) => p.start / 1000 + half), points.map((p) => p.bytes)],
    [points, half],
  );
  const total = points.reduce((a, p) => a + p.bytes, 0);
  const empty = total === 0;

  const box = useUPlot(
    (width) => {
      if (empty) return undefined;
      const color = cssVar("--chart-2");
      return {
        width,
        height,
        padding: [10, 6, 0, 4],
        legend: { show: false },
        cursor: { drag: { x: false, y: false }, points: { size: 6 }, y: false },
        scales: {
          x: { time: true, range: (_u, min, max) => [min - half, max + half] },
          y: { range: (_u, _min, max) => [0, Math.max(1, max) * 1.12] },
        },
        series: [
          {},
          {
            label: "Transferred",
            stroke: color,
            width: 1.75,
            fill: withAlpha(color, resolved === "dark" ? 0.24 : 0.14),
            points: { show: false },
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
            size: 68,
            space: 34,
            incrs: byteIncrs,
            values: (_u, vals) => vals.map((v) => formatBytes(v)),
          },
        ],
        hooks: { setCursor: [(u) => setHover(u.cursor.idx ?? null)] },
      };
    },
    data,
    [height, resolved, empty, unit, half],
  );

  const p = hover !== null ? points[hover] : undefined;
  if (empty) {
    return (
      <div
        className={cn(
          "grid h-24 place-items-center rounded-md border border-dashed text-[13px] text-muted-foreground",
          className,
        )}
      >
        Nothing was transferred in this period.
      </div>
    );
  }
  return (
    <div className={cn("flex flex-col gap-3", className)}>
      <div className="flex flex-wrap items-center gap-x-4 text-[13px]">
        <span className="flex items-center gap-1.5">
          <span className="size-2.5 rounded-sm bg-chart-2" aria-hidden />
          <span className="text-muted-foreground">Transferred</span>
          <span className="tabular font-medium">{formatBytes(p ? p.bytes : total)}</span>
        </span>
        <span className="tabular ml-auto text-xs text-muted-foreground">
          {p ? bucketLabel(p.start, unit) : "Whole period"}
        </span>
      </div>
      <div
        ref={box}
        className="w-full"
        style={{ height }}
        role="img"
        aria-label={`${formatBytes(total)} transferred`}
        onMouseLeave={() => setHover(null)}
      />
    </div>
  );
}
