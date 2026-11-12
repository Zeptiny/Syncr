import { describe, expect, it } from "vitest";

import { CronError, nextRuns, parseCron, previousRuns } from "./cron";

const iso = (ts: number[]) => ts.map((t) => new Date(t).toISOString());

describe("mock cron", () => {
  it("runs a spring-forward gap time at the transition", () => {
    const e = parseCron("30 2 * * *", "Europe/Berlin");
    expect(iso(nextRuns(e, Date.parse("2027-03-27T00:00:00Z"), 3))).toEqual([
      "2027-03-27T01:30:00.000Z",
      "2027-03-28T01:00:00.000Z",
      "2027-03-29T00:30:00.000Z",
    ]);
  });

  it("runs an ambiguous fall-back time once, at the earlier instant", () => {
    const e = parseCron("30 2 * * *", "Europe/Berlin");
    expect(iso(nextRuns(e, Date.parse("2026-10-24T12:00:00Z"), 2))).toEqual([
      "2026-10-25T00:30:00.000Z",
      "2026-10-26T01:30:00.000Z",
    ]);
  });

  it("applies the day-of-month or day-of-week rule", () => {
    const e = parseCron("0 0 1 * 1", "UTC");
    // 2026-06-01 is a Monday; the 8th and 15th are Mondays too.
    expect(iso(nextRuns(e, Date.parse("2026-05-31T12:00:00Z"), 3))).toEqual([
      "2026-06-01T00:00:00.000Z",
      "2026-06-08T00:00:00.000Z",
      "2026-06-15T00:00:00.000Z",
    ]);
  });

  it("lists earlier runs, newest first", () => {
    const before = Date.parse("2026-11-08T12:00:00Z");
    expect(iso(previousRuns("0 */6 * * *", "UTC", before, 2, 86_400_000))).toEqual([
      "2026-11-08T12:00:00.000Z",
      "2026-11-08T06:00:00.000Z",
    ]);
  });

  it("rejects bad input with the field at fault", () => {
    expect(() => parseCron("0 0 * *", "UTC")).toThrow(CronError);
    try {
      parseCron("0 0 * * *", "Mars/Olympus");
    } catch (e) {
      expect((e as CronError).field).toBe("timezone");
    }
    expect(nextRuns(parseCron("0 0 31 2 *", "UTC"), 0, 1)).toEqual([]);
  });
});
