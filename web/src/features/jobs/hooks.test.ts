import { create } from "@bufbuild/protobuf";
import { durationFromMs } from "@bufbuild/protobuf/wkt";
import { describe, expect, it } from "vitest";

import {
  FieldViolationSchema,
  JobDefinitionSchema,
  JobSchema,
  JobStatus,
  WorkerSchema,
  type Worker,
} from "@/gen/syncr/v1/api_pb";
import { HookPhase, HookStatus } from "@/gen/syncr/v1/link_pb";

import {
  definitionError,
  definitionState,
  emptyDefinition,
  toDefinition,
  withKind,
  type DefinitionErrors,
} from "./definition";
import {
  emptyHooks,
  hasHooks,
  hookNames,
  hooksError,
  hooksNote,
  hooksState,
  hookStatusMeta,
  hookSummary,
  jobHookGroups,
  offeredHooks,
  toHooks,
  unavailableHooks,
  type HooksState,
} from "./hooks";

function worker(name: string, hooks: [string, string?][] | undefined, features = ["job.hooks"]) {
  return create(WorkerSchema, {
    id: name,
    name,
    info:
      hooks === undefined
        ? undefined
        : {
            workerVersion: "v2.1.0",
            features,
            hooks: hooks.map(([n, description = ""]) => ({
              name: n,
              description,
              timeout: durationFromMs(120_000),
            })),
          },
  });
}

const app: HooksState = {
  before: ["stop-app", "dump-db"],
  onSuccess: [],
  onFailure: ["alert"],
  after: ["start-app"],
};

describe("hooks in a definition", () => {
  it("reads a stored definition's hooks and sends them back unchanged", () => {
    const def = create(JobDefinitionSchema, {
      hooks: {
        before: ["stop-app", "dump-db"],
        onSuccess: ["ping"],
        onFailure: ["alert"],
        after: ["start-app"],
      },
      operation: { case: "resticCheck", value: { repositoryId: "r" } },
    });
    const s = definitionState(def);
    expect(s.hooks).toEqual({
      before: ["stop-app", "dump-db"],
      onSuccess: ["ping"],
      onFailure: ["alert"],
      after: ["start-app"],
    });
    expect(create(JobDefinitionSchema, toDefinition(s)).hooks).toEqual(def.hooks);
  });

  it("sends no hooks when none is named, for every kind", () => {
    for (const kind of ["rclone_transfer", "restic_backup", "restic_prune"] as const) {
      const s = emptyDefinition(kind);
      expect(hasHooks(s.hooks)).toBe(false);
      expect(toDefinition(s).hooks).toBeUndefined();
    }
    expect(toHooks(emptyHooks())).toBeUndefined();
    expect(hooksState(undefined)).toEqual(emptyHooks());
  });

  it("keeps the hooks when the kind changes", () => {
    const s = { ...emptyDefinition("restic_backup", "r"), hooks: app };
    expect(withKind(s, "rclone_transfer").hooks).toEqual(app);
  });

  it("does not share its lists with the stored definition", () => {
    const def = create(JobDefinitionSchema, { hooks: { before: ["a"] } });
    const s = hooksState(def);
    s.before.push("b");
    expect(def.hooks?.before).toEqual(["a"]);
  });

  it("names each hook once, in the order they run", () => {
    expect(hookNames({ ...app, after: ["start-app", "stop-app"] })).toEqual([
      "stop-app",
      "dump-db",
      "alert",
      "start-app",
    ]);
    expect(hookSummary(app)).toBe(
      "Before: stop-app, dump-db · After failure: alert · Always: start-app",
    );
    expect(hookSummary(emptyHooks())).toBe("");
  });
});

describe("the hooks a target offers", () => {
  const nas = worker("nas", [
    ["stop-app", "Stop the app"],
    ["start-app", "Start it again"],
  ]);
  const db = worker("db", [["dump-db", "Dump the database"], ["stop-app"]]);

  it("lists one worker's hooks by name", () => {
    expect(offeredHooks([nas])).toEqual([
      { name: "start-app", description: "Start it again", timeoutSeconds: 120, workers: 1 },
      { name: "stop-app", description: "Stop the app", timeoutSeconds: 120, workers: 1 },
    ]);
    expect(offeredHooks([])).toEqual([]);
    expect(offeredHooks([worker("new", undefined)])).toEqual([]);
  });

  it("merges a label's workers, counting who defines what", () => {
    expect(offeredHooks([db, nas]).map((h) => [h.name, h.workers, h.description])).toEqual([
      ["dump-db", 1, "Dump the database"],
      ["start-app", 1, "Start it again"],
      // The first description there is: db has none for it.
      ["stop-app", 2, "Stop the app"],
    ]);
  });

  it("finds the hooks a worker does not define", () => {
    expect(unavailableHooks(app, { workers: [nas] })).toEqual(["dump-db", "alert"]);
    expect(unavailableHooks({ ...emptyHooks(), after: ["start-app"] }, { workers: [nas] })).toEqual(
      [],
    );
    // For a label, only what none of its workers defines.
    expect(unavailableHooks(app, { workers: [nas, db], label: "app" })).toEqual(["alert"]);
    // Nothing is known about a target that has not reported in: the controller decides.
    expect(unavailableHooks(app, { workers: [] })).toEqual([]);
  });

  it("says when a worker must be upgraded first", () => {
    const old: Worker = worker("old-nas", [], ["probe"]);
    expect(hooksNote(emptyHooks(), { workers: [old] })).toBe(
      "old-nas must be upgraded before it can run hooks.",
    );
    expect(hooksNote(emptyHooks(), { workers: [old], label: "nas" })).toBe(
      "The workers labelled nas must be upgraded before they can run hooks.",
    );
    expect(hooksNote(app, { workers: [nas] })).toBeUndefined();
    expect(hooksNote(app, { workers: [] })).toBeUndefined();
  });

  it("says how many of a label's workers can take the job", () => {
    const target = { workers: [nas, db], label: "app" };
    expect(hooksNote(emptyHooks(), target)).toMatch(/defines every hook chosen here/);
    expect(hooksNote({ ...emptyHooks(), before: ["stop-app"] }, target)).toBe(
      "2 of 2 workers labelled app define all of these hooks; a run goes to one of them.",
    );
    expect(hooksNote({ ...emptyHooks(), before: ["stop-app"], after: ["start-app"] }, target)).toBe(
      "1 of 2 workers labelled app defines all of these hooks; a run goes to one of them.",
    );
    expect(
      hooksNote({ ...emptyHooks(), before: ["dump-db"], after: ["start-app"] }, target),
    ).toMatch(/^No worker labelled app defines all of these hooks/);
  });
});

