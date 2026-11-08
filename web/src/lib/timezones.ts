/**
 * Time zone helpers on top of Intl, so the browser's own tz database is
 * used and no dependency is needed.
 */

let cached: string[] | undefined;

/** timeZones lists the IANA zones the browser knows, with UTC first. */
export function timeZones(): string[] {
  if (cached) return cached;
  let zones: string[];
  try {
    zones = Intl.supportedValuesOf("timeZone");
  } catch {
    zones = [];
  }
  cached = ["UTC", ...zones.filter((z) => z !== "UTC" && z !== "Etc/UTC")];
  return cached;
}

/** browserTimeZone is the viewer's IANA zone, or UTC when unknown. */
export function browserTimeZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

/** isTimeZone reports whether the browser accepts tz as a zone name. */
export function isTimeZone(tz: string): boolean {
  if (!tz) return false;
  try {
    new Intl.DateTimeFormat("en", { timeZone: tz });
    return true;
  } catch {
    return false;
  }
}

const offsetFormats = new Map<string, Intl.DateTimeFormat>();

/** tzOffset is the UTC offset of tz at date, as "+01:00" (or "+00:00"). */
export function tzOffset(tz: string, date: Date = new Date()): string {
  let f = offsetFormats.get(tz);
  if (!f) {
    try {
      f = new Intl.DateTimeFormat("en-US", { timeZone: tz, timeZoneName: "longOffset" });
    } catch {
      return "";
    }
    offsetFormats.set(tz, f);
  }
  const name = f.formatToParts(date).find((p) => p.type === "timeZoneName")?.value ?? "";
  const m = /([+-]\d{2}:\d{2})/.exec(name);
  return m ? m[1] : "+00:00";
}

/** offsetMinutes is the UTC offset of tz at date in minutes (east positive). */
export function offsetMinutes(tz: string, date: Date = new Date()): number {
  const m = /([+-])(\d{2}):(\d{2})/.exec(tzOffset(tz, date));
  if (!m) return 0;
  return (m[1] === "-" ? -1 : 1) * (Number(m[2]) * 60 + Number(m[3]));
}

/** zoneLabel makes a zone name readable: "America/New_York" → "America / New York". */
export function zoneLabel(tz: string): string {
  return tz.replaceAll("_", " ").replaceAll("/", " / ");
}

const zoneFormats = new Map<string, Intl.DateTimeFormat>();

/**
 * formatInZone renders a date as wall time in tz: "Sun 15 Nov 2026, 02:30".
 */
export function formatInZone(date: Date, tz: string, withYear = true): string {
  const key = `${tz}|${withYear}`;
  let f = zoneFormats.get(key);
  if (!f) {
    f = new Intl.DateTimeFormat("en-GB", {
      timeZone: tz,
      weekday: "short",
      day: "numeric",
      month: "short",
      year: withYear ? "numeric" : undefined,
      hour: "2-digit",
      minute: "2-digit",
    });
    zoneFormats.set(key, f);
  }
  return f.format(date);
}

/** hasOffsetChange reports whether the runs cross a UTC offset change (DST). */
export function hasOffsetChange(runs: Date[], timezone: string): boolean {
  return new Set(runs.map((r) => tzOffset(timezone, r))).size > 1;
}

/** ZoneOption is a zone with its current UTC offset. */
export interface ZoneOption {
  zone: string;
  offset: string;
}

function squash(s: string): string {
  return s.toLowerCase().replace(/[\s_/]+/g, "");
}

/**
 * filterZones keeps the zones whose name or current offset matches the
 * query, ignoring case, spaces, underscores and slashes ("new york",
 * "+05:30", "utc+1"). An empty query keeps them all.
 */
export function filterZones(query: string, zones: ZoneOption[]): ZoneOption[] {
  const q = query.trim();
  if (!q) return zones;
  const offset = /^(?:utc|gmt)?\s*([+-])(\d{1,2})(?::?(\d{2}))?$/i.exec(q);
  if (offset) {
    const want = `${offset[1]}${offset[2].padStart(2, "0")}:${offset[3] ?? "00"}`;
    return zones.filter((z) => z.offset === want);
  }
  const s = squash(q);
  return zones.filter((z) => squash(z.zone).includes(s));
}
