import { render, screen } from "@testing-library/react";
import type uPlot from "uplot";
import { describe, expect, it, vi } from "vitest";

import { formatRate } from "@/lib/format";

import { averageRate, chartData, phaseChanges } from "../charts/phases";
import { ProgressChart } from "../progress-chart";

// uPlot needs a canvas, which jsdom lacks; the mock keeps the options the
// chart was built with.
const built = vi.hoisted(() => [] as uPlot.Options[]);
vi.mock("uplot", () => ({
  default: class {
    constructor(opts: uPlot.Options) {
      built.push(opts);
    }
    setData() {}
    setSize() {}
    destroy() {}
  },
}));

describe("ProgressChart", () => {
  it("ticks whole seconds and whole bytes, so a short idle job repeats no label", () => {
    // A failed job that moved nothing within two seconds: before, the axes
    // split 0–1 B into fifths and one second into tenths, and printed
    // "1 B/s" and "22:00:07" under several ticks.
    render(
      <ProgressChart
        samples={[
          { t: 1000, bytes: 0, files: 0, errors: 0 },
          { t: 1000.4, bytes: 0, files: 0, errors: 0 },
          { t: 1001.2, bytes: 0, files: 0, errors: 1 },
        ]}
      />,
    );
    const axes = built.at(-1)?.axes ?? [];
    expect(axes).toHaveLength(3);
    for (const axis of axes) {
      const incrs = axis.incrs as number[];
      expect(incrs.length).toBeGreaterThan(0);
      expect(incrs.every((i) => Number.isInteger(i) && i >= 1)).toBe(true);
    }
  });

  // A mirror (rclone's counters), then the backup (restic's, from zero).
  const phased = [
    { t: 100, bytes: 0, files: 0, errors: 0, phase: "mirror" },
    { t: 110, bytes: 500, files: 1, errors: 0, phase: "mirror" },
    { t: 120, bytes: 900, files: 2, errors: 0, phase: "mirror" },
    { t: 130, bytes: 200, files: 1, errors: 0, phase: "backup" },
    { t: 140, bytes: 600, files: 2, errors: 0, phase: "backup" },
  ];
  const stored = phased.map(({ phase: _, ...s }) => s);

  it("finds a phase change by its name, its known time, or the counters restarting", () => {
    expect(phaseChanges(phased)).toEqual([{ index: 3, at: 125 }]);
    // Stored samples carry no phase: the job's result says when the
    // mirror ended, or else the drop of the bytes marks it.
    expect(phaseChanges(stored, [127])).toEqual([{ index: 3, at: 127, exact: true }]);
    expect(phaseChanges(stored)).toEqual([{ index: 3, at: 125 }]);
    // A known time wins over a drop, which then marks nothing more.
    const grows = stored.map((s, i) => (i >= 3 ? { ...s, bytes: s.bytes + 1000 } : s));
    expect(phaseChanges(grows)).toEqual([]);
    expect(phaseChanges(grows, [121])).toEqual([{ index: 3, at: 121, exact: true }]);
    // A change at a sample's own time stays just before it.
    expect(phaseChanges(stored, [130])[0].at).toBeLessThan(130);
    expect(phaseChanges(stored.slice(0, 3))).toEqual([]);
  });

  it("breaks both lines at a phase change and takes no rate across it", () => {
    const [t, rate, bytes] = chartData(phased, [{ index: 3, at: 125 }]);
    expect(t).toEqual([100, 110, 120, 125, 130, 140]);
    expect(bytes).toEqual([0, 500, 900, null, 200, 600]);
    expect(rate).toEqual([null, 50, 40, null, null, 40]);
    // A known change time: the first rate counts from it, from zero.
    const exact = chartData(phased, [{ index: 3, at: 125, exact: true }]);
    expect(exact[1]).toEqual([null, 50, 40, null, 40, 40]);
  });

  it("averages a finished job over its last phase", () => {
    // From the phase change, when the counters started again.
    expect(averageRate(stored, [{ index: 3, at: 120 }])).toBe(600 / 20);
    // One phase: from the job's start, or from the first sample.
    expect(averageRate(stored.slice(0, 3), [], 90)).toBe(900 / 30);
    expect(averageRate(stored.slice(0, 3), [])).toBe(900 / 20);
  });

  it("names the phases and shows a finished job's average", () => {
    render(
      <ProgressChart
        samples={stored}
        startedAt={95}
        phases={["Mirror", "Backup"]}
        phaseChangedAt={[120]}
      />,
    );
    expect(screen.getByText("Backup average")).toBeInTheDocument();
    expect(screen.getByText(formatRate(30))).toBeInTheDocument();
    expect(screen.getByText("Mirror → Backup: the counters restart")).toBeInTheDocument();
    const opts = built.at(-1);
    expect(opts?.hooks?.draw).toHaveLength(1);
  });

  it("keeps the live rate while a job runs", () => {
    render(<ProgressChart samples={stored.slice(0, 3)} live />);
    expect(screen.getByText("Throughput")).toBeInTheDocument();
    expect(screen.getByText(formatRate(40))).toBeInTheDocument();
    expect(screen.queryByText(/counters restart/)).toBeNull();
  });
});
