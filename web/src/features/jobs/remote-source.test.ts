import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";

import {
  FieldViolationSchema,
  JobDefinitionSchema,
  JobSchema,
  type JobDefinition,
} from "@/gen/syncr/v1/api_pb";
import { FilterAction, JobErrorCode, ResticRemoteSourceMethod } from "@/gen/syncr/v1/link_pb";

import {
  definitionError,
  definitionState,
  definitionWorkerProblem,
  emptyDefinition,
  emptyTimeLimit,
  toDefinition,
  validateDefinition,
  type DefinitionErrors,
  type JobDefinitionState,
} from "./definition";
import { emptyHooks } from "./hooks";
import { errorHint, errorTitle, jobTitle, resticSummary } from "./model";
import { remoteSourceTitle, repositoryOverlap } from "./remote-source";
import { toBackup, type BackupState } from "./restic-definition";

const MIRROR = ResticRemoteSourceMethod.MIRROR;
const MOUNT = ResticRemoteSourceMethod.MOUNT;

/** asDef wraps a backup form in a definition form. */
function asDef(backup: BackupState): JobDefinitionState {
  return { kind: "restic_backup", backup, timeLimit: emptyTimeLimit(), hooks: emptyHooks() };
}

/** remoteBackup is a backup form switched to a folder of a remote, filled in. */
function remoteBackup(over: Partial<BackupState["remote"]> = {}): BackupState {
  const s = emptyDefinition("restic_backup", "repo-1");
  if (s.kind !== "restic_backup") throw new Error("not a backup");
  return {
    ...s.backup,
    source: "remote",
    tags: ["phone"],
    excludeCaches: true,
    remote: {
      ...s.backup.remote,
      remoteId: "nextcloud",
      path: "/Photos/",
      filters: [
        { action: FilterAction.EXCLUDE, pattern: " .thumbnails/** " },
        { action: FilterAction.EXCLUDE, pattern: "" },
      ],
      transfers: "8",
      bwlimit: "2",
      bwlimitUnit: "MiB",
      fastList: true,
      discardMirror: true,
      allowEmpty: true,
      ...over,
    },
  };
}

