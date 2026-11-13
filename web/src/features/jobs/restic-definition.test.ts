import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";

import { JobDefinitionSchema, ValidationErrorSchema } from "@/gen/syncr/v1/api_pb";
import {
  BisyncCompare,
  BisyncConflictLoser,
  BisyncConflictResolve,
  BisyncResyncMode,
  FilterAction,
  RcloneTransferMode,
  ResticRestoreOverwrite,
} from "@/gen/syncr/v1/link_pb";

import {
  definitionError,
  definitionState,
  editableKinds,
  emptyDefinition,
  forWorkerOS,
  isDryRun,
  isEditableKind,
  scheduleKinds,
  toDefinition,
  validateDefinition,
  withKind,
  type DefinitionErrors,
} from "./definition";
import { resticSummary } from "./model";
import { formKind, validateRunJobSearch } from "./search";

type Init = MessageInitShape<typeof JobDefinitionSchema>;

/** roundTrip stores a definition, loads it into the form and builds it again. */
function roundTrip(init: Init) {
  const stored = create(JobDefinitionSchema, init);
  const state = definitionState(stored);
  return { stored, state, again: create(JobDefinitionSchema, toDefinition(state)) };
}

const cases: [string, Init][] = [
  [
    "rclone_transfer",
    {
      operation: {
        case: "rcloneTransfer",
        value: {
          mode: RcloneTransferMode.SYNC,
          source: { remoteId: "", path: "/srv/in" },
          destination: { remoteId: "r1", path: "out" },
          options: { transfers: 8, dryRun: true, maxTransferBytes: 1048576n },
        },
      },
    },
  ],
  [
    "restic_backup",
    {
      operation: {
        case: "resticBackup",
        value: {
          repositoryId: "repo-1",
          paths: ["/srv/photos", "/srv/docs"],
          excludes: ["*.tmp", ".cache"],
          tags: ["photos", "nightly"],
          host: "nas-01",
          useFsSnapshot: true,
          excludeCaches: true,
          oneFileSystem: true,
          excludeLargerThanBytes: 512n * 1024n * 1024n,
          dryRun: true,
        },
      },
    },
  ],
  [
    "restic_backup",
    {
      operation: {
        case: "resticBackup",
        // A size that is no whole number of MiB must survive too.
        value: { repositoryId: "repo-1", paths: ["/a"], excludeLargerThanBytes: 1500n },
      },
    },
  ],
  [
    "restic_forget",
    {
      operation: {
        case: "resticForget",
        value: {
          repositoryId: "repo-1",
          policy: {
            keepLast: 3,
            keepHourly: 24,
            keepDaily: 7,
            keepWeekly: 4,
            keepMonthly: 12,
            keepYearly: 2,
            keepWithin: "1y6m",
            keepTags: ["keep"],
          },
          prune: true,
          tags: ["photos"],
          host: "nas-01",
          dryRun: true,
        },
      },
    },
  ],
  [
    "restic_check",
    { operation: { case: "resticCheck", value: { repositoryId: "repo-1", readDataPercent: 0 } } },
  ],
  [
    "restic_check",
    { operation: { case: "resticCheck", value: { repositoryId: "repo-1", readDataPercent: 10 } } },
  ],
  [
    "restic_check",
    { operation: { case: "resticCheck", value: { repositoryId: "repo-1", readDataPercent: 37 } } },
  ],
  [
    "restic_prune",
    { operation: { case: "resticPrune", value: { repositoryId: "repo-1", dryRun: true } } },
  ],
  // Two-way syncs last: tests above pick cases by index.
  [
    "rclone_bisync",
    {
      operation: {
        case: "rcloneBisync",
        value: {
          path1: { remoteId: "", path: "/srv/shared" },
          path2: { remoteId: "r1", path: "shared" },
          options: {
            createEmptySrcDirs: true,
            filters: [{ action: FilterAction.EXCLUDE, pattern: "*.tmp" }],
          },
        },
      },
    },
  ],
  [
    "rclone_bisync",
    {
      operation: {
        case: "rcloneBisync",
        value: {
          path1: { remoteId: "r2", path: "a" },
          path2: { remoteId: "r1", path: "b/c" },
          options: {
            transfers: 8,
            checkers: 16,
            bandwidthLimitBytesPerSecond: 3n * 1024n * 1024n,
            lowLevelRetries: 0,
            dryRun: true,
            compare: [BisyncCompare.SIZE, BisyncCompare.CHECKSUM],
            conflictResolve: BisyncConflictResolve.PATH2,
            conflictLoser: BisyncConflictLoser.PATHNAME,
            conflictSuffix: "laptop,desktop",
            maxDeletePercent: 25,
            checkAccess: true,
            resyncMode: BisyncResyncMode.OLDER,
          },
        },
      },
    },
  ],
];

