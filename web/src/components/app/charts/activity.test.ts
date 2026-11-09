import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { describe, expect, it } from "vitest";

import { ActivityBucketSchema } from "@/gen/syncr/v1/dashboard_pb";

import { bucketLabel, shapeActivity, stack } from "./activity";

const utc = (iso: string) => Date.parse(iso);
const iso = (ms: number) => new Date(ms).toISOString().slice(0, 16) + "Z";
const bucket = (start: string, counts: { succeeded?: bigint; failed?: bigint; bytes?: bigint }) =>
  create(ActivityBucketSchema, { start: timestampFromMs(utc(start)), ...counts });

const NY = "America/New_York";
const BERLIN = "Europe/Berlin";

describe("shapeActivity", () => {
  it("fills hour buckets the API left out", () => {
    const pts = shapeActivity(
      [
        bucket("2026-11-08T10:00:00Z", { succeeded: 3n, bytes: 1024n }),
        bucket("2026-11-08T12:00:00Z", { failed: 1n }),
      ],
      {
        from: new Date(utc("2026-11-08T09:00:00Z")),
        until: new Date(utc("2026-11-08T13:00:00Z")),
        unit: "hour",
        timeZone: "UTC",
      },
    );
    expect(pts.map((p) => iso(p.start))).toEqual([
      "2026-11-08T09:00Z",
      "2026-11-08T10:00Z",
      "2026-11-08T11:00Z",
      "2026-11-08T12:00Z",
    ]);
    expect(pts.map((p) => p.succeeded)).toEqual([0, 3, 0, 0]);
    expect(pts.map((p) => p.failed)).toEqual([0, 0, 0, 1]);
    expect(pts[1].bytes).toBe(1024);
  });

  it("returns nothing for an empty range", () => {
    expect(shapeActivity([], { unit: "day", timeZone: "UTC" })).toEqual([]);
  });

  it("stacks the statuses bottom first", () => {
    const pts = shapeActivity(
      [
        create(ActivityBucketSchema, {
          start: timestampFromMs(utc("2026-01-01T00:00:00Z")),
          succeeded: 5n,
          failed: 2n,
          interrupted: 1n,
          cancelled: 1n,
        }),
      ],
      { unit: "day", timeZone: "UTC" },
    );
    expect(stack(pts)).toEqual([[5], [7], [8], [9]]);
  });

  describe("hour buckets across DST changes", () => {
    it("keeps the repeated hour at a fall-back (New York, 2026-11-01)", () => {
      // 01:00 happens twice: 05:00Z is 01:00 EDT and 06:00Z is 01:00 EST.
      const pts = shapeActivity(
        [
          bucket("2026-11-01T05:00:00Z", { succeeded: 1n }),
          bucket("2026-11-01T06:00:00Z", { succeeded: 2n }),
          bucket("2026-11-01T07:00:00Z", { succeeded: 4n }),
        ],
        {
          from: new Date(utc("2026-11-01T03:00:00Z")),
          until: new Date(utc("2026-11-01T09:30:00Z")),
          unit: "hour",
          timeZone: NY,
        },
      );
      expect(pts.map((p) => iso(p.start))).toEqual([
        "2026-11-01T03:00Z",
        "2026-11-01T04:00Z",
        "2026-11-01T05:00Z",
        "2026-11-01T06:00Z",
        "2026-11-01T07:00Z",
        "2026-11-01T08:00Z",
        "2026-11-01T09:00Z",
      ]);
      expect(pts.map((p) => p.succeeded)).toEqual([0, 0, 1, 2, 4, 0, 0]);
    });

    it("keeps the repeated hour at a fall-back (Berlin, 2026-10-25)", () => {
      // 02:00 happens twice: 00:00Z is 02:00 CEST and 01:00Z is 02:00 CET.
      const pts = shapeActivity(
        [
          bucket("2026-10-25T00:00:00Z", { failed: 1n }),
          bucket("2026-10-25T01:00:00Z", { failed: 2n }),
        ],
        {
          from: new Date(utc("2026-10-24T23:00:00Z")),
          until: new Date(utc("2026-10-25T03:30:00Z")),
          unit: "hour",
          timeZone: BERLIN,
        },
      );
      expect(pts.map((p) => iso(p.start))).toEqual([
        "2026-10-24T23:00Z",
        "2026-10-25T00:00Z",
        "2026-10-25T01:00Z",
        "2026-10-25T02:00Z",
        "2026-10-25T03:00Z",
      ]);
      expect(pts.map((p) => p.failed)).toEqual([0, 1, 2, 0, 0]);
    });

    it("skips the missing hour at a spring-forward (New York, 2026-03-08)", () => {
      // 02:00 does not exist: 06:00Z is 01:00 EST, 07:00Z is already 03:00 EDT.
      const pts = shapeActivity([bucket("2026-03-08T07:00:00Z", { succeeded: 5n })], {
        from: new Date(utc("2026-03-08T04:00:00Z")),
        until: new Date(utc("2026-03-08T09:30:00Z")),
        unit: "hour",
        timeZone: NY,
      });
      expect(pts.map((p) => iso(p.start))).toEqual([
        "2026-03-08T04:00Z",
        "2026-03-08T05:00Z",
        "2026-03-08T06:00Z",
        "2026-03-08T07:00Z",
        "2026-03-08T08:00Z",
        "2026-03-08T09:00Z",
      ]);
      expect(pts[3].succeeded).toBe(5);
    });

    it("skips the missing hour at a spring-forward (Berlin, 2026-03-29)", () => {
      // 02:00 does not exist: 00:00Z is 01:00 CET, 01:00Z is already 03:00 CEST.
      const pts = shapeActivity([], {
        from: new Date(utc("2026-03-28T23:00:00Z")),
        until: new Date(utc("2026-03-29T03:30:00Z")),
        unit: "hour",
        timeZone: BERLIN,
      });
      expect(pts.map((p) => iso(p.start))).toEqual([
        "2026-03-28T23:00Z",
        "2026-03-29T00:00Z",
        "2026-03-29T01:00Z",
        "2026-03-29T02:00Z",
        "2026-03-29T03:00Z",
      ]);
    });

    it("buckets half-hour zones on the zone's hour", () => {
      // India is +05:30: its hours start at :30 UTC.
      const pts = shapeActivity([], {
        from: new Date(utc("2026-06-01T05:45:00Z")),
        until: new Date(utc("2026-06-01T08:00:00Z")),
        unit: "hour",
        timeZone: "Asia/Kolkata",
      });
      expect(pts.map((p) => iso(p.start))).toEqual([
        "2026-06-01T05:30Z",
        "2026-06-01T06:30Z",
        "2026-06-01T07:30Z",
      ]);
    });

    it("matches API buckets however the browser's own zone behaves", () => {
      // Stored starts are exact instants; they must not be moved by flooring.
      const starts = ["2026-11-01T05:00:00Z", "2026-11-01T06:00:00Z"];
      const pts = shapeActivity(
        starts.map((s) => bucket(s, { succeeded: 1n })),
        { unit: "hour", timeZone: NY },
      );
      expect(pts.map((p) => iso(p.start))).toEqual(["2026-11-01T05:00Z", "2026-11-01T06:00Z"]);
      expect(pts.map((p) => p.succeeded)).toEqual([1, 1]);
    });
  });

  describe("day buckets across DST changes", () => {
    it("steps by local midnights over a 25-hour day (New York, 2026-11-01)", () => {
      const pts = shapeActivity([bucket("2026-11-01T04:00:00Z", { succeeded: 7n })], {
        from: new Date(utc("2026-10-31T04:00:00Z")),
        until: new Date(utc("2026-11-03T05:00:00Z")),
        unit: "day",
        timeZone: NY,
      });
      expect(pts.map((p) => iso(p.start))).toEqual([
        "2026-10-31T04:00Z",
        "2026-11-01T04:00Z",
        "2026-11-02T05:00Z",
      ]);
      expect(pts.map((p) => p.succeeded)).toEqual([0, 7, 0]);
    });

    it("steps by local midnights over a 23-hour day (New York, 2026-03-08)", () => {
      const pts = shapeActivity([], {
        from: new Date(utc("2026-03-07T05:00:00Z")),
        until: new Date(utc("2026-03-10T04:00:00Z")),
        unit: "day",
        timeZone: NY,
      });
      expect(pts.map((p) => iso(p.start))).toEqual([
        "2026-03-07T05:00Z",
        "2026-03-08T05:00Z",
        "2026-03-09T04:00Z",
      ]);
    });

    it("steps over a 25-hour day (Berlin, 2026-10-25)", () => {
      const pts = shapeActivity([], {
        from: new Date(utc("2026-10-24T22:00:00Z")),
        until: new Date(utc("2026-10-27T00:00:00Z")),
        unit: "day",
        timeZone: BERLIN,
      });
      expect(pts.map((p) => iso(p.start))).toEqual([
        "2026-10-24T22:00Z",
        "2026-10-25T23:00Z",
        "2026-10-26T23:00Z",
      ]);
    });
  });
});

describe("bucketLabel", () => {
  it("labels hours and days in the display zone", () => {
    expect(bucketLabel(utc("2026-11-08T19:00:00Z"), "hour", NY)).toBe("Sun 8 Nov, 14:00");
    expect(bucketLabel(utc("2026-11-08T05:00:00Z"), "day", NY)).toBe("Sunday 8 November");
    expect(bucketLabel(utc("2026-11-08T19:00:00Z"), "hour", BERLIN)).toBe("Sun 8 Nov, 20:00");
  });

  it("tells the two passes of a repeated hour apart", () => {
    const first = bucketLabel(utc("2026-11-01T05:00:00Z"), "hour", NY);
    const second = bucketLabel(utc("2026-11-01T06:00:00Z"), "hour", NY);
    expect(first).toContain("01:00");
    expect(second).toContain("01:00");
    expect(first).not.toBe(second);
    // The hours around it need no zone name.
    expect(bucketLabel(utc("2026-11-01T04:00:00Z"), "hour", NY)).toBe("Sun 1 Nov, 00:00");
    expect(bucketLabel(utc("2026-11-01T07:00:00Z"), "hour", NY)).toBe("Sun 1 Nov, 02:00");
  });
});
