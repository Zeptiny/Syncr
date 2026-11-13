import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";

import { RunJobRequestSchema, WorkerStatus } from "@/gen/syncr/v1/api_pb";
import { ResticRestoreOverwrite } from "@/gen/syncr/v1/link_pb";
import { DeleteRepositoryRequestSchema, RepositorySchema } from "@/gen/syncr/v1/restic_pb";
import * as m5 from "@/mock/m5";
import * as fx from "@/test/fixtures";

import {
  canDelete,
  defaultRestoreTarget,
  deletionFailed,
  emptyRestore,
  generatePassword,
  locationText,
  pickRestoreWorker,
  restoreSummary,
  shortId,
  toDeleteRequest,
  toRestoreRequest,
  validateRestore,
  originalParent,
} from "./model";
import {
  validateNewRepositorySearch,
  validateRepositorySearch,
  validateSnapshotSearch,
} from "./search";

const now = Date.UTC(2026, 10, 10, 12);
const date = new Date(2026, 10, 10, 9, 30);
const id = "4f3c2b1a9e8d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d9c8b7a6f5e4d3c2b";

describe("repository model", () => {
  it("shortens IDs to eight characters", () => {
    expect(shortId(id)).toBe("4f3c2b1a");
    expect(shortId("abc")).toBe("abc");
  });

  it("proposes a dated restore folder in the first allowed folder", () => {
    expect(defaultRestoreTarget(["/srv", "/mnt"], id, date, "linux")).toBe(
      "/srv/restore-4f3c2b1a-20261110",
    );
    expect(defaultRestoreTarget(["/srv/"], id, date, "linux")).toBe(
      "/srv/restore-4f3c2b1a-20261110",
    );
    expect(defaultRestoreTarget(["D:\\Restore"], id, date, "windows")).toBe(
      "D:\\Restore\\restore-4f3c2b1a-20261110",
    );
    expect(defaultRestoreTarget(["D:\\"], id, new Date(2026, 0, 5), "windows")).toBe(
      "D:\\restore-4f3c2b1a-20260105",
    );
    expect(defaultRestoreTarget([], id, date, "linux")).toBe("");
  });

  it("picks the online worker with the snapshot's host name, else the first usable one", () => {
    const workers = m5.workers(now);
    expect(pickRestoreWorker(workers, "nas-01")?.name).toBe("nas-01");
    expect(pickRestoreWorker(workers, "BACKUP-HOST")?.name).toBe("backup-host");
    // office-pc is offline, and old-nas cannot run restic: the first usable by name.
    expect(pickRestoreWorker(workers, "OFFICE-PC")?.name).toBe("backup-host");
    expect(pickRestoreWorker(workers, "old-nas")?.name).toBe("backup-host");
    expect(pickRestoreWorker(workers, "")?.name).toBe("backup-host");
    const office = workers.find((w) => w.id === fx.ids.office)!;
    office.status = WorkerStatus.ONLINE;
    expect(pickRestoreWorker(workers, "office-pc")?.name).toBe("office-pc");
    expect(pickRestoreWorker([], "nas-01")).toBeUndefined();
    expect(pickRestoreWorker(fx.workers(now), "nas-01")).toBeUndefined();
  });

  it("summarizes a restore in one line", () => {
    const keep = { target: "/srv/restore", overwrite: ResticRestoreOverwrite.NEVER };
    expect(restoreSummary({ mode: "items", paths: ["/a", "/b", "/c"] }, keep, "nas-01")).toBe(
      "Restores 3 items into /srv/restore on nas-01 and keeps existing files.",
    );
    expect(restoreSummary({ mode: "items", paths: ["/a"] }, keep, "nas-01")).toMatch(
      /^Restores 1 item into/,
    );
    expect(
      restoreSummary(
        { mode: "folder", folder: "/srv/photos/2026" },
        { target: "/srv/photos/2026", overwrite: ResticRestoreOverwrite.IF_CHANGED },
        "nas-01",
      ),
    ).toBe(
      "Restores the contents of /srv/photos/2026 into /srv/photos/2026 on nas-01 and replaces files that differ.",
    );
    expect(
      restoreSummary(
        { mode: "snapshot" },
        { target: " ", overwrite: ResticRestoreOverwrite.ALWAYS },
        undefined,
      ),
    ).toBe(
      "Restores the whole snapshot into a folder you still have to choose and replaces all files.",
    );
    expect(
      restoreSummary(
        { mode: "snapshot" },
        { target: "/x", overwrite: ResticRestoreOverwrite.IF_NEWER },
        "w",
      ),
    ).toMatch(/replaces older files\.$/);
  });

  it("keeps existing files unless the user chooses otherwise", () => {
    const s = { ...emptyRestore(), workerId: "w1", target: " /srv/restore " };
    expect(s.overwrite).toBe(ResticRestoreOverwrite.NEVER);
    const whole = create(
      RunJobRequestSchema,
      toRestoreRequest("repo-1", { id }, { mode: "snapshot" }, s),
    );
    expect(whole.workerId).toBe("w1");
    expect(whole.definition?.operation).toMatchObject({
      case: "resticRestore",
      value: {
        repositoryId: "repo-1",
        snapshotId: id,
        target: "/srv/restore",
        paths: [],
        subfolder: "",
        verify: false,
        overwrite: ResticRestoreOverwrite.NEVER,
      },
    });
    // An unset value is sent as NEVER too, never as something weaker.
    const unset = toRestoreRequest(
      "r",
      { id },
      { mode: "snapshot" },
      {
        ...s,
        overwrite: ResticRestoreOverwrite.UNSPECIFIED,
      },
    );
    expect(unset.definition?.operation?.value).toMatchObject({
      overwrite: ResticRestoreOverwrite.NEVER,
    });
    const items = toRestoreRequest(
      "r",
      { id },
      { mode: "items", paths: ["/a/b", "/c"] },
      {
        ...s,
        overwrite: ResticRestoreOverwrite.ALWAYS,
        verify: true,
        excludes: ["*.tmp", " ", "*.tmp"],
      },
    );
    expect(items.definition?.operation?.value).toMatchObject({
      paths: ["/a/b", "/c"],
      subfolder: "",
      overwrite: ResticRestoreOverwrite.ALWAYS,
      verify: true,
      excludes: ["*.tmp"],
    });
    const folder = toRestoreRequest("r", { id }, { mode: "folder", folder: "/srv/x" }, s);
    expect(folder.definition?.operation?.value).toMatchObject({ paths: [], subfolder: "/srv/x" });
  });

  it("validates a restore", () => {
    expect(validateRestore({ mode: "snapshot" }, emptyRestore())).toEqual({
      worker: "Choose the worker that receives the files.",
      target: "Choose the folder to restore into.",
    });
    const s = { ...emptyRestore(), workerId: "w", target: "restore" };
    expect(validateRestore({ mode: "items", paths: [] }, s)).toEqual({
      target: "Enter an absolute path on the worker.",
      what: "Select at least one item.",
    });
    expect(validateRestore({ mode: "snapshot" }, { ...s, target: "D:\\Restore" })).toEqual({});
  });

  it("maps the delete dialog to the request exactly", () => {
    const repo = create(RepositorySchema, {
      id: "repo-1",
      name: "Photos vault",
      remote: { id: "r", name: "B2 offsite", backendType: "b2" },
      path: "restic/photos",
    });
    // Forget only: the ID and nothing else, whatever else the form holds.
    const forget = create(
      DeleteRepositoryRequestSchema,
      toDeleteRequest(repo, { deleteData: false, typedName: "Photos vault", workerId: "w1" }),
    );
    expect(forget).toEqual(create(DeleteRepositoryRequestSchema, { repositoryId: "repo-1" }));
    expect(forget.deleteData).toBe(false);
    expect(forget.confirmName).toBe("");
    expect(forget.workerId).toBe("");

    const data = create(
      DeleteRepositoryRequestSchema,
      toDeleteRequest(repo, { deleteData: true, typedName: "Photos vault", workerId: "w1" }),
    );
    expect(data).toEqual(
      create(DeleteRepositoryRequestSchema, {
        repositoryId: "repo-1",
        deleteData: true,
        confirmName: "Photos vault",
        workerId: "w1",
      }),
    );
    // Automatic worker: an empty worker_id.
    expect(
      toDeleteRequest(repo, { deleteData: true, typedName: "Photos vault", workerId: "" }),
    ).toEqual({
      repositoryId: "repo-1",
      deleteData: true,
      confirmName: "Photos vault",
      workerId: "",
    });

    expect(canDelete(repo, { deleteData: false, typedName: "", workerId: "" })).toBe(true);
    expect(canDelete(repo, { deleteData: true, typedName: "", workerId: "" })).toBe(false);
    expect(canDelete(repo, { deleteData: true, typedName: "photos vault", workerId: "" })).toBe(
      false,
    );
    expect(canDelete(repo, { deleteData: true, typedName: "Photos vault", workerId: "" })).toBe(
      true,
    );
    const local = create(RepositorySchema, {
      id: "repo-1",
      name: "Photos vault",
      remote: { id: "r", name: "Media disk", backendType: "local" },
    });
    expect(canDelete(local, { deleteData: true, typedName: "Photos vault", workerId: "" })).toBe(
      false,
    );
    expect(canDelete(local, { deleteData: true, typedName: "Photos vault", workerId: "w1" })).toBe(
      true,
    );
    expect(canDelete(local, { deleteData: false, typedName: "", workerId: "" })).toBe(true);

    expect(locationText(repo)).toBe("B2 offsite: restic/photos");
    expect(
      locationText(create(RepositorySchema, { remote: { name: "B2 offsite" }, path: "" })),
    ).toBe("B2 offsite (its root)");
    expect(deletionFailed(create(RepositorySchema, { deletionError: "x" }))).toBe(true);
    expect(deletionFailed(create(RepositorySchema, { deletionError: "x", deleting: true }))).toBe(
      false,
    );
  });

  it("generates passwords of the asked length from the safe alphabet", () => {
    const a = generatePassword();
    const b = generatePassword();
    expect(a).toMatch(/^[A-HJ-NP-Za-km-z2-9]{24}$/);
    expect(a).not.toBe(b);
    expect(generatePassword(40)).toHaveLength(40);
  });

  it("keeps the password and other junk out of URL state", () => {
    expect(validateRepositorySearch({ tab: "settings", password: "x" })).toEqual({
      tab: "settings",
    });
    expect(validateRepositorySearch({ tab: "snapshots" })).toEqual({ tab: undefined });
    expect(validateRepositorySearch({ tab: "nope" })).toEqual({ tab: undefined });
    expect(validateNewRepositorySearch({ remote: "r1", password: "x", path: "/p/q/" })).toEqual({
      remote: "r1",
      path: "p/q",
    });
    // A path means nothing without its remote.
    expect(validateNewRepositorySearch({ path: "p" })).toEqual({});
    expect(validateSnapshotSearch({ path: "/srv/photos" })).toEqual({ path: "/srv/photos" });
    expect(validateSnapshotSearch({ path: "/" })).toEqual({ path: undefined });
    expect(validateSnapshotSearch({ path: "relative" })).toEqual({ path: undefined });
  });
});

describe("restore to the original location", () => {
  it("restores the items' common parent", () => {
    expect(originalParent(["/srv/photos/a.jpg"])).toBe("/srv/photos");
    expect(originalParent(["/srv/photos/2026/01", "/srv/docs/x"])).toBe("/srv");
    expect(originalParent(["/srv", "/home"])).toBe("/");
  });
});

describe("restore request to the original location", () => {
  it("sends the items of their common parent", () => {
    const req = toRestoreRequest(
      m5.ids.photosVault,
      { id: "1a2b3c4d" },
      { mode: "original", paths: ["/srv/photos/2026/01", "/srv/photos/README.txt"] },
      { ...emptyRestore(), workerId: fx.ids.nas, target: "/srv/photos", excludes: ["*.tmp"] },
    );
    expect(req.definition?.operation).toMatchObject({
      case: "resticRestore",
      value: {
        target: "/srv/photos",
        subfolder: "/srv/photos",
        paths: ["/srv/photos/2026/01", "/srv/photos/README.txt"],
        excludes: ["*.tmp"],
      },
    });
  });
});
