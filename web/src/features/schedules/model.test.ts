import { create } from "@bufbuild/protobuf";
import { durationFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";

import { ValidationErrorSchema } from "@/gen/syncr/v1/api_pb";
import { NotificationEvent } from "@/gen/syncr/v1/notify_pb";
import { MissedRunAction, MissedRunPolicySchema, OverlapPolicy } from "@/gen/syncr/v1/schedules_pb";
import * as fx from "@/test/fixtures";

import {
  emptySchedule,
  missedFromProto,
  missedSummary,
  missedToProto,
  scheduleFromJob,
  scheduleServerErrors,
  scheduleState,
  toScheduleInput,
  validateSchedule,
} from "./model";

const now = Date.parse("2026-11-08T12:00:00Z");
const HOUR = 3600;

describe("missed-run policy mapping", () => {
  it.each([
    ["unset", undefined, { action: "run_late", within: 0 }],
    [
      "run late, 6h",
      create(MissedRunPolicySchema, {
        action: MissedRunAction.RUN_LATE,
        maxDelay: durationFromMs(6 * HOUR * 1000),
      }),
      { action: "run_late", within: 6 * HOUR },
    ],
    [
      "run late, zero means until next",
      create(MissedRunPolicySchema, {
        action: MissedRunAction.RUN_LATE,
        maxDelay: durationFromMs(0),
      }),
      { action: "run_late", within: 0 },
    ],
    [
      "unspecified action runs late",
      create(MissedRunPolicySchema, { maxDelay: durationFromMs(15 * 60_000) }),
      { action: "run_late", within: 15 * 60 },
    ],
    [
      "skip",
      create(MissedRunPolicySchema, { action: MissedRunAction.SKIP }),
      { action: "skip", within: 0 },
    ],
  ])("reads %s", (_name, proto, want) => {
    expect(missedFromProto(proto)).toEqual(want);
  });

  it("writes until-next as an unset max_delay and skip without one", () => {
    expect(missedToProto({ action: "run_late", within: 0 })).toEqual({
      action: MissedRunAction.RUN_LATE,
      maxDelay: undefined,
    });
    const late = missedToProto({ action: "run_late", within: HOUR });
    expect(late.action).toBe(MissedRunAction.RUN_LATE);
    expect(Number(late.maxDelay?.seconds)).toBe(HOUR);
    expect(missedToProto({ action: "skip", within: HOUR })).toEqual({
      action: MissedRunAction.SKIP,
    });
  });

  it("round-trips every preset", () => {
    for (const within of [900, HOUR, 6 * HOUR, 12 * HOUR, 24 * HOUR, 0]) {
      const m = { action: "run_late" as const, within };
      const proto = create(MissedRunPolicySchema, missedToProto(m));
      expect(missedFromProto(proto)).toEqual(m);
    }
  });

  it("summarizes a policy", () => {
    expect(missedSummary(undefined)).toBe("Runs late, until the next run is due");
    expect(
      missedSummary(
        create(MissedRunPolicySchema, {
          action: MissedRunAction.RUN_LATE,
          maxDelay: durationFromMs(6 * HOUR * 1000),
        }),
      ),
    ).toBe("Runs late, within 6 hours");
    expect(missedSummary(create(MissedRunPolicySchema, { action: MissedRunAction.SKIP }))).toBe(
      "Skipped if the worker is offline",
    );
  });
});

describe("schedule form model", () => {
  it("builds the input with a label target, policies and contacts", () => {
    const s = emptySchedule("Europe/Berlin");
    s.name = "  Nightly  ";
    s.cron = " 30  2 * * * ";
    s.target = { kind: "label", workerId: "", label: " NAS " };
    s.missed = { action: "run_late", within: 0 };
    s.overlap = OverlapPolicy.QUEUE;
    s.contacts = [{ contactId: fx.ids.opsDiscord, events: [NotificationEvent.FAILED] }];
    s.definition.source = { remoteId: fx.ids.photos, path: "2026" };
    s.definition.destination = { remoteId: fx.ids.b2, path: "photos" };
    expect(validateSchedule(s)).toEqual({});
    const input = toScheduleInput(s);
    expect(input).toMatchObject({
      name: "Nightly",
      cron: "30 2 * * *",
      timezone: "Europe/Berlin",
      target: { target: { case: "workerLabel", value: "nas" } },
      missedRun: { action: MissedRunAction.RUN_LATE, maxDelay: undefined },
      overlap: OverlapPolicy.QUEUE,
      enabled: true,
      contacts: [{ contactId: fx.ids.opsDiscord, events: [NotificationEvent.FAILED] }],
    });
    expect(input.definition?.operation?.case).toBe("rcloneTransfer");
  });

  it("checks name, target, contacts and paths", () => {
    const s = emptySchedule("UTC");
    s.contacts = [{ contactId: fx.ids.adminMail, events: [] }];
    const e = validateSchedule(s);
    expect(e.name).toBeDefined();
    expect(e.target).toBe("Choose the worker that runs the jobs.");
    expect(e.contacts?.[0]).toContain("at least one event");
    expect(e.sourcePath).toBeDefined();
    s.target = { kind: "label", workerId: "", label: "" };
    expect(validateSchedule(s).target).toBe("Enter a worker label.");
  });

  it("round-trips a stored schedule", () => {
    const stored = fx.history(now).schedules.find((x) => x.id === fx.ids.nightly);
    if (!stored) throw new Error("no fixture");
    const s = scheduleState(stored);
    expect(s.target).toEqual({ kind: "label", workerId: "", label: "nas" });
    expect(s.missed).toEqual({ action: "run_late", within: 6 * HOUR });
    expect(toScheduleInput(s)).toMatchObject({
      name: stored.name,
      cron: stored.cron,
      timezone: stored.timezone,
      overlap: stored.overlap,
    });
  });

  it("starts a schedule from a job on its worker", () => {
    const job = fx.jobs(now)[3];
    const s = scheduleFromJob(job, "UTC");
    expect(s.target).toEqual({ kind: "worker", workerId: job.workerId, label: "" });
    expect(s.definition.source.path).toBe("archive/2025");
  });

  it("places server violations on their sections", () => {
    const err = new ConnectError("invalid", Code.InvalidArgument, undefined, [
      {
        desc: ValidationErrorSchema,
        value: {
          violations: [
            { field: "cron", message: "bad cron" },
            { field: "target.worker_label", message: "bad label" },
            { field: "missed_run.max_delay", message: "too long" },
            { field: "contacts[1].events", message: "pick one" },
            { field: "definition.rclone_transfer.source.path", message: "bad path" },
            { field: "something", message: "else" },
          ],
        },
      },
    ]);
    const { errors, rest } = scheduleServerErrors(err);
    expect(errors).toMatchObject({
      cron: "bad cron",
      target: "bad label",
      missed: "too long",
      contacts: { 1: "pick one" },
      sourcePath: "bad path",
    });
    expect(rest).toEqual(["something: else"]);
  });
});
