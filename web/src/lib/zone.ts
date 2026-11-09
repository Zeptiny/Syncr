/*
 * Time zone arithmetic on absolute instants (epoch milliseconds), for code
 * that has to line up with the server's date_trunc(unit, t, zone) buckets.
 * Nothing here uses the browser's own zone or Date's local-time setters, which
 * skip the repeated hour of a fall-back and resolve a missing hour by guessing.
 */

const HOUR = 3_600_000;
const MIN = 60_000;

const formatters = new Map<string, Intl.DateTimeFormat>();

function formatterFor(timeZone: string): Intl.DateTimeFormat {
  let f = formatters.get(timeZone);
  if (!f) {
    f = new Intl.DateTimeFormat("en-US", {
      timeZone,
      hourCycle: "h23",
      year: "numeric",
      month: "numeric",
      day: "numeric",
      hour: "numeric",
      minute: "numeric",
      second: "numeric",
    });
    formatters.set(timeZone, f);
  }
  return f;
}

/** LocalParts is a wall-clock reading. */
export interface LocalParts {
  year: number;
  /** month is 1 to 12. */
  month: number;
  day: number;
  hour: number;
  minute: number;
  second: number;
}

/** localParts reads the wall clock of timeZone at the instant t. */
export function localParts(t: number, timeZone: string): LocalParts {
  const out: Record<string, number> = {};
  for (const p of formatterFor(timeZone).formatToParts(new Date(t))) {
    if (p.type !== "literal") out[p.type] = Number(p.value);
  }
  return {
    year: out.year,
    month: out.month,
    day: out.day,
    hour: out.hour % 24,
    minute: out.minute,
    second: out.second,
  };
}

/** utcOffset is timeZone's offset from UTC at t, in milliseconds (east is positive). */
export function utcOffset(t: number, timeZone: string): number {
  const p = localParts(t, timeZone);
  const whole = Math.floor(t / 1000) * 1000;
  return Date.UTC(p.year, p.month - 1, p.day, p.hour, p.minute, p.second) - whole;
}

/**
 * floorHour is the start of the zone's local hour containing t, keeping the
 * offset t has (date_trunc('hour', t, zone)). Both passes of a repeated
 * fall-back hour keep their own start, and half-hour zones bucket at :30 UTC.
 */
export function floorHour(t: number, timeZone: string): number {
  const local = t + utcOffset(t, timeZone);
  return t - (((local % HOUR) + HOUR) % HOUR);
}

/**
 * localMidnight is the instant of midnight on the given date in timeZone,
 * with the server's rules: a midnight that happens twice resolves to the
 * later instant, one that does not exist (a spring-forward at 00:00) to the
 * transition instant. month is 1 to 12; day may overflow into the next month.
 */
export function localMidnight(year: number, month: number, day: number, timeZone: string): number {
  const naive = Date.UTC(year, month - 1, day);
  const y = new Date(naive);
  const want = [y.getUTCFullYear(), y.getUTCMonth() + 1, y.getUTCDate()];
  const before = utcOffset(naive - 36 * HOUR, timeZone);
  const after = utcOffset(naive + 36 * HOUR, timeZone);
  let latest: number | undefined;
  for (const off of [before, after]) {
    const u = naive - off;
    const p = localParts(u, timeZone);
    if (
      p.year === want[0] &&
      p.month === want[1] &&
      p.day === want[2] &&
      p.hour === 0 &&
      p.minute === 0 &&
      (latest === undefined || u > latest)
    ) {
      latest = u;
    }
  }
  return latest ?? naive - before;
}

/** floorDay is local midnight of the zone's date at t (date_trunc('day', t, zone)). */
export function floorDay(t: number, timeZone: string): number {
  const p = localParts(t, timeZone);
  return localMidnight(p.year, p.month, p.day, timeZone);
}

/** nextDay is the start of the local day after the one that starts at or contains t. */
export function nextDay(t: number, timeZone: string): number {
  const p = localParts(t, timeZone);
  return localMidnight(p.year, p.month, p.day + 1, timeZone);
}

/** sameWallClock reports whether two instants read the same on the zone's clock (to the minute). */
export function sameWallClock(a: number, b: number, timeZone: string): boolean {
  const x = localParts(Math.floor(a / MIN) * MIN, timeZone);
  const y = localParts(Math.floor(b / MIN) * MIN, timeZone);
  return (
    x.year === y.year &&
    x.month === y.month &&
    x.day === y.day &&
    x.hour === y.hour &&
    x.minute === y.minute
  );
}
