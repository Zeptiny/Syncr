import { create } from "@bufbuild/protobuf";
import { DurationSchema, type Duration, type Timestamp } from "@bufbuild/protobuf/wkt";

/** toNumber converts protobuf int64 (bigint) and plain numbers. */
export function toNumber(v: bigint | number | undefined | null): number {
  if (v === undefined || v === null) return 0;
  return typeof v === "bigint" ? Number(v) : v;
}

/** tsDate converts a protobuf Timestamp; undefined when unset. */
export function tsDate(ts: Timestamp | undefined): Date | undefined {
  if (!ts) return undefined;
  const ms = Number(ts.seconds) * 1000 + Math.floor(ts.nanos / 1e6);
  return ms > 0 ? new Date(ms) : undefined;
}

/** durationSeconds converts a protobuf Duration to seconds. */
export function durationSeconds(d: Duration | undefined): number {
  if (!d) return 0;
  return Number(d.seconds) + d.nanos / 1e9;
}

/**
 * durationFromSeconds is the inverse of durationSeconds. Unlike
 * durationFromMs it rounds to whole nanoseconds, so a stored Duration
 * survives the round trip through a number of seconds.
 */
export function durationFromSeconds(seconds: number): Duration {
  const whole = Math.trunc(seconds);
  let nanos = Math.round((seconds - whole) * 1e9);
  let secs = whole;
  if (nanos >= 1e9) {
    secs += 1;
    nanos -= 1e9;
  }
  return create(DurationSchema, { seconds: BigInt(secs), nanos });
}

const units = ["B", "KiB", "MiB", "GiB", "TiB", "PiB"];

/** formatBytes renders a byte count with binary units: "1.4 GiB". */
export function formatBytes(v: bigint | number | undefined): string {
  let n = toNumber(v);
  if (!Number.isFinite(n) || n <= 0) return "0 B";
  let i = 0;
  while (n >= 1024 && i < units.length - 1) {
    n /= 1024;
    i++;
  }
  const digits = i === 0 || n >= 100 ? 0 : n >= 10 ? 1 : 2;
  return `${n.toFixed(digits)} ${units[i]}`;
}

/** formatRate renders bytes per second; negative or invalid rates show 0. */
export function formatRate(bytesPerSecond: number): string {
  return `${formatBytes(clampRate(bytesPerSecond))}/s`;
}

/** clampRate keeps a rate finite and never negative. */
export function clampRate(r: number): number {
  return Number.isFinite(r) && r > 0 ? r : 0;
}

/** Point is a cumulative byte count at a time in seconds. */
export interface Point {
  t: number;
  bytes: number;
}

/**
 * rateBetween is Δbytes/Δt between two cumulative samples, never negative
 * (counters can reset when a job is retried).
 */
export function rateBetween(a: Point, b: Point): number {
  const dt = b.t - a.t;
  if (dt <= 0) return 0;
  return clampRate((b.bytes - a.bytes) / dt);
}

/** formatDuration renders seconds compactly: "45s", "3m 12s", "2h 05m". */
export function formatDuration(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return "–";
  const s = Math.round(seconds);
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${String(s % 60).padStart(2, "0")}s`;
  const h = Math.floor(m / 60);
  if (h < 24) return `${h}h ${String(m % 60).padStart(2, "0")}m`;
  const d = Math.floor(h / 24);
  return `${d}d ${h % 24}h`;
}

/**
 * formatLimit renders a configured duration exactly, largest unit first:
 * "6h", "1h 30m", "2d 12h", "90s".
 */
export function formatLimit(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "–";
  let rest = Math.round(seconds);
  const parts: string[] = [];
  for (const [size, unit] of [
    [86_400, "d"],
    [3600, "h"],
    [60, "m"],
    [1, "s"],
  ] as const) {
    const n = Math.floor(rest / size);
    if (n > 0) parts.push(`${n}${unit}`);
    rest -= n * size;
  }
  return parts.join(" ");
}

/** formatEta estimates the remaining time; "–" when it can't be known. */
export function formatEta(remainingBytes: number, bytesPerSecond: number): string {
  const rate = clampRate(bytesPerSecond);
  if (remainingBytes <= 0 || rate === 0) return "–";
  return formatDuration(remainingBytes / rate);
}

const rtf = new Intl.RelativeTimeFormat("en", { numeric: "auto" });

/** formatRelative renders a time relative to now: "5 minutes ago". */
export function formatRelative(date: Date | undefined, now: Date = new Date()): string {
  if (!date) return "never";
  const diff = (date.getTime() - now.getTime()) / 1000;
  const abs = Math.abs(diff);
  if (abs < 10) return "just now";
  if (abs < 60) return rtf.format(Math.round(diff), "second");
  if (abs < 3600) return rtf.format(Math.round(diff / 60), "minute");
  if (abs < 86400) return rtf.format(Math.round(diff / 3600), "hour");
  if (abs < 86400 * 30) return rtf.format(Math.round(diff / 86400), "day");
  return formatDate(date);
}

const dateFmt = new Intl.DateTimeFormat("en-GB", {
  day: "numeric",
  month: "short",
  year: "numeric",
});
const dateTimeFmt = new Intl.DateTimeFormat("en-GB", {
  day: "numeric",
  month: "short",
  year: "numeric",
  hour: "2-digit",
  minute: "2-digit",
  second: "2-digit",
});

/** formatDate renders a calendar date: "12 Mar 2026". */
export function formatDate(date: Date | undefined): string {
  return date ? dateFmt.format(date) : "–";
}

/** formatDateTime renders a full timestamp for tooltips and detail views. */
export function formatDateTime(date: Date | undefined): string {
  return date ? dateTimeFmt.format(date) : "–";
}

const countFmt = new Intl.NumberFormat("en");

/** formatCount renders an integer with digit grouping. */
export function formatCount(v: bigint | number | undefined): string {
  return countFmt.format(toNumber(v));
}

/** percent returns done/total in [0, 100], or undefined if total is unknown. */
export function percent(done: number, total: number): number | undefined {
  if (total <= 0) return undefined;
  return Math.min(100, Math.max(0, (done / total) * 100));
}

/** shortId shortens a UUID for display: "3f2a9c1e". */
export function shortId(id: string): string {
  return id.replaceAll("-", "").slice(0, 8);
}
