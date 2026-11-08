import { describe, expect, it } from "vitest";

import {
  formatBytes,
  formatDuration,
  formatEta,
  formatRate,
  formatRelative,
  percent,
  rateBetween,
} from "./format";

describe("format", () => {
  it.each([
    [0, "0 B"],
    [-5, "0 B"],
    [512, "512 B"],
    [1536, "1.50 KiB"],
    [10 * 1024 * 1024, "10.0 MiB"],
    [5n * 1024n ** 3n, "5.00 GiB"],
  ])("formatBytes(%s) = %s", (n, want) => {
    expect(formatBytes(n)).toBe(want);
  });

  it("never renders a negative rate", () => {
    expect(formatRate(-100)).toBe("0 B/s");
    expect(formatRate(Number.NaN)).toBe("0 B/s");
    expect(rateBetween({ t: 10, bytes: 500 }, { t: 15, bytes: 100 })).toBe(0);
    expect(rateBetween({ t: 10, bytes: 0 }, { t: 10, bytes: 100 })).toBe(0);
    expect(rateBetween({ t: 0, bytes: 0 }, { t: 2, bytes: 2048 })).toBe(1024);
  });

  it.each([
    [5, "5s"],
    [192, "3m 12s"],
    [7500, "2h 05m"],
    [90000, "1d 1h"],
  ])("formatDuration(%s) = %s", (s, want) => {
    expect(formatDuration(s)).toBe(want);
  });

  it("estimates the ETA only when it can", () => {
    expect(formatEta(1000, 0)).toBe("–");
    expect(formatEta(0, 100)).toBe("–");
    expect(formatEta(6000, 100)).toBe("1m 00s");
  });

  it("formats relative times", () => {
    const now = new Date("2026-03-12T12:00:00Z");
    expect(formatRelative(new Date("2026-03-12T11:59:55Z"), now)).toBe("just now");
    expect(formatRelative(new Date("2026-03-12T11:55:00Z"), now)).toBe("5 minutes ago");
    expect(formatRelative(new Date("2026-03-12T09:00:00Z"), now)).toBe("3 hours ago");
    expect(formatRelative(new Date("2026-03-11T12:00:00Z"), now)).toBe("yesterday");
    expect(formatRelative(undefined, now)).toBe("never");
  });

  it("bounds percentages", () => {
    expect(percent(5, 0)).toBeUndefined();
    expect(percent(150, 100)).toBe(100);
  });
});