describe("the API's hook errors", () => {
  const violation = (field: string, message: string) =>
    create(FieldViolationSchema, { field, message });

  it("keeps a message about a worker as it is", () => {
    const msg =
      'worker "nas" defines no hook named "dump-db": its operator adds hooks in the worker\'s configuration';
    expect(hooksError(violation("definition.hooks.before[1]", msg))).toBe(msg);
    expect(hooksError(violation("definition.hooks", 'cannot be used on worker "old" yet'))).toBe(
      'cannot be used on worker "old" yet',
    );
  });

  it("says where a hook's own problem is", () => {
    expect(hooksError(violation("definition.hooks.on_success[2]", "is listed twice"))).toBe(
      "After success, hook 3: is listed twice",
    );
    expect(hooksError(violation("definition.hooks.after", "names more than 8 hooks"))).toBe(
      "Always, at the end: names more than 8 hooks",
    );
  });

  it("leaves other fields alone", () => {
    expect(hooksError(violation("definition.max_runtime", "x"))).toBeUndefined();
    expect(hooksError(violation("definition.restic_backup.paths[0]", "x"))).toBeUndefined();
    expect(hooksError(violation("target.worker_id", "x"))).toBeUndefined();
  });

  it("collects every hook error in the definition's hooks section", () => {
    const errors: DefinitionErrors = {};
    expect(
      definitionError(
        violation("definition.hooks.before[0]", 'worker "nas" defines no hook named "a"'),
        errors,
      ),
    ).toBe(true);
    expect(
      definitionError(
        violation("definition.hooks.after[0]", 'worker "nas" defines no hook named "b"'),
        errors,
      ),
    ).toBe(true);
    expect(errors.hooks).toBe(
      'worker "nas" defines no hook named "a" worker "nas" defines no hook named "b"',
    );
    expect(Object.keys(errors)).toEqual(["hooks"]);
  });
});

describe("a job's hooks", () => {
  const job = (status: JobStatus, runs: { name: string; phase: HookPhase; status: HookStatus }[]) =>
    create(JobSchema, {
      status,
      definition: {
        hooks: {
          before: ["stop-app", "dump-db"],
          onSuccess: ["ping"],
          onFailure: ["alert"],
          after: ["start-app"],
        },
      },
      hookRuns: runs,
    });

  it("groups the runs by phase and lists what did not run", () => {
    const groups = jobHookGroups(
      job(JobStatus.FAILED, [
        { name: "stop-app", phase: HookPhase.BEFORE, status: HookStatus.SUCCEEDED },
        { name: "dump-db", phase: HookPhase.BEFORE, status: HookStatus.FAILED },
        { name: "alert", phase: HookPhase.ON_FAILURE, status: HookStatus.SUCCEEDED },
        { name: "start-app", phase: HookPhase.AFTER, status: HookStatus.SUCCEEDED },
      ]),
    );
    expect(groups.map((g) => [g.phase.label, g.rows.map((r) => [r.name, r.run?.status])])).toEqual([
      [
        "Before the job",
        [
          ["stop-app", HookStatus.SUCCEEDED],
          ["dump-db", HookStatus.FAILED],
        ],
      ],
      // The job failed, so its success hook did not run.
      ["After success", [["ping", undefined]]],
      ["After failure", [["alert", HookStatus.SUCCEEDED]]],
      ["Always, at the end", [["start-app", HookStatus.SUCCEEDED]]],
    ]);
  });

  it("shows a queued job's hooks as not run yet, and nothing for a job without hooks", () => {
    const groups = jobHookGroups(job(JobStatus.QUEUED, []));
    expect(groups.flatMap((g) => g.rows).every((r) => r.run === undefined)).toBe(true);
    expect(groups).toHaveLength(4);
    expect(jobHookGroups(create(JobSchema, {}))).toEqual([]);
  });

  it("hides nothing a worker reported: an unknown phase is listed last", () => {
    const groups = jobHookGroups(
      create(JobSchema, {
        hookRuns: [{ name: "later", phase: 99 as HookPhase, status: HookStatus.SUCCEEDED }],
      }),
    );
    expect(groups.map((g) => [g.phase.key, g.rows.map((r) => r.name)])).toEqual([
      ["after", ["later"]],
    ]);
  });

  it("names every status", () => {
    expect(hookStatusMeta(HookStatus.SUCCEEDED)).toEqual({ label: "Succeeded", tone: "success" });
    expect(hookStatusMeta(HookStatus.FAILED).tone).toBe("danger");
    expect(hookStatusMeta(HookStatus.TIMED_OUT).label).toBe("Timed out");
    expect(hookStatusMeta(HookStatus.CANCELLED).label).toBe("Stopped");
    expect(hookStatusMeta(HookStatus.UNSPECIFIED).label).toBe("Unknown");
  });
});
