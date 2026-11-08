import "uplot/dist/uPlot.min.css";

import { useEffect, useMemo, useRef, useState } from "react";
import uPlot from "uplot";

import type { JobSample } from "@/hooks/use-watch-job";
import { formatBytes, formatDateTime, formatRate, rateBetween } from "@/lib/format";
import { useTheme } from "@/lib/theme";
import { cn } from "@/lib/utils";

/** chartData turns cumulative samples into [time, throughput, bytes]. */
function chartData(samples: JobSample[]): [number[], (number | null)[], number[]] {
  const t: number[] = [];
  const rate: (number | null)[] = [];
  const bytes: number[] = [];
  samples.forEach((s, i) => {
    t.push(s.t);
    rate.push(i === 0 ? null : rateBetween(samples[i - 1], s));
    bytes.push(s.bytes);
  });
  return [t, rate, bytes];
}

const hm = new Intl.DateTimeFormat("en-GB", { hour: "2-digit", minute: "2-digit" });
const hms = new Intl.DateTimeFormat("en-GB", {
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
});

function cssVar(name: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim() || "#888";
}

function withAlpha(color: string, alpha: number): string {
  // Theme colours are hex; uPlot fills need rgba.
  const m = /^#([0-9a-f]{6})$/i.exec(color);
  if (!m) return color;
  const n = parseInt(m[1], 16);
  return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`;
}

/** ProgressChartProps configures ProgressChart. */
export interface ProgressChartProps {
  samples: JobSample[];
  height?: number;
  /** live marks a running job; the empty state then says it's waiting. */
  live?: boolean;
  className?: string;
}

/**
 * ProgressChart plots throughput (area, left axis) and cumulative bytes
 * (line, right axis). New samples are appended in place; the chart follows
 * the theme.
 */
export function ProgressChart({ samples, height = 260, live, className }: ProgressChartProps) {
  const box = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const { resolved } = useTheme();
  const data = useMemo(() => chartData(samples), [samples]);
  const dataRef = useRef(data);
  const [hover, setHover] = useState<number | null>(null);

  useEffect(() => {
    dataRef.current = data;
    plot.current?.setData(data);
  }, [data]);

  const empty = samples.length < 2;

  useEffect(() => {
    const el = box.current;
    if (!el || empty) return;
    const liveColor = cssVar("--chart-1");
    const bytesColor = cssVar("--chart-2");
    const grid = cssVar("--chart-grid");
    const axis = cssVar("--chart-axis");
    const font = `12px ${getComputedStyle(document.body).fontFamily}`;
    const axisBase = {
      stroke: axis,
      font,
      grid: { stroke: grid, width: 1 },
      ticks: { show: false },
    };
    const opts: uPlot.Options = {
      width: el.clientWidth || 640,
      height,
      padding: [12, 8, 0, 8],
      legend: { show: false },
      cursor: { drag: { x: false, y: false }, points: { size: 7 } },
      scales: {
        x: { time: true },
        rate: { range: (_u, _min, max) => [0, Math.max(max, 1) * 1.15] },
        bytes: { range: (_u, _min, max) => [0, Math.max(max, 1) * 1.05] },
      },
      series: [
        {},
        {
          label: "Throughput",
          scale: "rate",
          stroke: liveColor,
          width: 2,
          fill: withAlpha(liveColor, resolved === "dark" ? 0.22 : 0.14),
          points: { show: false },
          spanGaps: true,
        },
        {
          label: "Transferred",
          scale: "bytes",
          stroke: bytesColor,
          width: 1.75,
          dash: [5, 4],
          points: { show: false },
        },
      ],
      axes: [
        {
          ...axisBase,
          grid: { show: false },
          size: 30,
          space: 70,
          values: (_u, splits, _axis, _space, incr) =>
            splits.map((s) => (incr < 60 ? hms : hm).format(new Date(s * 1000))),
        },
        {
          ...axisBase,
          scale: "rate",
          size: 76,
          values: (_u, vals) => vals.map((v) => formatRate(v)),
        },
        {
          ...axisBase,
          scale: "bytes",
          side: 1,
          size: 72,
          grid: { show: false },
          values: (_u, vals) => vals.map((v) => formatBytes(v)),
        },
      ],
      hooks: {
        setCursor: [(u) => setHover(u.cursor.idx ?? null)],
      },
    };
    const u = new uPlot(opts, dataRef.current, el);
    plot.current = u;
    const ro = new ResizeObserver(() => {
      if (el.clientWidth > 0) u.setSize({ width: el.clientWidth, height });
    });
    ro.observe(el);
    return () => {
      ro.disconnect();
      u.destroy();
      plot.current = null;
    };
  }, [height, resolved, empty]);

  const idx = hover ?? data[0].length - 1;
  const rate = idx >= 0 ? (data[1][idx] ?? 0) : 0;
  const bytes = idx >= 0 ? (data[2][idx] ?? 0) : 0;
  const at = idx >= 0 && data[0][idx] ? new Date(data[0][idx] * 1000) : undefined;
  if (empty) {
    return (
      <div
        className={cn(
          "grid h-28 place-items-center rounded-md border border-dashed text-[13px] text-muted-foreground",
          className,
        )}
      >
        {live ? "Waiting for progress from the worker…" : "No progress samples were recorded."}
      </div>
    );
  }

  return (
    <div className={cn("flex flex-col gap-3", className)}>
      <div className="flex flex-wrap items-center gap-x-5 gap-y-1 text-[13px]">
        <span className="flex items-center gap-2">
          <span className="h-2.5 w-2.5 rounded-sm bg-chart-1" aria-hidden />
          <span className="text-muted-foreground">Throughput</span>
          <span className="tabular font-medium">{formatRate(rate)}</span>
        </span>
        <span className="flex items-center gap-2">
          <span className="h-0 w-3 border-t-2 border-dashed border-chart-2" aria-hidden />
          <span className="text-muted-foreground">Transferred</span>
          <span className="tabular font-medium">{formatBytes(bytes)}</span>
        </span>
        {at && hover !== null && (
          <span className="tabular ml-auto text-xs text-muted-foreground">
            {formatDateTime(at)}
          </span>
        )}
      </div>
      <div className="relative">
        <div
          ref={box}
          className="w-full"
          style={{ height }}
          role="img"
          aria-label="Throughput and bytes transferred over time"
        />
      </div>
    </div>
  );
}
