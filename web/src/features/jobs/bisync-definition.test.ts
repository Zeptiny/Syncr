import { clone, create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { describe, expect, it } from "vitest";

import {
  FieldViolationSchema,
  JobDefinitionSchema,
  JobSchema,
  JobStatus,
  type Job,
  type JobDefinition,
} from "@/gen/syncr/v1/api_pb";
import { ScheduleSchema } from "@/gen/syncr/v1/schedules_pb";
import {
  BisyncConflictLoser,
  BisyncConflictResolve,
  BisyncResyncMode,
  FilterAction,
  FilterRuleSchema,
  JobErrorCode,
} from "@/gen/syncr/v1/link_pb";
import { errorCodeLabel, jobKinds, kindLabel } from "@/lib/status";
import * as bisyncMock from "@/mock/bisync";

import {
  bisyncError,
  bisyncOf,
  bisyncState,
  conflictsSummary,
  emptyBisync,
  filtersNeedResync,
  planRecovery,
  recoveryFor,
  samePair,
  sameSettings,
  sidesOverlap,
  validateBisync,
  withOneShot,
  withoutOneShot,
  type BisyncState,
} from "./bisync-definition";
import {
  definitionError,
  definitionState,
  editableKinds,
  emptyDefinition,
  scheduleKinds,
  toDefinition,
  withKind,
  type DefinitionErrors,
} from "./definition";
import { formKind } from "./search";

const pair = (over: Partial<BisyncState> = {}): BisyncState => ({
  ...emptyBisync(),
  path1: { remoteId: "", path: "/srv/shared" },
  path2: { remoteId: "r2", path: "shared" },
  ...over,
});

describe("two-way sync form", () => {
  it("is a kind the forms and schedules offer, named Two-way sync", () => {
    expect(editableKinds[1]).toBe("rclone_bisync");
    expect(scheduleKinds).toContain("rclone_bisync");
    expect(formKind("rclone_bisync")).toBe("rclone_bisync");
    expect(jobKinds).toContain("rclone_bisync");
    expect(kindLabel("rclone_bisync")).toBe("Two-way sync");
  });

  it("labels the two-way sync error codes", () => {
    expect(errorCodeLabel(JobErrorCode.RESYNC_REQUIRED)).toBe("Resync required");
    expect(errorCodeLabel(JobErrorCode.SAFETY_ABORT)).toBe("Safety check stopped the sync");
    expect(errorCodeLabel(JobErrorCode.SIDE_UNAVAILABLE)).toBe("A folder looked unavailable");
  });

  it("sends Syncr's defaults unset, and never resync or force", () => {
    const def = create(JobDefinitionSchema, toDefinition({ ...emptyDefinition("rclone_bisync") }));
    const b = bisyncOf(def)!;
    expect(b.resync).toBe(false);
    expect(b.force).toBe(false);
    expect(b.options).toMatchObject({
      conflictResolve: BisyncConflictResolve.UNSPECIFIED,
      conflictLoser: BisyncConflictLoser.UNSPECIFIED,
      resyncMode: BisyncResyncMode.UNSPECIFIED,
      compare: [],
    });
    expect(b.options?.maxDeletePercent).toBeUndefined();
    // Unset values show as the defaults they mean.
    const s = bisyncState(b);
    expect(s.conflictResolve).toBe(BisyncConflictResolve.NONE);
    expect(s.conflictLoser).toBe(BisyncConflictLoser.NUM);
    expect(s.resyncMode).toBe(BisyncResyncMode.NEWER);
    expect(s.compare).toEqual({ size: true, modtime: true, checksum: false });
  });

  it("drops a stored job's resync or force when the form loads it", () => {
    const s = definitionState(bisyncMock.definition("resync"));
    expect(s.kind).toBe("rclone_bisync");
    const again = bisyncOf(create(JobDefinitionSchema, toDefinition(s)))!;
    expect(again.resync).toBe(false);
    expect(again.path1?.path).toBe("/srv/shared");
    expect(again.options?.filters.map((f) => f.pattern)).toEqual(["*.tmp", ".~lock.*#"]);
  });

  it("keeps the locations and filters between a transfer and a two-way sync", () => {
    const t = emptyDefinition("rclone_transfer");
    if (t.kind !== "rclone_transfer") throw new Error("kind");
    t.transfer.source = { remoteId: "", path: "/srv/a" };
    t.transfer.destination = { remoteId: "r1", path: "b" };
    t.transfer.filters = [{ action: FilterAction.EXCLUDE, pattern: "*.tmp" }];
    const b = withKind(t, "rclone_bisync");
    expect(b).toMatchObject({
      kind: "rclone_bisync",
      bisync: {
        path1: t.transfer.source,
        path2: t.transfer.destination,
        filters: t.transfer.filters,
      },
    });
    expect(withKind(b, "rclone_transfer")).toMatchObject({
      transfer: { source: t.transfer.source, destination: t.transfer.destination },
    });
  });

  it.each([
    ["/srv/a", "/srv/b", false],
    ["/srv/a", "/srv/a", true],
    ["/srv/a", "/srv/a/", true],
    ["/srv/a", "/srv/a/b", true],
    ["/srv/a/b", "/srv/a", true],
    ["/srv/ab", "/srv/a", false],
    ["C:\\Data", "c:/data/sub", true],
    ["C:\\Data", "/Data", false],
  ])("local %s and %s overlap: %s", (a, b, want) => {
    expect(sidesOverlap({ remoteId: "", path: a }, { remoteId: "", path: b })).toBe(want);
  });

  it("treats a remote's root as holding all its folders, and other remotes as apart", () => {
    expect(sidesOverlap({ remoteId: "r", path: "" }, { remoteId: "r", path: "x" })).toBe(true);
    expect(sidesOverlap({ remoteId: "r", path: "/x/" }, { remoteId: "r", path: "x" })).toBe(true);
    expect(sidesOverlap({ remoteId: "r", path: "x" }, { remoteId: "q", path: "x" })).toBe(false);
  });

  it("checks what the browser can, like the kind's Check", () => {
    expect(validateBisync(pair())).toEqual({});
    expect(validateBisync(pair({ path1: { remoteId: "", path: "srv" } }))).toHaveProperty(
      "path1Path",
    );
    expect(
      validateBisync(pair({ path2: { remoteId: "", path: "/srv/shared/sub" } })),
    ).toHaveProperty("path2Path");
    const long = "x".repeat(120);
    expect(
      validateBisync(
        pair({ path1: { remoteId: "", path: `/${long}` }, path2: { remoteId: "r", path: long } }),
      ),
    ).toHaveProperty("path1Path");
    expect(
      validateBisync(
        pair({
          maxDelete: "0",
          conflictSuffix: "bad suffix",
          compare: { size: false, modtime: false, checksum: false },
          transfers: "65",
          checkers: "0",
          lowLevelRetries: "1001",
          bwlimit: "-1",
        }),
      ),
    ).toSatisfy((e: object) =>
      [
        "maxDelete",
        "conflictSuffix",
        "compare",
        "transfers",
        "checkers",
        "lowLevelRetries",
        "bwlimit",
      ].every((k) => k in e && Object.keys(e).length === 7),
    );
    expect(
      validateBisync(pair({ maxDelete: "100", conflictSuffix: "a.b,c-d", lowLevelRetries: "0" })),
    ).toEqual({});
  });

  it("places the server's violations on their fields", () => {
    const errors: DefinitionErrors = {};
    const put = (field: string) =>
      definitionError(create(FieldViolationSchema, { field, message: field }), errors);
    for (const f of [
      "definition.rclone_bisync.path1.remote_id",
      "definition.rclone_bisync.path1.path",
      "definition.rclone_bisync.path2",
      "definition.rclone_bisync.options.max_delete_percent",
      "definition.rclone_bisync.options.conflict_suffix",
      "definition.rclone_bisync.options.conflict_loser",
      "definition.rclone_bisync.options.compare[1]",
      "definition.rclone_bisync.options.resync_mode",
      "definition.rclone_bisync.options.filters[0].pattern",
      "definition.rclone_bisync.options.transfers",
      "definition.rclone_bisync.options.bandwidth_limit_bytes_per_second",
      "definition.rclone_bisync.resync",
    ])
      expect(put(f)).toBe(true);
    expect(errors).toEqual({
      path1Remote: "definition.rclone_bisync.path1.remote_id",
      path1Path: "definition.rclone_bisync.path1.path",
      path2Path: "definition.rclone_bisync.path2",
      maxDelete: "definition.rclone_bisync.options.max_delete_percent",
      conflictSuffix: "definition.rclone_bisync.options.conflict_suffix",
      conflicts: "definition.rclone_bisync.options.conflict_loser",
      compare: "definition.rclone_bisync.options.compare[1]",
      resyncMode: "definition.rclone_bisync.options.resync_mode",
      filters: "definition.rclone_bisync.options.filters[0].pattern",
      transfers: "definition.rclone_bisync.options.transfers",
      bwlimit: "definition.rclone_bisync.options.bandwidth_limit_bytes_per_second",
      options: "definition.rclone_bisync.resync",
    });
    expect(
      bisyncError(
        create(FieldViolationSchema, { field: "definition.rclone_transfer.source.path" }),
      ),
    ).toBeUndefined();
  });

  it("says when changed filters need a resync: same sides, other rules", () => {
    const saved = pair({ filters: [{ action: FilterAction.EXCLUDE, pattern: "*.tmp" }] });
    expect(filtersNeedResync(saved, saved)).toBe(false);
    // Empty rows and spaces are not rules.
    expect(
      filtersNeedResync(
        saved,
        pair({
          filters: [
            { action: FilterAction.EXCLUDE, pattern: " *.tmp " },
            { action: FilterAction.INCLUDE, pattern: "" },
          ],
        }),
      ),
    ).toBe(false);
    expect(filtersNeedResync(saved, pair({ filters: [] }))).toBe(true);
    expect(
      filtersNeedResync(
        saved,
        pair({ filters: [{ action: FilterAction.INCLUDE, pattern: "*.tmp" }] }),
      ),
    ).toBe(true);
    // Another side is another pair: its first run resyncs on its own.
    expect(
      filtersNeedResync(saved, pair({ path2: { remoteId: "r2", path: "other" }, filters: [] })),
    ).toBe(false);
  });
});

describe("recovery plan", () => {
  const MIN = 60_000;
  const now = Date.UTC(2026, 10, 12, 12);
  const NAS = "nas";
  const at = (ms: number) => timestampFromMs(ms);
  const failed = (code: JobErrorCode, over: { scheduleId?: string } = {}): Job =>
    create(JobSchema, {
      id: "failed",
      workerId: NAS,
      kind: "rclone_bisync",
      status: JobStatus.FAILED,
      createdAt: at(now - 60 * MIN),
      definition: bisyncMock.definition(),
      error: { code, message: "too many deletes (>50%, 731 of 1268)" },
      ...over,
    });
  const later = (id: string, def: JobDefinition, minutesAgo: number, workerId = NAS) =>
    create(JobSchema, {
      id,
      workerId,
      kind: "rclone_bisync",
      createdAt: at(now - minutesAgo * MIN),
      definition: def,
    });
  const schedule = (def: JobDefinition = bisyncMock.definition(), workerId = NAS) =>
    create(ScheduleSchema, {
      id: "s1",
      definition: def,
      target: { target: { case: "workerId", value: workerId } },
    });
  const withFilter = () => {
    const d = bisyncMock.definition();
    bisyncOf(d)!.options!.filters.push(
      create(FilterRuleSchema, { action: FilterAction.EXCLUDE, pattern: "/photos/**" }),
    );
    return d;
  };
  const none = { jobs: [] as Job[], complete: true };

  it("compares pairs by their two locations and settings without the one-shot fields", () => {
    expect(samePair(bisyncMock.definition(), withFilter())).toBe(true);
    expect(
      samePair(bisyncMock.definition(), bisyncMock.definition(undefined, "/srv/shared/")),
    ).toBe(true);
    expect(samePair(bisyncMock.definition(), bisyncMock.definition(undefined, "/srv/other"))).toBe(
      false,
    );
    expect(samePair(bisyncMock.definition(), create(JobDefinitionSchema, {}))).toBe(false);
    expect(sameSettings(bisyncMock.definition("force"), bisyncMock.definition())).toBe(true);
    expect(sameSettings(bisyncMock.definition(), withFilter())).toBe(false);
  });

  it("runs a manual job's own settings on its worker while it is the pair's newest", () => {
    const job = failed(JobErrorCode.RESYNC_REQUIRED);
    expect(planRecovery({ job, from: "job", newer: none })).toEqual({
      recovery: "resync",
      run: { definition: job.definition, workerId: NAS, from: "job" },
    });
    // Not known yet: no action.
    expect(planRecovery({ job, from: "job" })).toMatchObject({ block: { why: "pending" } });
    // Newer jobs of other pairs, or of the pair elsewhere, do not count;
    // one of the pair on this worker does, a resync included.
    const others = [
      later("other-pair", bisyncMock.definition(undefined, "/srv/other"), 10),
      later("elsewhere", bisyncMock.definition(), 10, "office"),
      later("older", bisyncMock.definition(), 90),
    ];
    expect(
      planRecovery({ job, from: "job", newer: { jobs: others, complete: true } })?.run,
    ).toBeDefined();
    const resync = later("resync", bisyncMock.definition("resync"), 30);
    expect(
      planRecovery({ job, from: "job", newer: { jobs: [...others, resync], complete: true } }),
    ).toEqual({ recovery: "resync", block: { why: "newer", jobId: "resync" } });
    // More newer jobs than one page: not known to be the newest.
    expect(planRecovery({ job, from: "job", newer: { jobs: others, complete: false } })).toEqual({
      recovery: "resync",
      block: { why: "newer" },
    });
  });

  it("acts on a scheduled job only while its schedule still has its settings and worker", () => {
    const job = failed(JobErrorCode.SAFETY_ABORT, { scheduleId: "s1" });
    expect(planRecovery({ job, from: "job", schedule: schedule(), newer: none })?.run).toEqual({
      definition: job.definition,
      workerId: NAS,
      from: "job",
    });
    for (const s of [schedule(withFilter()), schedule(bisyncMock.definition(), "office")])
      expect(planRecovery({ job, from: "job", schedule: s, newer: none })).toEqual({
        recovery: "force",
        block: { why: "scheduleChanged", scheduleId: "s1" },
      });
    // A deleted schedule leaves the job on its own.
    expect(planRecovery({ job, from: "job", newer: none })?.run?.from).toBe("job");
  });

  it("runs the schedule's current settings on its current worker from the schedule's page", () => {
    const job = failed(JobErrorCode.RESYNC_REQUIRED, { scheduleId: "s1" });
    const s = schedule(withFilter(), "office");
    expect(planRecovery({ job, from: "schedule", schedule: s, newer: none })).toEqual({
      recovery: "resync",
      run: { definition: s.definition, workerId: "office", from: "schedule" },
    });
    // A newer job of the schedule's pair on its worker supersedes the run.
    const resync = later("resync", clone(JobDefinitionSchema, s.definition!), 5, "office");
    expect(
      planRecovery({
        job,
        from: "schedule",
        schedule: s,
        newer: { jobs: [resync], complete: true },
      })?.block,
    ).toEqual({ why: "newer", jobId: "resync" });
  });

  it("runs past the safety check from the schedule's page only with the settings that stopped", () => {
    const job = failed(JobErrorCode.SAFETY_ABORT, { scheduleId: "s1" });
    expect(planRecovery({ job, from: "schedule", schedule: schedule(), newer: none })?.run).toEqual(
      {
        definition: schedule().definition,
        workerId: NAS,
        from: "schedule",
      },
    );
    for (const s of [schedule(withFilter()), schedule(bisyncMock.definition(), "office")])
      expect(planRecovery({ job, from: "schedule", schedule: s, newer: none })).toEqual({
        recovery: "force",
        block: { why: "settingsChanged" },
      });
  });

  it("offers only a hint after SIDE_UNAVAILABLE, and nothing for other failures", () => {
    expect(planRecovery({ job: failed(JobErrorCode.SIDE_UNAVAILABLE), from: "job" })).toEqual({
      recovery: "hint",
    });
    expect(
      planRecovery({ job: failed(JobErrorCode.EXECUTION_FAILED), from: "job" }),
    ).toBeUndefined();
  });
});

describe("one-shot runs", () => {
  it("sets resync or force for one run, never both", () => {
    const def = bisyncMock.definition();
    expect(bisyncOf(create(JobDefinitionSchema, withOneShot(def, "resync")))).toMatchObject({
      resync: true,
      force: false,
    });
    const forced = bisyncOf(
      create(JobDefinitionSchema, withOneShot(bisyncMock.definition("resync"), "force")),
    );
    expect(forced).toMatchObject({ resync: false, force: true, path1: { path: "/srv/shared" } });
    expect(withOneShot(create(JobDefinitionSchema, {}), "resync")).toBeUndefined();
  });

  it("resyncs for real from a dry-run definition, and keeps a forced run dry", () => {
    const dry = bisyncMock.definition();
    bisyncOf(dry)!.options!.dryRun = true;
    const resync = bisyncOf(create(JobDefinitionSchema, withOneShot(dry, "resync")));
    expect(resync?.options).toMatchObject({ dryRun: false, createEmptySrcDirs: true });
    expect(resync?.options?.filters).toHaveLength(2);
    expect(bisyncOf(create(JobDefinitionSchema, withOneShot(dry, "force")))?.options?.dryRun).toBe(
      true,
    );
    // The definition it came from is left alone.
    expect(bisyncOf(dry)?.options?.dryRun).toBe(true);
  });

  it("runs a one-shot job again as a normal run", () => {
    const again = bisyncOf(
      create(JobDefinitionSchema, withoutOneShot(bisyncMock.definition("force"))),
    );
    expect(again).toMatchObject({ resync: false, force: false });
    const plain = bisyncMock.definition();
    expect(withoutOneShot(plain)).toBe(plain);
  });

  it("offers a resync, a forced run or only a hint by error code", () => {
    expect(recoveryFor(JobErrorCode.RESYNC_REQUIRED)).toBe("resync");
    expect(recoveryFor(JobErrorCode.SAFETY_ABORT)).toBe("force");
    expect(recoveryFor(JobErrorCode.SIDE_UNAVAILABLE)).toBe("hint");
    expect(recoveryFor(JobErrorCode.NOT_FOUND)).toBeUndefined();
    expect(recoveryFor(undefined)).toBeUndefined();
  });

  it("describes how conflicts are kept", () => {
    expect(conflictsSummary(bisyncOf(bisyncMock.definition()))).toBe(
      "both versions kept, renamed (.conflict1, .conflict2…)",
    );
    const def = create(JobDefinitionSchema, {
      operation: {
        case: "rcloneBisync",
        value: {
          options: {
            conflictResolve: BisyncConflictResolve.NEWER,
            conflictLoser: BisyncConflictLoser.PATHNAME,
            conflictSuffix: "desktop,laptop",
          },
        },
      },
    });
    expect(conflictsSummary(bisyncOf(def))).toBe(
      "the newer version wins; the other is kept, renamed (.desktop for Folder A's, .laptop for Folder B's)",
    );
  });
});
