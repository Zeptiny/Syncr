import { describe, expect, it } from "vitest";

import { cronText, describeCron } from "./cron";

describe("describeCron", () => {
  it.each([
    ["30 2 * * *", "At 02:30"],
    ["0 * * * *", "Every hour"],
    ["0 18 * * 1-5", "At 18:00, Monday through Friday"],
    ["@daily", "At 00:00"],
    ["  */15   * * * * ", "Every 15 minutes"],
  ])("describes %j", (expr, text) => {
    expect(describeCron(expr)).toEqual({ ok: true, text });
  });

  it.each([
    ["", "Enter a schedule"],
    ["0 0 * * * *", "Seconds are not supported"],
    ["* *", "Use five fields"],
    ["@every 5m", "@every are not supported"],
    ["CRON_TZ=UTC 0 0 * * *", "time zone below"],
    ["@sometimes", "not a known shortcut"],
    ["61 * * * *", "minutes part"],
  ])("rejects %j", (expr, msg) => {
    const d = describeCron(expr);
    expect(d.ok).toBe(false);
    if (!d.ok) expect(d.error).toContain(msg);
  });

  it("falls back to the expression", () => {
    expect(cronText("nope")).toBe("nope");
  });
});
