import { describe, expect, it } from "vitest";

import { floorDay, floorHour, localMidnight, nextDay, utcOffset } from "./zone";

const utc = (iso: string) => Date.parse(iso);
const iso = (ms: number) => new Date(ms).toISOString().slice(0, 16) + "Z";
const HOUR = 3_600_000;

describe("zone arithmetic", () => {
  it("reads the UTC offset of a zone at an instant", () => {
    expect(utcOffset(utc("2026-07-01T12:00:00Z"), "America/New_York")).toBe(-4 * HOUR);
    expect(utcOffset(utc("2026-01-01T12:00:00Z"), "America/New_York")).toBe(-5 * HOUR);
    expect(utcOffset(utc("2026-01-01T12:00:00Z"), "Asia/Kolkata")).toBe(5.5 * HOUR);
    expect(utcOffset(utc("2026-01-01T12:00:00Z"), "UTC")).toBe(0);
  });

  it("floors hours keeping the offset, also in a repeated hour", () => {
    // Both 01:30 EDT (05:30Z) and 01:30 EST (06:30Z) floor to their own 01:00.
    expect(iso(floorHour(utc("2026-11-01T05:30:10Z"), "America/New_York"))).toBe(
      "2026-11-01T05:00Z",
    );
    expect(iso(floorHour(utc("2026-11-01T06:30:10Z"), "America/New_York"))).toBe(
      "2026-11-01T06:00Z",
    );
    expect(iso(floorHour(utc("2026-06-01T06:10:00Z"), "Asia/Kolkata"))).toBe("2026-06-01T05:30Z");
  });

  it("floors days to local midnight, 23 to 25 hours apart across DST", () => {
    expect(iso(floorDay(utc("2026-11-01T20:00:00Z"), "America/New_York"))).toBe(
      "2026-11-01T04:00Z",
    );
    expect(iso(nextDay(utc("2026-11-01T04:00:00Z"), "America/New_York"))).toBe("2026-11-02T05:00Z");
    expect(iso(nextDay(utc("2026-03-08T05:00:00Z"), "America/New_York"))).toBe("2026-03-09T04:00Z");
  });

  it("resolves a midnight that does not exist to the transition instant", () => {
    // America/Sao_Paulo skipped 2018-11-04 00:00 (it became 01:00, -03 to -02).
    expect(iso(localMidnight(2018, 11, 4, "America/Sao_Paulo"))).toBe("2018-11-04T03:00Z");
    expect(iso(floorDay(utc("2018-11-04T15:00:00Z"), "America/Sao_Paulo"))).toBe(
      "2018-11-04T03:00Z",
    );
    expect(iso(nextDay(utc("2018-11-04T03:00:00Z"), "America/Sao_Paulo"))).toBe(
      "2018-11-05T02:00Z",
    );
  });

  it("carries days over month ends", () => {
    expect(iso(localMidnight(2026, 2, 29, "UTC"))).toBe("2026-03-01T00:00Z");
    expect(iso(nextDay(utc("2026-12-31T00:00:00Z"), "UTC"))).toBe("2027-01-01T00:00Z");
  });
});
