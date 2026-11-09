import { create } from "@bufbuild/protobuf";
import { DurationSchema, type Duration } from "@bufbuild/protobuf/wkt";

/*
 * Exact conversions between stored values (bytes, durations) and the
 * "number + unit" fields of the job form. Everything is bigint math on
 * decimal text, so a stored value shows as an exact number in some unit and
 * goes back unchanged unless the user edits it.
 */

/** ByteUnit is a binary size unit of the form. */
export type ByteUnit = "B" | "KiB" | "MiB" | "GiB" | "TiB";

/** byteUnits lists the units smallest first, with their size in bytes. */
export const byteUnits: { id: ByteUnit; size: bigint }[] = [
  { id: "B", size: 1n },
  { id: "KiB", size: 1024n },
  { id: "MiB", size: 1024n ** 2n },
  { id: "GiB", size: 1024n ** 3n },
  { id: "TiB", size: 1024n ** 4n },
];

/** DurationUnit is a time unit of the form. */
export type DurationUnit = "s" | "min" | "h";

const NANOS = 1_000_000_000n;

/** durationUnits lists the units smallest first, with their size in nanoseconds. */
export const durationUnits: { id: DurationUnit; label: string; size: bigint }[] = [
  { id: "s", label: "sec", size: NANOS },
  { id: "min", label: "min", size: 60n * NANOS },
  { id: "h", label: "hours", size: 3600n * NANOS },
];

/** Quantity is a number as typed, in a unit. */
export interface Quantity<U extends string> {
  text: string;
  unit: U;
}

/** isDecimal reports whether text is a plain non-negative decimal: 12, 1.5 or .5. */
export function isDecimal(text: string): boolean {
  return /^(\d+(\.\d*)?|\.\d+)$/.test(text);
}

/**
 * decimalToScaled returns text * scale rounded to the nearest integer (half
 * up), without floating point. text must satisfy isDecimal.
 */
export function decimalToScaled(text: string, scale: bigint): bigint {
  const [int = "", frac = ""] = text.split(".");
  const digits = BigInt(int + frac || "0");
  const div = 10n ** BigInt(frac.length);
  return (digits * scale + div / 2n) / div;
}

/** scaledToDecimal writes value / scale as a decimal, without trailing zeros. */
function scaledToDecimal(value: bigint, scale: bigint): string {
  const int = value / scale;
  let rest = value % scale;
  if (rest === 0n) return String(int);
  let frac = "";
  // scale is a power of ten times a small factor; 12 digits are plenty.
  for (let i = 0; i < 12 && rest !== 0n; i++) {
    rest *= 10n;
    frac += String(rest / scale);
    rest %= scale;
  }
  return `${int}.${frac}`;
}

/** toBigInt reads a stored int64 (bigint, or a number from older callers). */
function toBigInt(v: bigint | number): bigint {
  return typeof v === "bigint" ? v : BigInt(Math.trunc(v));
}

/**
 * bytesToQuantity shows a stored byte count in the largest unit that holds
 * it as a whole number: 1048576 is 1 MiB, 131072 is 128 KiB, 1000 is 1000 B.
 * An unset value is empty, in defaultUnit.
 */
export function bytesToQuantity(
  v: bigint | number | undefined,
  defaultUnit: ByteUnit,
): Quantity<ByteUnit> {
  if (v === undefined) return { text: "", unit: defaultUnit };
  const n = toBigInt(v);
  if (n <= 0n) return { text: String(n), unit: defaultUnit };
  let best = byteUnits[0];
  for (const u of byteUnits) if (n % u.size === 0n) best = u;
  return { text: String(n / best.size), unit: best.id };
}

/** quantityToBytes is the inverse: undefined for empty (or invalid) text. */
export function quantityToBytes(q: Quantity<ByteUnit>): bigint | undefined {
  if (q.text === "" || !isDecimal(q.text)) return undefined;
  const size = byteUnits.find((u) => u.id === q.unit)?.size ?? 1n;
  return decimalToScaled(q.text, size);
}

const totalNanos = (d: Duration) => d.seconds * NANOS + BigInt(d.nanos);

/**
 * durationToQuantity shows a stored duration in the largest unit that holds
 * it as a whole number; one that is not a whole number of seconds is a
 * decimal number of seconds. An unset duration is empty, in defaultUnit.
 */
export function durationToQuantity(
  d: Duration | undefined,
  defaultUnit: DurationUnit,
): Quantity<DurationUnit> {
  if (!d) return { text: "", unit: defaultUnit };
  const n = totalNanos(d);
  if (n <= 0n) return { text: String(n), unit: defaultUnit };
  let best = durationUnits[0];
  for (const u of durationUnits) if (n % u.size === 0n) best = u;
  return { text: scaledToDecimal(n, best.size), unit: best.id };
}

/** quantityToDuration is the inverse: undefined for empty (or invalid) text. */
export function quantityToDuration(q: Quantity<DurationUnit>): Duration | undefined {
  if (q.text === "" || !isDecimal(q.text)) return undefined;
  const size = durationUnits.find((u) => u.id === q.unit)?.size ?? NANOS;
  const n = decimalToScaled(q.text, size);
  return create(DurationSchema, { seconds: n / NANOS, nanos: Number(n % NANOS) });
}
