import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { describe, expect, it } from "vitest";

import { JobStatus, WorkerSchema } from "@/gen/syncr/v1/api_pb";
import {
  ScheduleGroupSchema,
  ScheduleRunStatus,
  ScheduleSchema,
  type Schedule,
} from "@/gen/syncr/v1/schedules_pb";

import {
  counts,
  matches,
  needsAttention,
  nextSort,
  searchText,
  sections,
  sortSchedules,
  statusOf,
  type ListContext,
} from "./list";

const now = Date.UTC(2026, 10, 21, 12);
const H = 3_600_000;

const photos = create(ScheduleGroupSchema, { id: "g-photos", name: "Photos" });
const office = create(ScheduleGroupSchema, { id: "g-office", name: "office" });
const nas = create(WorkerSchema, { id: "w-nas", name: "nas-01" });
const pc = create(WorkerSchema, { id: "w-pc", name: "Office PC" });
const ctx: ListContext = {
  workers: new Map([
    [nas.id, nas],
    [pc.id, pc],
  ]),
  groups: [office, photos],
};

/** ScheduleInit is a schedule's fields, as create takes them (not a message). */
type ScheduleInit = Exclude<MessageInitShape<typeof ScheduleSchema>, Schedule>;

function sched(name: string, init: ScheduleInit = {}): Schedule {
  return create(ScheduleSchema, {
    id: `s-${name}`,
    name,
    cron: "30 2 * * *",
    timezone: "Europe/Berlin",
    enabled: true,
    target: { target: { case: "workerId", value: nas.id } },
    definition: {
      operation: {
        case: "rcloneTransfer",
        value: {
          source: { path: "/srv/photos" },
          destination: { remoteId: "r-b2", path: "photos" },
        },
      },
    },
    remotes: [{ id: "r-b2", name: "B2 offsite", backendType: "b2" }],
    ...init,
  });
}

const run = (status: ScheduleRunStatus, jobStatus = JobStatus.UNSPECIFIED, at = now - H) => ({
  status,
  jobStatus,
  scheduledFor: timestampFromMs(at),
});

describe("needsAttention and statusOf", () => {
  it("flags schedules Syncr paused and last runs that went wrong", () => {
    const cases: [Schedule, boolean, string][] = [
      [sched("ok", { lastRun: run(ScheduleRunStatus.LAUNCHED, JobStatus.SUCCEEDED) }), false, "on"],
      [sched("never"), false, "on"],
      [
        sched("failed", { lastRun: run(ScheduleRunStatus.LAUNCHED, JobStatus.FAILED) }),
        true,
        "attention",
      ],
      [sched("missed", { lastRun: run(ScheduleRunStatus.MISSED) }), true, "attention"],
      [sched("no start", { lastRun: run(ScheduleRunStatus.FAILED) }), true, "attention"],
      [
        sched("interrupted", { lastRun: run(ScheduleRunStatus.LAUNCHED, JobStatus.INTERRUPTED) }),
        true,
        "attention",
      ],
      // Someone cancelled it: nothing to fix.
      [
        sched("cancelled job", { lastRun: run(ScheduleRunStatus.LAUNCHED, JobStatus.CANCELLED) }),
        false,
        "on",
      ],
      [sched("cancelled run", { lastRun: run(ScheduleRunStatus.CANCELLED) }), false, "on"],
      [
        sched("paused", { enabled: false, lastRun: run(ScheduleRunStatus.MISSED) }),
        false,
        "paused",
      ],
      [sched("auto", { enabled: false, pausedReason: "worker_removed" }), true, "attention"],
    ];
    for (const [s, attention, status] of cases) {
      expect(needsAttention(s), s.name).toBe(attention);
      expect(statusOf(s), s.name).toBe(status);
    }
  });

  it("counts every schedule as on or paused, and some as needing attention", () => {
    const list = [
      sched("a"),
      sched("b", { lastRun: run(ScheduleRunStatus.MISSED) }),
      sched("c", { enabled: false }),
      sched("d", { enabled: false, pausedReason: "remote_unavailable" }),
    ];
    expect(counts(list)).toEqual({ total: 4, attention: 2, on: 2, paused: 2 });
  });
});

describe("matches", () => {
  const list = [
    sched("Nightly photos", { groupId: photos.id, description: "To Backblaze" }),
    sched("Office docs", {
      groupId: office.id,
      target: { target: { case: "workerLabel", value: "desktop" } },
      definition: {
        operation: { case: "resticBackup", value: { repositoryId: "repo", paths: ["C:/Docs"] } },
      },
      repository: { id: "repo", name: "Office vault" },
      remotes: [],
    }),
    sched("Loose", { enabled: false }),
  ];
  const names = (search: Parameters<typeof matches>[1]) =>
    list.filter((s) => matches(s, search, ctx)).map((s) => s.name);

  it("searches names, descriptions, groups, paths, remotes, repositories and targets", () => {
    expect(names({ q: "backblaze" })).toEqual(["Nightly photos"]);
    expect(names({ q: "BACKBLAZE" })).toEqual(["Nightly photos"]);
    // The group's name (office) and the schedule's.
    expect(names({ q: "office" })).toEqual(["Office docs"]);
    expect(names({ q: "/srv/photos" })).toEqual(["Nightly photos", "Loose"]);
    expect(names({ q: "b2 offsite" })).toEqual(["Nightly photos", "Loose"]);
    expect(names({ q: "office vault" })).toEqual(["Office docs"]);
    expect(names({ q: "desktop" })).toEqual(["Office docs"]);
    expect(names({ q: "nas-01 loose" })).toEqual(["Loose"]);
    // When it runs, in words and as the expression.
    expect(names({ q: "at 02:30" })).toHaveLength(3);
    expect(names({ q: "30 2 *" })).toHaveLength(3);
    expect(searchText(list[0], ctx)).toContain("photos");
  });

  it("filters by kind, target, group and status", () => {
    expect(names({ kind: "restic_backup" })).toEqual(["Office docs"]);
    expect(names({ target: "l:desktop" })).toEqual(["Office docs"]);
    expect(names({ target: `w:${nas.id}` })).toEqual(["Nightly photos", "Loose"]);
    expect(names({ group: photos.id })).toEqual(["Nightly photos"]);
    expect(names({ group: "none" })).toEqual(["Loose"]);
    expect(names({ status: "paused" })).toEqual(["Loose"]);
    expect(names({ status: "on" })).toEqual(["Nightly photos", "Office docs"]);
    expect(names({ status: "attention" })).toEqual([]);
    expect(names({ q: "photos", status: "on" })).toEqual(["Nightly photos"]);
  });
});

