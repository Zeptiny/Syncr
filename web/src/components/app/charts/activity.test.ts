import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { describe, expect, it } from "vitest";

import { ActivityBucketSchema } from "@/gen/syncr/v1/dashboard_pb";

import { shapeActivity, stack } from "./activity";

function at(y: number, m: number, d: number, h = 0) {
  return new Date(y, m - 1, d, h).getTime();
}

describe("shapeActivity", () => {
  it("fills hour buckets the API left out", () => {
    const from = new Date(at(2026, 11, 8, 9));
    const until = new Date(at(2026, 11, 8, 13));
    const pts = shapeActivity(
      [
        create(ActivityBucketSchema, {
          start: timestampFromMs(at(2026, 11, 8, 10)),
          succeeded: 3n,
          bytes: 1024n,
        }),
        create(ActivityBucketSchema, {
          start: timestampFromMs(at(2026, 11, 8, 12)),
          failed: 1n,
        }),
      ],
      { from, until, unit: "hour" },
    );
    expect(pts.map((p) => p.start)).toEqual([9, 10, 11, 12].map((h) => at(2026, 11, 8, h)));
    expect(pts.map((p) => p.succeeded)).toEqual([0, 3, 0, 0]);
    expect(pts.map((p) => p.failed)).toEqual([0, 0, 0, 1]);
    expect(pts[1].bytes).toBe(1024);
  });

  it("steps day buckets by local midnights", () => {
    const pts = shapeActivity([], {
      from: new Date(at(2026, 3, 27)),
      until: new Date(at(2026, 3, 31)),
      unit: "day",
    });
    expect(pts.map((p) => new Date(p.start).getDate())).toEqual([27, 28, 29, 30]);
    expect(pts.every((p) => new Date(p.start).getHours() === 0)).toBe(true);
  });

  it("returns nothing for an empty range", () => {
    expect(shapeActivity([], { unit: "day" })).toEqual([]);
  });

  it("stacks the statuses bottom first", () => {
    const pts = shapeActivity(
      [
        create(ActivityBucketSchema, {
          start: timestampFromMs(at(2026, 1, 1)),
          succeeded: 5n,
          failed: 2n,
          interrupted: 1n,
          cancelled: 1n,
        }),
      ],
      { unit: "day" },
    );
    expect(stack(pts)).toEqual([[5], [7], [8], [9]]);
  });
});
