import { render } from "@testing-library/react";
import type uPlot from "uplot";
import { describe, expect, it, vi } from "vitest";

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
});
