import "uplot/dist/uPlot.min.css";

import { useEffect, useMemo, useRef, useState } from "react";
import uPlot from "uplot";

import type { JobSample } from "@/hooks/use-watch-job";
import { formatBytes, formatDateTime, formatRate } from "@/lib/format";
import { useTheme } from "@/lib/theme";
import { cn } from "@/lib/utils";

import { averageRate, chartData, phaseChanges } from "./charts/phases";
import { byteIncrs, cssVar, secondIncrs, withAlpha } from "./charts/uplot-theme";

const hm = new Intl.DateTimeFormat("en-GB", { hour: "2-digit", minute: "2-digit" });
const hms = new Intl.DateTimeFormat("en-GB", {
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
});

/** ProgressChartProps configures ProgressChart. */
export interface ProgressChartProps {
  samples: JobSample[];
  height?: number;
  /** live marks a running job; the empty state then says it's waiting. */
  live?: boolean;
  /** empty replaces the text shown when there is nothing to plot. */
  empty?: React.ReactNode;
  /** note is shown under the chart, e.g. that the history is thinned. */
  note?: React.ReactNode;
  /** startedAt is the job's start in seconds, for a finished job's average. */
  startedAt?: number;
  /**
   * phases names the phases of a job that has several, in order (e.g.
   * "Mirror", "Backup"), for the legend; samples that name their phase
   * need none.
   */
  phases?: string[];
  /** phaseChangedAt holds when later phases began (seconds), if known. */
  phaseChangedAt?: number[];
  className?: string;
}

/** phaseName names a sample's phase for the legend. */
function phaseName(phase: string | undefined, phases: string[] | undefined, index: number) {
  if (phase === "mirror") return "Mirror";
  if (phase === "backup") return "Backup";
  return phases?.[index];
}

/**
 * ProgressChart plots throughput (area, left axis) and cumulative bytes
 * (line, right axis). New samples are appended in place; the chart follows
 * the theme. A phase change (counters restart) breaks both lines at a
 * dashed marker. A finished job's legend shows the average of its last
 * phase until a point is hovered.
 */
export function ProgressChart({
  samples,
  height = 260,
  live,
  empty: emptyText,
  note,
  startedAt,
  phases,
  phaseChangedAt,
  className,
}: ProgressChartProps) {
  const box = useRef<HTMLDivElement>(null);
  const plot = useRef<uPlot | null>(null);
  const { resolved } = useTheme();
  const knownKey = (phaseChangedAt ?? []).join(",");
  const changes = useMemo(
    () => phaseChanges(samples, phaseChangedAt),
    // phaseChangedAt is compared by value.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [samples, knownKey],
  );
  const data = useMemo(() => chartData(samples, changes), [samples, changes]);
  const dataRef = useRef(data);
  // The marker positions (the empty points), read by the draw hook.
  const marks = useMemo(() => changes.map((c) => c.at), [changes]);
  const marksRef = useRef(marks);
  const [hover, setHover] = useState<number | null>(null);

  useEffect(() => {
    dataRef.current = data;
    marksRef.current = marks;
    plot.current?.setData(data);
  }, [data, marks]);

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
          incrs: secondIncrs,
          values: (_u, splits, _axis, _space, incr) =>
            splits.map((s) => (incr < 60 ? hms : hm).format(new Date(s * 1000))),
        },
        {
          ...axisBase,
          scale: "rate",
          size: 76,
          incrs: byteIncrs,
          values: (_u, vals) => vals.map((v) => formatRate(v)),
        },
        {
          ...axisBase,
          scale: "bytes",
          side: 1,
          size: 72,
          grid: { show: false },
          incrs: byteIncrs,
          values: (_u, vals) => vals.map((v) => formatBytes(v)),
        },
      ],
      hooks: {
        setCursor: [(u) => setHover(u.cursor.idx ?? null)],
        // A dashed line where a phase change restarted the counters.
        draw: [
          (u) => {
            const ctx = u.ctx as CanvasRenderingContext2D | null;
            if (!ctx || marksRef.current.length === 0) return;
            ctx.save();
            ctx.strokeStyle = axis;
            ctx.lineWidth = devicePixelRatio;
            ctx.setLineDash([4 * devicePixelRatio, 4 * devicePixelRatio]);
            for (const m of marksRef.current) {
              const x = Math.round(u.valToPos(m, "x", true));
              ctx.beginPath();
              ctx.moveTo(x, u.bbox.top);
              ctx.lineTo(x, u.bbox.top + u.bbox.height);
              ctx.stroke();
            }
            ctx.restore();
          },
        ],
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
  // A finished job, nothing hovered: the last phase's average, not the
  // rate of its last few seconds.
  const average = !live && hover === null;
  const rate = average
    ? averageRate(samples, changes, startedAt)
    : idx >= 0
      ? (data[1][idx] ?? 0)
      : 0;
  const bytes = idx >= 0 ? (data[2][idx] ?? 0) : 0;
  const at = idx >= 0 && data[0][idx] ? new Date(data[0][idx] * 1000) : undefined;
  // The phase of the point shown: each marker before it is one phase
  // change and one extra point in the data.
  const phased = changes.length > 0;
  const shownAt = data[0][idx] ?? 0;
  const shownPhase = phased
    ? phaseName(
        samples[idx - marks.filter((m) => m <= shownAt).length]?.phase,
        phases,
        marks.filter((m) => m < shownAt).length,
      )
    : undefined;
  if (empty) {
    return (
      <div
        className={cn(
          "grid h-28 place-items-center rounded-md border border-dashed text-[13px] text-muted-foreground",
          className,
        )}
      >
        {emptyText ??
          (live ? "Waiting for progress from the worker…" : "No progress samples were recorded.")}
      </div>
    );
  }

  return (
    <div className={cn("flex flex-col gap-3", className)}>
      <div className="flex flex-wrap items-center gap-x-5 gap-y-1 text-[13px]">
        <span className="flex items-center gap-2">
          <span className="h-2.5 w-2.5 rounded-sm bg-chart-1" aria-hidden />
          <span className="text-muted-foreground">
            {average ? (shownPhase ? `${shownPhase} average` : "Average") : "Throughput"}
          </span>
          <span className="tabular font-medium">{formatRate(rate)}</span>
        </span>
        <span className="flex items-center gap-2">
          <span className="h-0 w-3 border-t-2 border-dashed border-chart-2" aria-hidden />
          <span className="text-muted-foreground">Transferred</span>
          <span className="tabular font-medium">{formatBytes(bytes)}</span>
        </span>
        {phased && (
          <span className="flex items-center gap-2">
            <span className="h-3 w-0 border-l border-dashed border-muted-foreground" aria-hidden />
            <span className="text-muted-foreground">
              {phases && phases.length > 1
                ? `${phases.join(" → ")}: the counters restart`
                : "Phase change: the counters restart"}
            </span>
          </span>
        )}
        {at && hover !== null && (
          <span className="tabular ml-auto text-xs text-muted-foreground">
            {shownPhase ? `${shownPhase} · ` : ""}
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
      {note && <p className="text-xs text-muted-foreground">{note}</p>}
    </div>
  );
}
