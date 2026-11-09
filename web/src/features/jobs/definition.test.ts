import { create } from "@bufbuild/protobuf";
import { DurationSchema } from "@bufbuild/protobuf/wkt";
import { describe, expect, it } from "vitest";

import { JobDefinitionSchema, type JobDefinition } from "@/gen/syncr/v1/api_pb";
import { RcloneTransferMode } from "@/gen/syncr/v1/link_pb";

import { definitionState, toDefinition, validateDefinition } from "./definition";

interface Limits {
  bandwidthLimitBytesPerSecond?: bigint;
  maxTransferBytes?: bigint;
  maxDuration?: { seconds: bigint; nanos?: number };
}

function stored(limits: Limits): JobDefinition {
  return create(JobDefinitionSchema, {
    operation: {
      case: "rcloneTransfer",
      value: {
        mode: RcloneTransferMode.COPY,
        source: { remoteId: "", path: "/srv/in" },
        destination: { remoteId: "r1", path: "out" },
        options: {
          ...limits,
          maxDuration: limits.maxDuration && create(DurationSchema, limits.maxDuration),
        },
      },
    },
  });
}

function optionsOf(def: { operation?: { case?: string; value?: unknown } }) {
  if (def.operation?.case !== "rcloneTransfer") throw new Error("not a transfer");
  return (def.operation.value as { options?: Limits }).options;
}

describe("editing a definition keeps byte and duration values", () => {
  it.each<[string, Limits]>([
    ["a max transfer of 1 MiB (shown as 0 GiB before)", { maxTransferBytes: 1048576n }],
    ["a bwlimit of 128 KiB/s (came back as 0.13 MiB)", { bandwidthLimitBytesPerSecond: 131072n }],
    ["a bwlimit below 0.005 MiB/s (became 0)", { bandwidthLimitBytesPerSecond: 1000n }],
    ["one byte per second", { bandwidthLimitBytesPerSecond: 1n }],
    ["a bwlimit that is no round number", { bandwidthLimitBytesPerSecond: 1234567n }],
    ["a huge max transfer", { maxTransferBytes: 9223372036854775807n }],
    ["a max transfer past 2^53", { maxTransferBytes: 9007199254740993n }],
    ["a huge TiB-aligned max transfer", { maxTransferBytes: 8191n * 1024n ** 4n }],
    ["a max duration of 100 s", { maxDuration: { seconds: 100n } }],
    ["a max duration of 90 min", { maxDuration: { seconds: 5400n } }],
    ["a max duration with nanoseconds", { maxDuration: { seconds: 1n, nanos: 123_456_789 } }],
    [
      "everything at once",
      {
        bandwidthLimitBytesPerSecond: 524288n,
        maxTransferBytes: 3n * 1024n ** 3n,
        maxDuration: { seconds: 7200n },
      },
    ],
  ])("%s", (_name, limits) => {
    const def = stored(limits);
    const want = optionsOf(def);
    const got = optionsOf(toDefinition(definitionState(def)));
    expect(got?.bandwidthLimitBytesPerSecond).toBe(want?.bandwidthLimitBytesPerSecond);
    expect(got?.maxTransferBytes).toBe(want?.maxTransferBytes);
    expect(got?.maxDuration).toEqual(want?.maxDuration);
    expect(validateDefinition(definitionState(def)).options).toBeUndefined();
  });

  it("shows stored values in a unit that holds them exactly", () => {
    const s = definitionState(stored({ maxTransferBytes: 1048576n }));
    expect([s.maxTransfer, s.maxTransferUnit]).toEqual(["1", "MiB"]);
    const bw = definitionState(stored({ bandwidthLimitBytesPerSecond: 131072n }));
    expect([bw.bwlimit, bw.bwlimitUnit]).toEqual(["128", "KiB"]);
  });

  it("leaves unset limits unset", () => {
    const o = optionsOf(toDefinition(definitionState(stored({}))));
    expect(o?.bandwidthLimitBytesPerSecond).toBeUndefined();
    expect(o?.maxTransferBytes).toBeUndefined();
    expect(o?.maxDuration).toBeUndefined();
  });

  it("converts what the user types in the chosen unit", () => {
    const s = definitionState(stored({}));
    s.bwlimit = "1.5";
    s.bwlimitUnit = "MiB";
    s.maxTransfer = "2";
    s.maxTransferUnit = "TiB";
    s.maxDuration = "1.5";
    s.maxDurationUnit = "h";
    const o = optionsOf(toDefinition(s));
    expect(o?.bandwidthLimitBytesPerSecond).toBe(1572864n);
    expect(o?.maxTransferBytes).toBe(2n * 1024n ** 4n);
    expect(o?.maxDuration).toMatchObject({ seconds: 5400n, nanos: 0 });
  });

  it("rejects numbers that are not plain decimals", () => {
    for (const bad of ["-1", "1e3", "x"]) {
      const s = definitionState(stored({}));
      s.maxTransfer = bad;
      expect(validateDefinition(s).options).toBe("Numbers must be zero or more.");
    }
  });
});
