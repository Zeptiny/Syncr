import { create } from "@bufbuild/protobuf";
import { durationFromMs } from "@bufbuild/protobuf/wkt";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import {
  FieldViolationSchema,
  JobDefinitionSchema,
  type RunJobRequest,
} from "@/gen/syncr/v1/api_pb";
import type { CreateScheduleRequest } from "@/gen/syncr/v1/schedules_pb";
import * as m5 from "@/mock/m5";
import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { renderApp } from "@/test/render";

import {
  definitionError,
  definitionState,
  emptyDefinition,
  toDefinition,
  validateDefinition,
  withKind,
  type DefinitionErrors,
  type JobDefinitionState,
} from "./definition";
import { timeLimitHelp } from "./job-definition-editor";

const now = Date.now();
const H = 3_600_000;

describe("time limit in the definition", () => {
  it.each([
    [90 * 60_000, "90", "min"],
    [6 * H, "6", "h"],
    [2 * 24 * H, "2", "d"],
    [90_000, "1.5", "min"],
  ])("shows a stored %d ms as %s %s and sends it back unchanged", (ms, text, unit) => {
    const def = create(JobDefinitionSchema, {
      maxRuntime: durationFromMs(ms),
      operation: { case: "resticCheck", value: { repositoryId: "r" } },
    });
    const s = definitionState(def);
    expect(s.timeLimit).toEqual({ text, unit });
    expect(create(JobDefinitionSchema, toDefinition(s)).maxRuntime).toEqual(def.maxRuntime);
  });

  it("sends no max_runtime when empty, for every kind", () => {
    for (const kind of ["rclone_transfer", "restic_backup", "restic_prune"] as const) {
      const s = emptyDefinition(kind);
      expect(s.timeLimit.text).toBe("");
      expect(toDefinition(s).maxRuntime).toBeUndefined();
    }
  });

  it("keeps the time limit when the kind changes", () => {
    const s: JobDefinitionState = {
      ...emptyDefinition("restic_backup", "r"),
      timeLimit: { text: "3", unit: "h" },
    };
    expect(withKind(s, "restic_check").timeLimit).toEqual({ text: "3", unit: "h" });
  });

  it.each([
    ["0.5", "min", "at least 1 minute"],
    ["367", "d", "at most 366 days"],
    ["abc", "h", "Enter a number"],
  ] as const)("refuses %s %s", (text, unit, message) => {
    const s = { ...emptyDefinition("restic_check", "r"), timeLimit: { text, unit } };
    expect(validateDefinition(s).timeLimit).toMatch(message);
  });

  it("accepts the bounds", () => {
    for (const [text, unit] of [
      ["1", "min"],
      ["366", "d"],
    ] as const) {
      const s = { ...emptyDefinition("restic_check", "r"), timeLimit: { text, unit } };
      expect(validateDefinition(s).timeLimit).toBeUndefined();
    }
  });

  it("places the API's max_runtime error on the field", () => {
    const errors: DefinitionErrors = {};
    const v = create(FieldViolationSchema, {
      field: "definition.max_runtime",
      message: "must be between 1 minute and 366 days",
    });
    expect(definitionError(v, errors)).toBe(true);
    expect(errors.timeLimit).toBe("must be between 1 minute and 366 days");
  });

  it("explains how a transfer's time limit differs from rclone's max duration", () => {
    expect(timeLimitHelp("rclone_transfer")).toMatch(/only stops new transfers from starting/);
    expect(timeLimitHelp("rclone_transfer")).toMatch(/fails/);
    expect(timeLimitHelp("restic_backup")).not.toMatch(/max duration/);
  });
});

describe("time limit in the forms", { timeout: 30_000 }, () => {
  it("runs a check with a time limit", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    renderApp(`/jobs/new?kind=restic_check&repository=${m5.ids.photosVault}`, api.transport);
    const field = await screen.findByRole("textbox", { name: /^Stop the job after/ });
    expect(screen.getByText(/A hard stop: once the job has run this long/)).toBeVisible();
    await user.click(screen.getByRole("combobox", { name: /^Worker/ }));
    await user.click(await screen.findByRole("option", { name: /nas-01/ }));

    await user.type(field, "30");
    await user.click(screen.getByRole("combobox", { name: "Stop the job after unit" }));
    // Minutes, hours and days: never seconds.
    expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual([
      "min",
      "hours",
      "days",
    ]);
    await user.click(screen.getByRole("option", { name: "min" }));
    await user.click(screen.getByRole("button", { name: "Run job" }));
    await waitFor(() => expect(api.of("RunJob")).toHaveLength(1));
    const req = api.of<RunJobRequest>("RunJob")[0];
    expect(req.definition?.maxRuntime).toMatchObject({ seconds: 1800n });
  });

  it("refuses a time limit under a minute before sending", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    renderApp(`/jobs/new?kind=restic_check&repository=${m5.ids.photosVault}`, api.transport);
    await user.click(await screen.findByRole("combobox", { name: /^Worker/ }));
    await user.click(await screen.findByRole("option", { name: /nas-01/ }));
    await user.type(screen.getByRole("textbox", { name: /^Stop the job after/ }), "0");
    await user.click(screen.getByRole("button", { name: "Run job" }));
    expect(await screen.findByText("A time limit must be at least 1 minute.")).toBeVisible();
    expect(api.of("RunJob")).toHaveLength(0);
  });

  it("stores the time limit with a schedule's definition", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    renderApp(`/schedules/new?kind=restic_prune&repository=${m5.ids.photosVault}`, api.transport);
    await user.type(await screen.findByLabelText("Name"), "Monthly prune");
    await user.click(screen.getByRole("combobox", { name: /^Worker/ }));
    await user.click(await screen.findByRole("option", { name: /nas-01/ }));
    await user.type(screen.getByRole("textbox", { name: /^Stop the job after/ }), "12");
    await user.click(screen.getByRole("button", { name: "Create schedule" }));
    await waitFor(() => expect(api.of("CreateSchedule")).toHaveLength(1));
    const input = api.of<CreateScheduleRequest>("CreateSchedule")[0].input;
    expect(input?.definition?.maxRuntime).toMatchObject({ seconds: 12n * 3600n });
    expect(input?.target?.target).toEqual({ case: "workerId", value: fx.ids.nas });
  });
});