describe("the backup of a folder of a remote", () => {
  it("sends the folder of a remote and its mirror options", () => {
    expect(toBackup(remoteBackup())).toEqual({
      repositoryId: "repo-1",
      remoteSource: {
        remoteId: "nextcloud",
        path: "Photos",
        method: MIRROR,
        filters: [{ action: FilterAction.EXCLUDE, pattern: ".thumbnails/**" }],
        discardMirror: true,
        allowEmpty: true,
        transfers: 8,
        bandwidthLimitBytesPerSecond: 2n * 1024n * 1024n,
        fastList: true,
      },
      tags: ["phone"],
      host: "",
      excludeCaches: true,
      excludeLargerThanBytes: undefined,
      dryRun: false,
    });
  });

  it("drops the mirror's own options with a mount, which the controller refuses", () => {
    const src = toBackup(remoteBackup({ method: MOUNT })).remoteSource;
    expect(src).toMatchObject({
      method: MOUNT,
      discardMirror: false,
      transfers: undefined,
      fastList: false,
      // The bandwidth limit and empty folders apply to both methods.
      bandwidthLimitBytesPerSecond: 2n * 1024n * 1024n,
      allowEmpty: true,
    });
  });

  it("keeps both sources while switching and sends only the chosen one", () => {
    const remote = remoteBackup();
    const typed: BackupState = {
      ...remote,
      paths: ["/srv/photos"],
      excludes: ["*.tmp"],
      oneFileSystem: true,
      useFsSnapshot: true,
    };
    // A backup of a remote never sends paths, excludes, one file system or VSS.
    const asRemote = toBackup(typed);
    expect(asRemote).not.toHaveProperty("paths");
    expect(asRemote).not.toHaveProperty("excludes");
    expect(asRemote).not.toHaveProperty("oneFileSystem");
    expect(asRemote).not.toHaveProperty("useFsSnapshot");
    // Back to folders on a worker: what was typed there is all still sent,
    // and the remote is not.
    const asPaths = toBackup({ ...typed, source: "paths" });
    expect(asPaths.remoteSource).toBeUndefined();
    expect(asPaths).toMatchObject({
      paths: ["/srv/photos"],
      excludes: ["*.tmp"],
      oneFileSystem: true,
      useFsSnapshot: true,
    });
    // And the remote side is kept for a switch back.
    expect({ ...typed, source: "paths" }.remote).toEqual(remote.remote);
  });

  it("round-trips through a stored definition", () => {
    const def = create(JobDefinitionSchema, {
      operation: { case: "resticBackup", value: toBackup(remoteBackup()) },
    });
    const state = definitionState(def);
    expect(state.kind === "restic_backup" && state.backup.source).toBe("remote");
    expect(create(JobDefinitionSchema, toDefinition(state))).toEqual(def);
  });

  it("checks the remote, the parallel downloads and the tags before sending", () => {
    const check = (b: BackupState) => validateDefinition(asDef(b));
    expect(check(remoteBackup({ remoteId: "" }))).toMatchObject({
      sourceRemoteId: "Choose the remote to back up.",
    });
    expect(check(remoteBackup({ path: "a/../b" }))).toMatchObject({
      sourceFolder: 'A folder cannot contain "..".',
    });
    expect(check(remoteBackup({ transfers: "65" }))).toMatchObject({
      sourceOptions: "Parallel downloads must be a whole number from 1 to 64.",
    });
    // A mount ignores the mirror's transfers.
    expect(check(remoteBackup({ transfers: "65", method: MOUNT }))).toEqual({});
    expect(check({ ...remoteBackup(), tags: ["syncr-remote:x"] }).tags).toMatch(
      /cannot start with “syncr-remote:”/,
    );
    expect(
      check({ ...remoteBackup(), tags: Array.from({ length: 32 }, (_, i) => `t${i}`) }).tags,
    ).toMatch(/at most 31 tags/);
    // No worker paths are asked for.
    expect(check(remoteBackup())).toEqual({});
  });

  it("places the API's field errors on their fields", () => {
    const at = (field: string, message = "m"): DefinitionErrors => {
      const e: DefinitionErrors = {};
      expect(definitionError(create(FieldViolationSchema, { field, message }), e)).toBe(true);
      return e;
    };
    const rs = "definition.restic_backup.remote_source";
    expect(at(`${rs}.remote_id`)).toEqual({ sourceRemoteId: "m" });
    expect(at(`${rs}.path`)).toEqual({ sourceFolder: "m" });
    expect(at(`${rs}.method`)).toEqual({ sourceMethod: "m" });
    expect(at(`${rs}.filters[2].pattern`)).toEqual({ sourceFilters: "m" });
    expect(at(`${rs}.filters`)).toEqual({ sourceFilters: "m" });
    expect(at(`${rs}.transfers`)).toEqual({ sourceOptions: "m" });
    expect(at(`${rs}.discard_mirror`)).toEqual({ sourceOptions: "m" });
    expect(at(`${rs}.bandwidth_limit_bytes_per_second`)).toEqual({ sourceOptions: "m" });
    expect(at("definition.restic_backup.tags")).toEqual({ tags: "m" });
    expect(at("definition.restic_backup.tags[3]")).toEqual({ tags: "m" });
    expect(at("definition.restic_backup.excludes")).toEqual({ excludes: "m" });
    expect(at("definition.restic_backup.paths")).toEqual({ paths: "m" });
    // Retention's tags stay with its options.
    expect(at("definition.restic_forget.tags")).toEqual({ options: "m" });
  });

  it("sees a folder that is, holds or is inside the repository's", () => {
    const repo = { remoteId: "minio", path: "restic/vault", name: "Vault" };
    const o = (path: string, remoteId = "minio") => repositoryOverlap({ remoteId, path }, repo);
    expect(o("restic/vault/")).toMatch(/^This is the folder of repository Vault/);
    expect(o("restic")).toMatch(/^This folder holds repository Vault/);
    expect(o("")).toMatch(/^This folder holds repository Vault/);
    expect(o("restic/vault/x")).toMatch(/^This folder is inside repository Vault/);
    expect(o("restic/vault2")).toBeUndefined();
    expect(o("restic", "nextcloud")).toBeUndefined();
  });

  it("names the worker problem of each method for the worker pickers", () => {
    const def = asDef(remoteBackup({ method: MOUNT }));
    expect(definitionWorkerProblem(def)).toBeTypeOf("function");
    expect(definitionWorkerProblem(emptyDefinition("restic_backup"))).toBeUndefined();
    expect(definitionWorkerProblem(emptyDefinition("rclone_transfer"))).toBeUndefined();
  });
});

