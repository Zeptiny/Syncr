import { describe, expect, it } from "vitest";

import { labelProblem, normalizeLabels } from "./labels";

describe("labels", () => {
  it.each([
    ["nas", undefined],
    ["a", undefined],
    ["eu-west.1_b", undefined],
    ["", "Enter a label."],
    ["-nas", expect.stringContaining("Start with")],
    ["Nas", expect.stringContaining("Start with")],
    ["has space", expect.stringContaining("Start with")],
    ["x".repeat(33), "Use at most 32 characters."],
  ])("%j → %s", (label, want) => {
    expect(labelProblem(label)).toEqual(want);
  });

  it("normalizes like the controller", () => {
    expect(normalizeLabels([" NAS", "linux", "nas", "", "Linux "])).toEqual(["linux", "nas"]);
  });
});
