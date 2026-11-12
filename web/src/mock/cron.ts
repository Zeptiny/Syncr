/**
 * A small cron evaluator for the dev mock and test fixtures. It follows the
 * controller's rules (internal/cronexpr) closely enough for previews:
 * wall-time occurrences in a zone, a time in a spring-forward gap runs at
 * the transition, and an ambiguous fall-back time runs once, at the earlier
 * instant. The real UI always asks the controller (PreviewSchedule).
 */

const descriptors: Record<string, string> = {
  "@yearly": "0 0 1 1 *",
  "@annually": "0 0 1 1 *",
  "@monthly": "0 0 1 * *",
  "@weekly": "0 0 * * 0",
  "@daily": "0 0 * * *",
  "@midnight": "0 0 * * *",
  "@hourly": "0 * * * *",
};

const months = ["jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"];
const days = ["sun", "mon", "tue", "wed", "thu", "fri", "sat"];

interface Field {
  set: Set<number>;
  star: boolean;
}

/** CronError says which part of an expression is wrong. */
export class CronError extends Error {
  readonly field: "cron" | "timezone";
  constructor(field: "cron" | "timezone", message: string) {
    super(message);
    this.field = field;
  }
}

function parseField(src: string, min: number, max: number, names?: string[]): Field {
  const set = new Set<number>();
  const num = (s: string) => {
    const i = names?.indexOf(s.toLowerCase()) ?? -1;
    const n = i >= 0 ? i + (names === months ? 1 : 0) : Number(s);
    if (!Number.isInteger(n) || n < min || n > max)
      throw new CronError("cron", `${s} is out of range (${min}-${max}).`);
    return n;
  };
  for (const part of src.split(",")) {
    const [range, stepS] = part.split("/");
    const step = stepS === undefined ? 1 : Number(stepS);
    if (!Number.isInteger(step) || step < 1) throw new CronError("cron", `Bad step in ${part}.`);
    let lo: number;
    let hi: number;
    if (range === "*" || range === "?") {
      lo = min;
      hi = max;
    } else if (range.includes("-")) {
      const [a, b] = range.split("-");
      lo = num(a);
      hi = num(b);
    } else {
      lo = num(range);
      hi = stepS === undefined ? lo : max;
    }
    for (let v = lo; v <= hi; v += step) set.add(v);
  }
  return { set, star: src === "*" || src === "?" };
}

/** Expr is a parsed five-field expression in a zone. */
export interface Expr {
  minute: Field;
  hour: Field;
  dom: Field;
  month: Field;
  dow: Field;
  tz: string;
  normalized: string;
}

/** parseCron parses expr in tz, throwing CronError when it is invalid. */
export function parseCron(expr: string, tz: string): Expr {
  let e = expr.trim().replace(/\s+/g, " ");
  if (!tz || tz === "Local") throw new CronError("timezone", "Choose a time zone.");
  try {
    new Intl.DateTimeFormat("en", { timeZone: tz });
  } catch {
    throw new CronError("timezone", `${tz} is not a known time zone.`);
  }
  if (e.startsWith("@")) {
    const d = descriptors[e.toLowerCase()];
    if (!d) throw new CronError("cron", `${e} is not supported.`);
    e = d;
  }
  const f = e.split(" ");
  if (f.length !== 5) throw new CronError("cron", "Use five fields.");
  const dow = parseField(f[4], 0, 7, days);
  if (dow.set.has(7)) {
    dow.set.delete(7);
    dow.set.add(0);
  }
  return {
    minute: parseField(f[0], 0, 59),
    hour: parseField(f[1], 0, 23),
    dom: parseField(f[2], 1, 31),
    month: parseField(f[3], 1, 12, months),
    dow,
    tz,
    normalized: e,
  };
}

const partFormats = new Map<string, Intl.DateTimeFormat>();

function wall(tz: string, ms: number): [number, number, number, number, number] {
  let fmt = partFormats.get(tz);
  if (!fmt) {
    fmt = new Intl.DateTimeFormat("en-US", {
      timeZone: tz,
      hourCycle: "h23",
      year: "numeric",
      month: "numeric",
      day: "numeric",
      hour: "numeric",
      minute: "numeric",
    });
    partFormats.set(tz, fmt);
  }
  const p: Record<string, number> = {};
  for (const x of fmt.formatToParts(new Date(ms))) p[x.type] = Number(x.value);
  return [p.year, p.month, p.day, p.hour, p.minute];
}

function wallKey(w: number[]): number {
  return Date.UTC(w[0], w[1] - 1, w[2], w[3], w[4]);
}

function offsetAt(tz: string, ms: number): number {
  return wallKey(wall(tz, ms)) - Math.floor(ms / 60_000) * 60_000;
}

/** instantOf maps a wall time (as a UTC-read ms value) to an instant in tz. */
function instantOf(tz: string, naive: number): number {
  const cands = [
    naive - offsetAt(tz, naive - 12 * 3_600_000),
    naive - offsetAt(tz, naive + 12 * 3_600_000),
  ]
    .filter((t) => wallKey(wall(tz, t)) === naive)
    .sort((a, b) => a - b);
  if (cands.length) return cands[0];
  // In a gap: the first minute whose wall time is past the wanted one.
  let lo = naive - offsetAt(tz, naive + 12 * 3_600_000);
  let hi = naive - offsetAt(tz, naive - 12 * 3_600_000);
  if (lo > hi) [lo, hi] = [hi, lo];
  while (hi - lo > 60_000) {
    const mid = Math.floor((lo + hi) / 120_000) * 60_000;
    if (wallKey(wall(tz, mid)) >= naive) hi = mid;
    else lo = mid;
  }
  return hi;
}

/**
 * nextRuns returns up to n occurrences strictly after `after` (and not past
 * `until`, when given).
 */
export function nextRuns(e: Expr, after: number, n: number, until = Infinity): number[] {
  const out: number[] = [];
  const start = wall(e.tz, after);
  let day = Date.UTC(start[0], start[1] - 1, start[2]);
  const hours = [...e.hour.set].sort((a, b) => a - b);
  const minutes = [...e.minute.set].sort((a, b) => a - b);
  for (let i = 0; i < 366 * 5 && out.length < n && day <= until; i++, day += 86_400_000) {
    const d = new Date(day);
    if (!e.month.set.has(d.getUTCMonth() + 1)) continue;
    const domOk = e.dom.set.has(d.getUTCDate());
    const dowOk = e.dow.set.has(d.getUTCDay());
    const dayOk = e.dom.star || e.dow.star ? domOk && dowOk : domOk || dowOk;
    if (!dayOk) continue;
    for (const h of hours) {
      for (const m of minutes) {
        const t = instantOf(e.tz, day + h * 3_600_000 + m * 60_000);
        if (t > until) return out;
        if (t > after && t !== out[out.length - 1]) out.push(t);
        if (out.length >= n) return out;
      }
    }
  }
  return out;
}

/** next returns the first occurrence after `after`, or undefined. */
export function next(expr: string, tz: string, after: number): number | undefined {
  return nextRuns(parseCron(expr, tz), after, 1)[0];
}

/** previous returns up to n occurrences at or before `before`, newest first. */
export function previousRuns(
  expr: string,
  tz: string,
  before: number,
  n: number,
  span: number,
): number[] {
  const e = parseCron(expr, tz);
  return nextRuns(e, before - span, 100_000, before)
    .reverse()
    .slice(0, n);
}