describe("sortSchedules", () => {
  const a = sched("alpha", {
    nextRunAt: timestampFromMs(now + 3 * H),
    lastRun: run(ScheduleRunStatus.LAUNCHED, JobStatus.SUCCEEDED, now - 5 * H),
  });
  const b = sched("Beta", {
    nextRunAt: timestampFromMs(now + H),
    lastRun: run(ScheduleRunStatus.MISSED, 0, now - H),
  });
  const c = sched("charlie", { enabled: false });
  const order = (sort?: Parameters<typeof sortSchedules>[1]) =>
    sortSchedules([c, b, a], sort).map((s) => s.name);

  it("sorts by name, ignoring case, either way", () => {
    expect(order()).toEqual(["alpha", "Beta", "charlie"]);
    expect(order("-name")).toEqual(["charlie", "Beta", "alpha"]);
  });

  it("sorts by next and last run, schedules without one last in both directions", () => {
    expect(order("next")).toEqual(["Beta", "alpha", "charlie"]);
    expect(order("-next")).toEqual(["alpha", "Beta", "charlie"]);
    expect(order("-last")).toEqual(["Beta", "alpha", "charlie"]);
    expect(order("last")).toEqual(["alpha", "Beta", "charlie"]);
  });

  it("starts each column at its default direction, then reverses it", () => {
    expect(nextSort(undefined, "name")).toBe("-name");
    expect(nextSort("-name", "name")).toBe("name");
    expect(nextSort(undefined, "next")).toBe("next");
    expect(nextSort("next", "next")).toBe("-next");
    expect(nextSort("next", "last")).toBe("-last");
    expect(nextSort("-last", "last")).toBe("last");
  });
});

describe("sections", () => {
  const list = sortSchedules([
    sched("Nightly", { groupId: photos.id }),
    sched("Docs", {
      groupId: office.id,
      target: { target: { case: "workerLabel", value: "desktop" } },
    }),
    sched("Loose", { target: { target: { case: "workerId", value: pc.id } }, enabled: false }),
    sched("Gone", {
      groupId: "g-deleted",
      definition: { operation: { case: "resticCheck", value: {} } },
    }),
  ]);
  const shape = (secs: ReturnType<typeof sections>) =>
    secs.map((s) => [s.title, s.schedules.map((x) => x.name)]);

  it("lists groups by name, then the schedules in no group (a deleted group counts as none)", () => {
    expect(shape(sections(list, "group", ctx))).toEqual([
      ["office", ["Docs"]],
      ["Photos", ["Nightly"]],
      ["No group", ["Gone", "Loose"]],
    ]);
  });

  it("shows empty groups only when asked (no filter set)", () => {
    const some = list.filter((s) => s.name !== "Docs");
    expect(sections(some, "group", ctx).map((s) => s.title)).toEqual(["Photos", "No group"]);
    const shown = sections(some, "group", ctx, true);
    expect(shown.map((s) => s.title)).toEqual(["office", "Photos", "No group"]);
    expect(shown[0].group).toBe(office);
  });

  it("is one untitled section without groups or grouping", () => {
    expect(shape(sections(list, "group", { ...ctx, groups: [] }))).toEqual([
      ["", list.map((s) => s.name)],
    ]);
    expect(shape(sections(list, "none", ctx))).toEqual([["", list.map((s) => s.name)]]);
  });

  it("is no section at all for an empty list", () => {
    expect(sections([], "none", ctx)).toEqual([]);
    expect(sections([], "group", { ...ctx, groups: [] })).toEqual([]);
    expect(sections([], "target", ctx)).toEqual([]);
  });

  it("groups by worker (workers first), kind and status", () => {
    expect(shape(sections(list, "target", ctx))).toEqual([
      ["nas-01", ["Gone", "Nightly"]],
      ["Office PC", ["Loose"]],
      ["Workers labelled desktop", ["Docs"]],
    ]);
    expect(shape(sections(list, "kind", ctx))).toEqual([
      ["Transfer", ["Docs", "Loose", "Nightly"]],
      ["Check", ["Gone"]],
    ]);
    expect(shape(sections(list, "status", ctx))).toEqual([
      ["On", ["Docs", "Gone", "Nightly"]],
      ["Paused", ["Loose"]],
    ]);
  });
});