describe("jobs and schedules of a backup of a remote", () => {
  const def = (method = MIRROR, path = "Photos"): JobDefinition =>
    create(JobDefinitionSchema, {
      operation: {
        case: "resticBackup",
        value: {
          repositoryId: "r",
          tags: ["phone"],
          remoteSource: {
            remoteId: "nc",
            path,
            method,
            filters: [{ action: FilterAction.EXCLUDE, pattern: "*.tmp" }],
            allowEmpty: true,
          },
        },
      },
    });
  const remotes = [{ id: "nc", name: "Nextcloud" }];

  it("titles and describes it like the controller", () => {
    expect(remoteSourceTitle({ remoteId: "nc", path: "Photos", method: MIRROR }, remotes)).toBe(
      "Nextcloud/Photos (mirror)",
    );
    expect(remoteSourceTitle({ remoteId: "nc", path: "", method: MOUNT }, remotes)).toBe(
      "Nextcloud (mount)",
    );
    expect(remoteSourceTitle({ remoteId: "gone", path: "", method: MOUNT }, [])).toBe(
      "a remote (mount)",
    );
    expect(
      resticSummary(def(), { id: "r", name: "Vault", deleted: false } as never, remotes),
    ).toEqual({
      title: "Back up Nextcloud/Photos (mirror) → Vault",
      detail: "1 filter, an empty folder is allowed, tagged phone",
    });
    // A job's title is the controller's.
    const job = create(JobSchema, { definition: def(), title: "From the API" });
    expect(jobTitle(job)).toBe("From the API");
  });

  it("explains an empty folder and a method its worker cannot use", () => {
    const failed = (code: JobErrorCode, method = MIRROR) =>
      create(JobSchema, { definition: def(method), error: { code, message: "x" } });
    expect(errorHint(failed(JobErrorCode.SOURCE_EMPTY))).toMatch(
      /turn on “Back up an empty folder” under Advanced/,
    );
    expect(errorTitle(failed(JobErrorCode.SOURCE_EMPTY))).toBe("The remote folder is empty");
    expect(errorTitle(failed(JobErrorCode.TOOL_UNAVAILABLE, MOUNT))).toBe(
      "The worker cannot mount remotes",
    );
    expect(errorHint(failed(JobErrorCode.TOOL_UNAVAILABLE, MOUNT))).toMatch(/Choose the mirror/);
    expect(errorTitle(failed(JobErrorCode.TOOL_UNAVAILABLE))).toBe(
      "The worker does not keep mirrors",
    );
    // Other jobs keep the code's label and get no hint.
    const plain = create(JobSchema, { error: { code: JobErrorCode.TOOL_UNAVAILABLE } });
    expect(errorTitle(plain)).toBe("Tool unavailable");
    expect(errorHint(plain)).toBeUndefined();
  });
});
