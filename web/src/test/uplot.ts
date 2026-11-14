import type uPlot from "uplot";

/** builtCharts holds the options of every chart built so far in a test file, oldest first. */
export const builtCharts: uPlot.Options[] = [];

/**
 * UPlotStub stands in for uPlot in every test (vitest.setup.ts mocks the
 * module with it): uPlot draws on a canvas, which jsdom lacks. It keeps the
 * options each chart was built with (builtCharts) and draws nothing.
 */
export default class UPlotStub {
  static paths = { bars: () => () => null };

  constructor(opts: uPlot.Options) {
    builtCharts.push(opts);
  }

  setData(): void {}

  setSize(): void {}

  destroy(): void {}
}
