import type { ActivityBucket } from "@/gen/syncr/v1/dashboard_pb";
import { toNumber, tsDate } from "@/lib/format";

/** ActivityPoint is one bucket of the activity charts, with plain numbers. */
export interface ActivityPoint {
  /** start is the bucket's start, in ms. */
  start: number;
  succeeded: number;
  failed: number;
  interrupted: number;
  cancelled: number;
  bytes: number;
  files: number;
}

/** BucketUnit is the size of an activity bucket. */
export type BucketUnit = "hour" | "day";

function step(t: number, unit: BucketUnit): number {
  const d = new Date(t);
  if (unit === "hour") d.setHours(d.getHours() + 1);
  else d.setDate(d.getDate() + 1);
  return d.getTime();
}

function floor(t: number, unit: BucketUnit): number {
  const d = new Date(t);
  if (unit === "hour") d.setMinutes(0, 0, 0);
  else d.setHours(0, 0, 0, 0);
  return d.getTime();
}

/**
 * shapeActivity turns the API buckets into one point per bucket from
 * `from` to `until`, filling buckets the API left out with zeros. Day
 * buckets follow local midnights, so DST days still get one bucket each.
 */
export function shapeActivity(
  buckets: ActivityBucket[],
  range: { from?: Date; until?: Date; unit: BucketUnit },
): ActivityPoint[] {
  const byStart = new Map<number, ActivityBucket>();
  for (const b of buckets) {
    const at = tsDate(b.start);
    if (at) byStart.set(floor(at.getTime(), range.unit), b);
  }
  const starts = [...byStart.keys()].sort((a, b) => a - b);
  const from = range.from ? floor(range.from.getTime(), range.unit) : starts[0];
  const until =
    range.until?.getTime() ?? (starts.length ? step(starts[starts.length - 1], range.unit) : 0);
  const out: ActivityPoint[] = [];
  if (from === undefined) return out;
  for (let t = from; t < until && out.length < 1000; t = step(t, range.unit)) {
    const b = byStart.get(t);
    out.push({
      start: t,
      succeeded: toNumber(b?.succeeded),
      failed: toNumber(b?.failed),
      interrupted: toNumber(b?.interrupted),
      cancelled: toNumber(b?.cancelled),
      bytes: toNumber(b?.bytes),
      files: toNumber(b?.files),
    });
  }
  return out;
}

/** The stacked series of the activity chart, bottom first. */
export const activityStatuses = ["succeeded", "failed", "interrupted", "cancelled"] as const;

/** ActivityStatus is one stacked series. */
export type ActivityStatus = (typeof activityStatuses)[number];

/**
 * stack returns the cumulative totals of each status, bottom first, so each
 * series can be drawn as a bar from zero up to its running total.
 */
export function stack(points: ActivityPoint[]): number[][] {
  const out = activityStatuses.map(() => [] as number[]);
  for (const p of points) {
    let acc = 0;
    activityStatuses.forEach((s, i) => {
      acc += p[s];
      out[i].push(acc);
    });
  }
  return out;
}

const longHourFmt = new Intl.DateTimeFormat("en-GB", {
  weekday: "short",
  day: "numeric",
  month: "short",
  hour: "2-digit",
  minute: "2-digit",
});
const longDayFmt = new Intl.DateTimeFormat("en-GB", {
  weekday: "long",
  day: "numeric",
  month: "long",
});

/** bucketLabel names a bucket for the hover line: "Sun 8 Nov, 14:00". */
export function bucketLabel(start: number, unit: BucketUnit): string {
  return (unit === "hour" ? longHourFmt : longDayFmt).format(new Date(start));
}
