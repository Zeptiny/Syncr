import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";

import { JobStatus } from "@/gen/syncr/v1/api_pb";
import { ActivityGroupSchema, ScheduleHealthSchema } from "@/gen/syncr/v1/dashboard_pb";
import { ScheduleRunSchema, ScheduleRunStatus } from "@/gen/syncr/v1/schedules_pb";
import { durationFromSeconds } from "@/lib/format";

import { formatRate, groupStats, runState, scheduleCounts, signedBytes } from "./breakdowns";

describe("groupStats", () => {
  it("averages over the jobs that ran or waited only", () => {
    const s = groupStats(
      create(ActivityGroupSchema, {
        succeeded: 3n,
        failed: 1n,
        cancelled: 2n,
        bytes: 10n,
        runTime: durationFromSeconds(90),
        started: 3n,
        queueWait: durationFromSeconds(4),
        dispatched: 2n,
      }),
    );
    expect(s).toMatchObject({ jobs: 6, successRate: 0.75, bytes: 10, avgRun: 30, avgWait: 2 });
  });

  it("leaves rates and averages unset without jobs to count", () => {
    const s = groupStats(create(ActivityGroupSchema, { cancelled: 2n }));
    expect(s).toMatchObject({
      jobs: 2,
      successRate: undefined,
      avgRun: undefined,
      avgWait: undefined,
    });
  });
});

describe("formatRate", () => {
  it.each([
    [undefined, "–"],
    [1, "100%"],
    [0, "0%"],
    [0.5, "50%"],
    [0.9523, "95.2%"],
    [0.99961, "100%"],
  ])("%s → %s", (rate, want) => expect(formatRate(rate)).toBe(want));
});

describe("runState", () => {
  const run = (status: ScheduleRunStatus, jobStatus = JobStatus.UNSPECIFIED) =>
    create(ScheduleRunSchema, { status, jobStatus });
  it.each([
    [run(ScheduleRunStatus.PENDING), "Starting", false],
    [run(ScheduleRunStatus.MISSED), "Missed", true],
    [run(ScheduleRunStatus.FAILED), "Didn't start", true],
    [run(ScheduleRunStatus.CANCELLED), "Cancelled", true],
    [run(ScheduleRunStatus.LAUNCHED, JobStatus.RUNNING), "Running", false],
    [run(ScheduleRunStatus.LAUNCHED, JobStatus.SUCCEEDED), "Succeeded", false],
    [run(ScheduleRunStatus.LAUNCHED, JobStatus.FAILED), "Failed", true],
    [run(ScheduleRunStatus.LAUNCHED, JobStatus.INTERRUPTED), "Interrupted", true],
  ])("%#: %s", (r, label, problem) => {
    expect(runState(r)).toMatchObject({ meta: { label }, problem });
  });

  it("knows nothing of a launched run without a job, or of no run", () => {
    expect(runState(run(ScheduleRunStatus.LAUNCHED))).toBeUndefined();
    expect(runState(undefined)).toBeUndefined();
  });
});

describe("scheduleCounts", () => {
  it("counts a failing paused schedule as failing, not paused", () => {
    const launched = (jobStatus: JobStatus) =>
      create(ScheduleRunSchema, { status: ScheduleRunStatus.LAUNCHED, jobStatus });
    const counts = scheduleCounts([
      create(ScheduleHealthSchema, { enabled: true, latestRun: launched(JobStatus.SUCCEEDED) }),
      create(ScheduleHealthSchema, { enabled: false, latestRun: launched(JobStatus.FAILED) }),
      create(ScheduleHealthSchema, { enabled: false, latestRun: launched(JobStatus.SUCCEEDED) }),
      create(ScheduleHealthSchema, { enabled: true }),
    ]);
    expect(counts).toEqual({ healthy: 1, failing: 1, paused: 1, neverRan: 1 });
  });
});

describe("signedBytes", () => {
  it("signs the change", () => {
    expect(signedBytes(0)).toBe("no change");
    expect(signedBytes(200 * 1024 ** 2)).toBe("+200 MiB");
    expect(signedBytes(-2 * 1024 ** 3)).toBe("−2.00 GiB");
  });
});
