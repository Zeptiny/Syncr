import type { ActivityBucket } from "@/gen/syncr/v1/dashboard_pb";
import { toNumber, tsDate } from "@/lib/format";
import { browserTimeZone } from "@/lib/timezones";
import { floorDay, floorHour, nextDay, sameWallClock } from "@/lib/zone";

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

const HOUR = 3_600_000;

/** floor is the start of the bucket containing t, as the server's date_trunc computes it. */
function floor(t: number, unit: BucketUnit, timeZone: string): number {
  return unit === "hour" ? floorHour(t, timeZone) : floorDay(t, timeZone);
}

/**
 * step is the start of the bucket after the one starting at t. Hours step in
 * absolute time, so the repeated hour of a fall-back is visited and the
 * missing hour of a spring-forward is not; days step to the next local
 * midnight, so they are 23 to 25 hours long across DST changes.
 */
function step(t: number, unit: BucketUnit, timeZone: string): number {
  if (unit === "day") return nextDay(t, timeZone);
  return Math.max(t + 1, floorHour(t + HOUR, timeZone));
}

/**
 * shapeActivity turns the API buckets into one point per bucket from
 * `from` to `until`, filling buckets the API left out with zeros. The API
 * already sends every bucket (date_trunc in the request's zone), so this
 * only matches buckets by their exact start instant and fills gaps the same
 * way: timeZone is the zone the dashboard was requested in (the browser's
 * by default), and nothing depends on the browser's own zone rules.
 */
export function shapeActivity(
  buckets: ActivityBucket[],
  range: { from?: Date; until?: Date; unit: BucketUnit; timeZone?: string },
): ActivityPoint[] {
  const timeZone = range.timeZone ?? browserTimeZone();
  const byStart = new Map<number, ActivityBucket>();
  for (const b of buckets) {
    const at = tsDate(b.start);
    if (at) byStart.set(floor(at.getTime(), range.unit, timeZone), b);
  }
  const starts = [...byStart.keys()].sort((a, b) => a - b);
  const from = range.from ? floor(range.from.getTime(), range.unit, timeZone) : starts[0];
  const until =
    range.until?.getTime() ??
    (starts.length ? step(starts[starts.length - 1], range.unit, timeZone) : 0);
  const out: ActivityPoint[] = [];
  if (from === undefined) return out;
  for (let t = from; t < until && out.length < 1000; t = step(t, range.unit, timeZone)) {
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

const longFormats = new Map<
  string,
  [Intl.DateTimeFormat, Intl.DateTimeFormat, Intl.DateTimeFormat]
>();

function formatsFor(timeZone: string) {
  let f = longFormats.get(timeZone);
  if (!f) {
    const hour: Intl.DateTimeFormatOptions = {
      timeZone,
      weekday: "short",
      day: "numeric",
      month: "short",
      hour: "2-digit",
      minute: "2-digit",
      hourCycle: "h23",
    };
    f = [
      new Intl.DateTimeFormat("en-GB", hour),
      new Intl.DateTimeFormat("en-GB", { ...hour, timeZoneName: "short" }),
      new Intl.DateTimeFormat("en-GB", {
        timeZone,
        weekday: "long",
        day: "numeric",
        month: "long",
      }),
    ];
    longFormats.set(timeZone, f);
  }
  return f;
}

/**
 * bucketLabel names a bucket for the hover line: "Sun 8 Nov, 14:00". The
 * two passes of a repeated fall-back hour also carry the zone's name
 * ("Sun 1 Nov, 01:00 GMT-4" and "... 01:00 GMT-5") so they can be told apart.
 */
export function bucketLabel(
  start: number,
  unit: BucketUnit,
  timeZone: string = browserTimeZone(),
): string {
  const [hour, hourWithZone, day] = formatsFor(timeZone);
  if (unit === "day") return day.format(new Date(start));
  const repeated =
    sameWallClock(start, start - HOUR, timeZone) || sameWallClock(start, start + HOUR, timeZone);
  return (repeated ? hourWithZone : hour).format(new Date(start));
}
