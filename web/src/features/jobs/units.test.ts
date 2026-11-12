import { create } from "@bufbuild/protobuf";
import { DurationSchema } from "@bufbuild/protobuf/wkt";
import { describe, expect, it } from "vitest";

import {
  bytesToQuantity,
  decimalToScaled,
  durationToQuantity,
  isDecimal,
  quantityToBytes,
  quantityToDuration,
} from "./units";

describe("bytes", () => {
  it.each([
    [1048576n, "1", "MiB"],
    [131072n, "128", "KiB"],
    [1610612736n, "1536", "MiB"],
    [1n, "1", "B"],
    [1000n, "1000", "B"],
    [1023n, "1023", "B"],
    [4n * 1024n ** 4n, "4", "TiB"],
    [4096n * 1024n ** 4n, "4096", "TiB"],
    // Beyond Number.MAX_SAFE_INTEGER, and not a multiple of anything.
    [9007199254740993n, "9007199254740993", "B"],
    [9223372036854775807n, "9223372036854775807", "B"],
    [9223372036854775808n - 1024n, "9007199254740991", "KiB"],
  ])("shows %s as %s %s and gives it back unchanged", (bytes, text, unit) => {
    const q = bytesToQuantity(bytes, "MiB");
    expect(q).toEqual({ text, unit });
    expect(quantityToBytes(q)).toBe(bytes);
  });

  it("leaves unset values empty and keeps the default unit", () => {
    expect(bytesToQuantity(undefined, "GiB")).toEqual({ text: "", unit: "GiB" });
    expect(quantityToBytes({ text: "", unit: "GiB" })).toBeUndefined();
  });

  it("keeps a stored zero as zero", () => {
    const q = bytesToQuantity(0n, "MiB");
    expect(q.text).toBe("0");
    expect(quantityToBytes(q)).toBe(0n);
  });

  it("converts typed decimals exactly", () => {
    expect(quantityToBytes({ text: "1.5", unit: "GiB" })).toBe(1610612736n);
    expect(quantityToBytes({ text: "0.125", unit: "MiB" })).toBe(131072n);
    expect(quantityToBytes({ text: ".5", unit: "KiB" })).toBe(512n);
    expect(quantityToBytes({ text: "2.", unit: "KiB" })).toBe(2048n);
    // Rounds to the nearest byte.
    expect(quantityToBytes({ text: "0.1", unit: "MiB" })).toBe(104858n);
    expect(quantityToBytes({ text: "0.0001", unit: "KiB" })).toBe(0n);
    expect(quantityToBytes({ text: "8191", unit: "TiB" })).toBe(8191n * 1024n ** 4n);
  });

  it("rejects what is not a plain decimal", () => {
    for (const bad of ["-1", "1e3", "abc", "1,5", "1.2.3", " 1", "."]) {
      expect(isDecimal(bad)).toBe(false);
      expect(quantityToBytes({ text: bad, unit: "MiB" })).toBeUndefined();
    }
  });
});

describe("durations", () => {
  const dur = (seconds: bigint, nanos = 0) => create(DurationSchema, { seconds, nanos });

  it.each([
    [dur(3600n), "1", "h"],
    [dur(7200n), "2", "h"],
    [dur(5400n), "90", "min"],
    [dur(60n), "1", "min"],
    [dur(100n), "100", "s"],
    [dur(1n), "1", "s"],
    [dur(0n, 500_000_000), "0.5", "s"],
    [dur(90n, 250_000_000), "90.25", "s"],
    [dur(0n, 1), "0.000000001", "s"],
    [dur(315576000000n), "87660000", "h"],
  ])("shows %o as %s %s and gives it back unchanged", (d, text, unit) => {
    const q = durationToQuantity(d, "min");
    expect(q).toEqual({ text, unit });
    expect(quantityToDuration(q)).toEqual(d);
  });

  it("leaves unset values empty and converts typed decimals", () => {
    expect(durationToQuantity(undefined, "min")).toEqual({ text: "", unit: "min" });
    expect(quantityToDuration({ text: "", unit: "min" })).toBeUndefined();
    expect(quantityToDuration({ text: "1.5", unit: "min" })).toEqual(dur(90n));
    expect(quantityToDuration({ text: "0.1", unit: "h" })).toEqual(dur(360n));
    expect(quantityToDuration({ text: "2.5", unit: "s" })).toEqual(dur(2n, 500_000_000));
  });
});

describe("decimalToScaled", () => {
  it("rounds half up without floating point", () => {
    expect(decimalToScaled("0.5", 1n)).toBe(1n);
    expect(decimalToScaled("0.49", 1n)).toBe(0n);
    expect(decimalToScaled("1.1", 1000n)).toBe(1100n);
    expect(decimalToScaled("123456789012345678.5", 2n)).toBe(246913578024691357n);
  });
});