describe("JobDefinitionState", () => {
  it.each(cases)("round-trips a stored %s definition", (kind, init) => {
    const { stored, state, again } = roundTrip(init);
    expect(state.kind).toBe(kind);
    expect(again).toEqual(stored);
    expect(validateDefinition(state)).toEqual({});
  });

  it("covers every kind the form edits", () => {
    expect(new Set(cases.map(([k]) => k))).toEqual(new Set(editableKinds));
  });

  it("never offers a restore or a data deletion to forms and schedules", () => {
    for (const kinds of [editableKinds, scheduleKinds]) {
      expect(kinds).not.toContain("restic_restore");
      expect(kinds).not.toContain("restic_delete_data");
    }
    expect(isEditableKind("restic_restore")).toBe(false);
    expect(isEditableKind("restic_delete_data")).toBe(false);
    expect(formKind("restic_delete_data")).toBeUndefined();
    expect(formKind("restic_restore")).toBeUndefined();
    expect(validateRunJobSearch({ kind: "restic_delete_data", repository: "r" })).toEqual({
      from: undefined,
      kind: undefined,
      repository: "r",
    });
    expect(validateRunJobSearch({ kind: "restic_check" }).kind).toBe("restic_check");
  });

  it("opens a restore or a data deletion as a blank transfer, never as itself", () => {
    for (const init of [
      { operation: { case: "resticRestore", value: { repositoryId: "r", snapshotId: "s" } } },
      { operation: { case: "resticDeleteData", value: { repositoryId: "r" } } },
    ] as Init[]) {
      expect(definitionState(create(JobDefinitionSchema, init)).kind).toBe("rclone_transfer");
    }
  });

  it("starts each kind blank, on the given repository", () => {
    for (const kind of editableKinds) {
      const s = emptyDefinition(kind, "repo-9");
      expect(s.kind).toBe(kind);
      const def = toDefinition(s);
      if (kind !== "rclone_transfer" && kind !== "rclone_bisync")
        expect(def.operation?.value).toMatchObject({ repositoryId: "repo-9" });
    }
    const retention = emptyDefinition("restic_forget");
    expect(toDefinition(retention).operation?.value).toMatchObject({
      prune: true,
      policy: { keepDaily: 7, keepWeekly: 4, keepMonthly: 12 },
    });
    expect(toDefinition(emptyDefinition("restic_check")).operation?.value).toMatchObject({
      readDataPercent: 0,
    });
  });

  it("keeps the repository when the kind changes", () => {
    const backup = emptyDefinition("restic_backup", "repo-1");
    const check = withKind(backup, "restic_check");
    expect(check).toMatchObject({ kind: "restic_check", check: { repositoryId: "repo-1" } });
    expect(withKind(check, "restic_check")).toBe(check);
    expect(withKind(check, "rclone_transfer").kind).toBe("rclone_transfer");
  });

  it("validates each restic kind", () => {
    expect(validateDefinition(emptyDefinition("restic_backup"))).toEqual({
      repository: "Choose the repository to back up into.",
      paths: "Add at least one folder or file to back up.",
    });
    const backup = emptyDefinition("restic_backup", "r");
    if (backup.kind !== "restic_backup") throw new Error("kind");
    backup.backup.paths = ["relative/path"];
    backup.backup.excludeLargerThan = "0";
    expect(validateDefinition(backup)).toEqual({
      paths: "Enter absolute paths on the worker, such as /srv/data.",
      options: "The size limit must be a number above zero.",
    });
    backup.backup.paths = ["C:\\Users\\ada", " "];
    backup.backup.excludeLargerThan = "1.5";
    expect(validateDefinition(backup)).toEqual({});
    expect(toDefinition(backup).operation?.value).toMatchObject({
      paths: ["C:\\Users\\ada"],
      excludeLargerThanBytes: 1572864n,
    });

    const retention = emptyDefinition("restic_forget", "r");
    if (retention.kind !== "restic_forget") throw new Error("kind");
    retention.retention.policy = {
      ...retention.retention.policy,
      keepDaily: "",
      keepWeekly: "",
      keepMonthly: "",
    };
    expect(validateDefinition(retention).policy).toMatch(/at least one rule/);
    retention.retention.policy.keepWithin = "two weeks";
    expect(validateDefinition(retention).policy).toMatch(/years, months, days and hours/);
    retention.retention.policy.keepWithin = "30d";
    expect(validateDefinition(retention)).toEqual({});

    // The controller's rules, shown inline instead of coming back from the
    // server: no zero or leading-zero periods, whole counts, blank tags
    // dropped (so a blank tag alone is no rule), filter tags and host.
    const forget = (policy: Partial<typeof retention.retention.policy>, extra = {}) => ({
      ...retention,
      retention: {
        ...retention.retention,
        ...extra,
        policy: { ...retention.retention.policy, keepWithin: "", ...policy },
      },
    });
    expect(validateDefinition(forget({ keepWithin: "0d" })).policy).toMatch(/at least 1/);
    expect(validateDefinition(forget({ keepWithin: "01d" })).policy).toMatch(/leading zeros/);
    expect(validateDefinition(forget({ keepTags: [" "] })).policy).toMatch(/at least one rule/);
    expect(validateDefinition(forget({ keepDaily: "1.5" })).policy).toMatch(/whole number/);
    expect(validateDefinition(forget({ keepWithin: "1d1y" }))).toEqual({});
    expect(validateDefinition(forget({ keepWithin: "2y5m7d3h" }))).toEqual({});
    expect(validateDefinition(forget({ keepLast: "3" }, { tags: ["a,b"] })).options).toMatch(
      /comma/,
    );
    expect(
      toDefinition(forget({ keepDaily: "07", keepTags: [" legal ", " "] })).operation?.value,
    ).toMatchObject({ policy: { keepDaily: 7, keepTags: ["legal"], keepWithin: "" } });

    const check = emptyDefinition("restic_check", "r");
    if (check.kind !== "restic_check") throw new Error("kind");
    check.check.readData = "custom";
    for (const bad of ["", "0", "101", "x"]) {
      check.check.custom = bad;
      expect(validateDefinition(check).readData).toBe("Enter a whole percentage from 1 to 100.");
    }
    check.check.custom = "25";
    expect(toDefinition(check).operation?.value).toMatchObject({ readDataPercent: 25 });

    expect(validateDefinition(emptyDefinition("restic_prune"))).toEqual({
      repository: "Choose the repository to prune.",
    });
  });

  it("reports a dry run per kind", () => {
    expect(isDryRun(roundTrip(cases[1][1]).state)).toBe(true);
    expect(isDryRun(roundTrip(cases[3][1]).state)).toBe(true);
    expect(isDryRun(roundTrip(cases[4][1]).state)).toBe(false);
    expect(isDryRun(roundTrip(cases[7][1]).state)).toBe(true);
  });

  it("leaves VSS out for a worker that is not Windows", () => {
    const { state } = roundTrip(cases[1][1]);
    const vss = (os?: string) =>
      (toDefinition(forWorkerOS(state, os)).operation?.value as { useFsSnapshot: boolean })
        .useFsSnapshot;
    expect(vss("windows")).toBe(true);
    expect(vss("linux")).toBe(false);
    // Unknown (a label target): the controller decides.
    expect(vss(undefined)).toBe(true);
  });

  it("places server violations on the restic sections", () => {
    const errors: DefinitionErrors = {};
    const put = (field: string, message: string) =>
      definitionError(
        create(ValidationErrorSchema, { violations: [{ field, message }] }).violations[0],
        errors,
      );
    expect(put("definition.restic_backup.repository_id", "repo")).toBe(true);
    expect(put("definition.restic_backup.paths[1]", "path")).toBe(true);
    expect(put("definition.restic_backup.excludes[0]", "exclude")).toBe(true);
    expect(put("definition.restic_forget.policy.keep_within", "policy")).toBe(true);
    expect(put("definition.restic_check.read_data_percent", "percent")).toBe(true);
    expect(put("definition.restic_backup.use_fs_snapshot", "vss")).toBe(true);
    expect(put("definition.rclone_transfer.source.path", "src")).toBe(true);
    expect(put("name", "other")).toBe(false);
    expect(errors).toEqual({
      repository: "repo",
      paths: "path",
      excludes: "exclude",
      policy: "policy",
      readData: "percent",
      options: "vss",
      sourcePath: "src",
    });
  });
});

