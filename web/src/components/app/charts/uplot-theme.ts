import type uPlot from "uplot";

/** cssVar reads a theme token from the document (uPlot draws on a canvas). */
export function cssVar(name: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim() || "#888";
}

/** withAlpha turns a hex theme colour into rgba, for uPlot fills. */
export function withAlpha(color: string, alpha: number): string {
  const m = /^#([0-9a-f]{6})$/i.exec(color);
  if (!m) return color;
  const n = parseInt(m[1], 16);
  return `rgba(${(n >> 16) & 255}, ${(n >> 8) & 255}, ${n & 255}, ${alpha})`;
}

/** axisBase is the shared axis look: theme colours, no ticks, light grid. */
export function axisBase(): Partial<uPlot.Axis> {
  return {
    stroke: cssVar("--chart-axis"),
    font: `12px ${getComputedStyle(document.body).fontFamily}`,
    grid: { stroke: cssVar("--chart-grid"), width: 1 },
    ticks: { show: false },
  };
}

/**
 * bucketSplits puts x-axis labels on bucket centres, keeping every k-th one
 * so labels at least minSpace pixels apart fit in the plot width.
 */
export function bucketSplits(xs: number[], width: number, minSpace: number): number[] {
  if (!xs.length) return [];
  const fit = Math.max(1, Math.floor(width / minSpace));
  const k = Math.max(1, Math.ceil(xs.length / fit));
  // Count from the newest bucket, so the latest one is always labelled.
  return xs.filter((_, i) => (xs.length - 1 - i) % k === 0);
}

/** byteIncrs are tick steps for byte axes: 1, 2 and 5 times powers of 1024. */
export const byteIncrs: number[] = Array.from({ length: 6 }, (_, p) =>
  [1, 2, 5, 10, 20, 50, 100, 200, 500].map((m) => m * 1024 ** p),
).flat();

/**
 * secondIncrs are tick steps for time axes in whole seconds, so a short job
 * does not repeat the same second under several ticks.
 */
export const secondIncrs: number[] = [
  1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 10800, 21600, 43200, 86400, 172800,
  604800,
];
