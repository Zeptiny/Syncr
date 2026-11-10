import { describe, expect, it } from "vitest";

import {
  cleanTags,
  describeRetention,
  emptyRetention,
  hasRetentionRule,
  hostProblem,
  keepCount,
  MAX_KEEP,
  retentionPolicyProblem,
  tagsProblem,
  validRetentionDuration,
  type RetentionPolicyState,
} from "./retention";

const policy = (p: Partial<RetentionPolicyState>): RetentionPolicyState => ({
  ...emptyRetention(),
  ...p,
});

// The controller's rules (internal/jobkind, ResticForget.Check): keep_within
// matches ^([1-9][0-9]*[ymdh])+$, counts are whole numbers from 0 to 100000,
// tags are non-empty, at most 128 bytes, without commas or control
// characters, at most 32 of them, and a policy needs at least one rule.
describe("validRetentionDuration", () => {
  it.each(["1d1y", "2y5m7d3h", "30d", "1y6m", "12h", "6m1y", "1h1h", "100y"])("accepts %s", (d) =>
    expect(validRetentionDuration(d)).toBe(true),
  );
  it.each([
    "",
    "0d",
    "01d",
    "1y0m",
    "30",
    "d",
    "1w",
    "1D",
    "1d ",
    " 1d",
    "1.5d",
    "-1d",
    "two weeks",
  ])("refuses %j", (d) => expect(validRetentionDuration(d)).toBe(false));
});

describe("keepCount", () => {
  it("reads whole numbers only", () => {
    expect(keepCount("")).toBe(0);
    expect(keepCount("7")).toBe(7);
    expect(keepCount(" 12 ")).toBe(12);
    expect(keepCount("007")).toBe(7);
    for (const bad of ["1.5", "1e3", "-1", "x", "0x10"]) expect(keepCount(bad)).toBeNaN();
  });
});

describe("hasRetentionRule", () => {
  it.each<[Partial<RetentionPolicyState>, boolean]>([
    [{}, false],
    [{ keepDaily: "0" }, false],
    [{ keepTags: [" "] }, false],
    [{ keepTags: ["", "  "] }, false],
    [{ keepDaily: "1.5" }, false],
    [{ keepTags: [" keep "] }, true],
    [{ keepWithin: "30d" }, true],
    [{ keepLast: "1" }, true],
  ])("%j: %s", (p, want) => expect(hasRetentionRule(policy(p))).toBe(want));
});

describe("retentionPolicyProblem", () => {
  it.each<[Partial<RetentionPolicyState>, RegExp]>([
    [{ keepWithin: "0d" }, /each number is at least 1/],
    [{ keepWithin: "01d" }, /without leading zeros/],
    [{ keepTags: [" "] }, /at least one rule/],
    [{}, /at least one rule/],
    [{ keepDaily: "1.5" }, /Keep daily must be a whole number from 0 to 100000/],
    [{ keepLast: String(MAX_KEEP + 1) }, /Keep last must be a whole number/],
    [{ keepTags: ["a,b"] }, /comma/],
    [{ keepTags: ["x".repeat(129)] }, /at most 128 bytes/],
    [{ keepTags: ["é".repeat(65)] }, /at most 128 bytes/], // 130 bytes in UTF-8
    [{ keepTags: ["tab\there"] }, /control characters/],
    [{ keepTags: Array.from({ length: 33 }, (_, i) => `t${i}`) }, /at most 32 tags/],
  ])("%j", (p, want) => expect(retentionPolicyProblem(policy(p))).toMatch(want));

  it.each<Partial<RetentionPolicyState>>([
    { keepWithin: "1d1y" },
    { keepWithin: "2y5m7d3h" },
    { keepDaily: "7", keepWeekly: "4" },
    { keepLast: String(MAX_KEEP) },
    { keepDaily: "0", keepTags: [" keep ", ""] },
    { keepTags: ["é".repeat(64)] },
    { keepTags: Array.from({ length: 32 }, (_, i) => `t${i}`) },
  ])("accepts %j", (p) => expect(retentionPolicyProblem(policy(p))).toBeUndefined());
});

describe("tags and host", () => {
  it("cleans tags like what is sent", () => {
    expect(cleanTags([" a ", "", "a", "b", "  "])).toEqual(["a", "b"]);
    expect(tagsProblem([])).toBeUndefined();
  });
  it("checks the host like the controller", () => {
    expect(hostProblem("")).toBeUndefined();
    expect(hostProblem("nas-01")).toBeUndefined();
    expect(hostProblem("h".repeat(256))).toMatch(/255 bytes/);
    expect(hostProblem("a\nb")).toMatch(/control characters/);
  });
  it("describes only what is sent", () => {
    expect(describeRetention(policy({ keepTags: [" legal ", " "] }))).toBe(
      "Keeps snapshots tagged legal.",
    );
    expect(describeRetention(policy({ keepDaily: "1.5" }))).toBe(
      "Keeps nothing yet: set at least one rule.",
    );
  });
});