describe("resticSummary", () => {
  const repo = {
    $typeName: "syncr.v1.JobRepositoryRef" as const,
    id: "r",
    name: "Photos vault",
    deleted: false,
  };
  const sum = (init: Init) => resticSummary(create(JobDefinitionSchema, init), repo);

  it("describes every restic kind", () => {
    expect(sum(cases[1][1])).toEqual({
      title: "Back up /srv/photos, /srv/docs → Photos vault",
      detail:
        "tagged photos, nightly, 2 excludes, volume snapshot (VSS), skips caches, one file system, as host nas-01, dry run",
    });
    expect(sum(cases[4][1])).toEqual({ title: "Check Photos vault", detail: "structure only" });
    expect(sum(cases[5][1])?.detail).toBe("reads 10% of the data");
    expect(sum(cases[7][1])).toEqual({ title: "Prune Photos vault", detail: "dry run" });
    expect(sum(cases[3][1])?.title).toBe("Apply retention to Photos vault");
    expect(sum(cases[3][1])?.detail).toContain("prunes afterwards");
    expect(
      sum({
        operation: {
          case: "resticRestore",
          value: {
            repositoryId: "r",
            snapshotId: "0123456789abcdef",
            target: "/srv/restore",
            paths: ["/a", "/b"],
            overwrite: ResticRestoreOverwrite.UNSPECIFIED,
          },
        },
      }),
    ).toEqual({
      title: "Restore 2 items from Photos vault → /srv/restore",
      detail: "keeps existing files",
    });
    const ofFolder = (paths: string[]) =>
      sum({
        operation: {
          case: "resticRestore",
          value: {
            repositoryId: "r",
            snapshotId: "0123456789abcdef",
            target: "/srv/photos",
            subfolder: "/srv/photos",
            paths,
          },
        },
      })?.title;
    expect(ofFolder(["/srv/photos/a", "/srv/photos/b"])).toBe(
      "Restore 2 items of /srv/photos from Photos vault → /srv/photos",
    );
    expect(ofFolder(["/srv/photos/a"])).toBe(
      "Restore /srv/photos/a from Photos vault → /srv/photos",
    );
    expect(ofFolder([])).toBe("Restore /srv/photos from Photos vault → /srv/photos");
    expect(
      sum({ operation: { case: "resticDeleteData", value: { repositoryId: "r" } } })?.title,
    ).toBe("Delete the data of Photos vault");
    expect(sum(cases[0][1])).toBeUndefined();
  });
});
