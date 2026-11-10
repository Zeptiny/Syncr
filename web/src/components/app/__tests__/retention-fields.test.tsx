import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it } from "vitest";

import {
  defaultRetention,
  describeRetention,
  emptyRetention,
  hasRetentionRule,
  matchingPreset,
  retentionPresets,
  validRetentionDuration,
  type RetentionPolicyState,
} from "@/lib/retention";

import { RetentionFields } from "../retention-fields";

function Harness({ initial, error }: { initial: RetentionPolicyState; error?: string }) {
  const [v, setV] = useState(initial);
  return (
    <>
      <RetentionFields idPrefix="keep" value={v} onChange={setV} error={error} />
      <pre data-testid="state">{JSON.stringify(v)}</pre>
    </>
  );
}

const state = () =>
  JSON.parse(screen.getByTestId("state").textContent ?? "{}") as RetentionPolicyState;

describe("describeRetention", () => {
  it.each<[Partial<RetentionPolicyState>, string]>([
    [{}, "Keeps nothing yet: set at least one rule."],
    [{ keepLast: "30" }, "Keeps the last 30 snapshots."],
    [
      { keepDaily: "7", keepWeekly: "4", keepMonthly: "12" },
      "Keeps 7 daily, 4 weekly and 12 monthly snapshots.",
    ],
    [{ keepHourly: "24", keepDaily: "7" }, "Keeps 24 hourly and 7 daily snapshots."],
    [{ keepWithin: "30d" }, "Keeps everything from the last 30d."],
    [
      { keepYearly: "2", keepWithin: "1y6m", keepTags: ["keep", "legal"] },
      "Keeps 2 yearly snapshots, everything from the last 1y6m and snapshots tagged keep, legal.",
    ],
    [{ keepDaily: "0", keepWeekly: "" }, "Keeps nothing yet: set at least one rule."],
  ])("%j", (policy, want) => {
    expect(describeRetention({ ...emptyRetention(), ...policy })).toBe(want);
  });

  it("knows the presets, the default and what counts as a rule", () => {
    expect(retentionPresets.map((p) => p.label)).toEqual([
      "7 daily, 4 weekly, 12 monthly",
      "Last 30",
      "24 hourly, 7 daily, 4 weekly",
    ]);
    expect(matchingPreset(defaultRetention())).toBe("daily-weekly-monthly");
    expect(matchingPreset({ ...defaultRetention(), keepLast: "1" })).toBeUndefined();
    expect(hasRetentionRule(emptyRetention())).toBe(false);
    expect(hasRetentionRule({ ...emptyRetention(), keepTags: ["x"] })).toBe(true);
    expect(["30d", "1y6m", "12h", "2y3m4d5h"].every(validRetentionDuration)).toBe(true);
    expect(["", "30", "d", "two weeks", "6m1y"].some(validRetentionDuration)).toBe(false);
  });
});

describe("RetentionFields", () => {
  it("applies a preset and marks the one that matches", async () => {
    const user = userEvent.setup();
    render(<Harness initial={defaultRetention()} />);
    const first = screen.getByRole("button", { name: "7 daily, 4 weekly, 12 monthly" });
    expect(first).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("status")).toHaveTextContent(
      "Keeps 7 daily, 4 weekly and 12 monthly snapshots.",
    );

    await user.click(screen.getByRole("button", { name: "Last 30" }));
    expect(state()).toMatchObject({ keepLast: "30", keepDaily: "", keepWeekly: "" });
    expect(screen.getByRole("button", { name: "Last 30" })).toHaveAttribute("aria-pressed", "true");
    expect(first).toHaveAttribute("aria-pressed", "false");

    await user.click(screen.getByRole("button", { name: "24 hourly, 7 daily, 4 weekly" }));
    expect(state()).toMatchObject({
      keepLast: "",
      keepHourly: "24",
      keepDaily: "7",
      keepWeekly: "4",
    });
  });

  it("edits single rules, digits only, and describes the result", async () => {
    const user = userEvent.setup();
    render(<Harness initial={emptyRetention()} />);
    expect(screen.getByRole("status")).toHaveTextContent("Keeps nothing yet");
    await user.type(screen.getByLabelText("Keep yearly"), "3x");
    await user.type(screen.getByLabelText("Keep everything from the last"), "30d");
    await user.type(screen.getByLabelText("Always keep snapshots tagged"), "legal{Enter}");
    expect(state()).toMatchObject({ keepYearly: "3", keepWithin: "30d", keepTags: ["legal"] });
    expect(screen.getByRole("status")).toHaveTextContent(
      "Keeps 3 yearly snapshots, everything from the last 30d and snapshots tagged legal.",
    );
    // No preset matches a hand-made policy.
    for (const p of retentionPresets)
      expect(screen.getByRole("button", { name: p.label })).toHaveAttribute(
        "aria-pressed",
        "false",
      );
  });

  it("shows a policy error in place of the summary", () => {
    render(<Harness initial={emptyRetention()} error="Set at least one rule." />);
    expect(screen.getByRole("alert")).toHaveTextContent("Set at least one rule.");
  });
});
