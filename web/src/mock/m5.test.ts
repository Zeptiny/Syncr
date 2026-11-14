import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";

import { JobService, JobStatus, WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import { AuthService } from "@/gen/syncr/v1/auth_pb";
import { ProbeEntryType } from "@/gen/syncr/v1/link_pb";
import { ProbeService } from "@/gen/syncr/v1/probe_pb";
import { RepositoryCreateMode, ResticService } from "@/gen/syncr/v1/restic_pb";
import { ScheduleService } from "@/gen/syncr/v1/schedules_pb";
import { fieldViolations } from "@/lib/errors";
import * as fx from "@/test/fixtures";

import * as m5 from "./m5";
import * as remoteBackup from "./remote-backup";
import { createMockTransport } from "./transport";

async function signIn(user: "admin" | "alice" = "admin") {
  const t = createMockTransport();
  await createClient(AuthService, t).login({ username: user, password: `syncr-demo-${user}` });
  return t;
}

async function violations(p: Promise<unknown>): Promise<Record<string, string>> {
  try {
    await p;
  } catch (err) {
    return Object.fromEntries(fieldViolations(err).map((v) => [v.field, v.message]));
  }
  throw new Error("the call did not fail");
}

async function code(p: Promise<unknown>): Promise<[Code, string]> {
  try {
    await p;
  } catch (err) {
    if (err instanceof ConnectError) return [err.code, err.rawMessage];
    throw err;
  }
  throw new Error("the call did not fail");
}

// The dev mock backs the screenshots; every M5 call must serialize and
// behave like the API where the UI depends on it.
describe("mock transport, milestone 5", { timeout: 20_000 }, () => {
  it("serves repositories, snapshots and trees", async () => {
    const t = await signIn();
    const restic = createClient(ResticService, t);
    const { repositories } = await restic.listRepositories({});
    expect(repositories.map((r) => r.name)).toEqual([
      "Archive 2019",
      "NAS documents",
      "Nextcloud photos",
      "Office laptop",
      "Old laptop",
      "Photos vault",
    ]);
    const vault = repositories.find((r) => r.id === m5.ids.photosVault)!;
    expect(vault).toMatchObject({
      path: "restic/photos",
      remote: { name: "B2 offsite", backendType: "b2" },
      deleting: false,
      stats: { snapshotCount: 30, scheduleCount: 3, snapshotsStale: false },
    });
    expect(vault.stats?.lastBackup?.status).toBe(JobStatus.SUCCEEDED);
    const old = repositories.find((r) => r.id === m5.ids.oldLaptop)!;
    expect(old).toMatchObject({ deleting: true, deletionJobId: m5.ids.oldLaptopDelete });
    const failed = repositories.find((r) => r.id === m5.ids.archive2019)!;
    expect(failed.deleting).toBe(false);
    expect(failed.deletionError).toMatch(/^deleting the data failed: /);
    expect((await restic.listRepositories({ remoteId: fx.ids.b2 })).repositories).toHaveLength(1);

    const snaps = await restic.listSnapshots({ repositoryId: m5.ids.photosVault });
    expect(snaps.snapshots).toHaveLength(30);
    expect(snaps.stale).toBe(false);
    expect(snaps.snapshots[0]).toMatchObject({ host: "nas-01", paths: ["/srv/photos"] });
    expect(snaps.snapshots[0].summary?.totalBytesProcessed).toBeGreaterThan(100n * 1024n ** 3n);
    const stale = await restic.listSnapshots({ repositoryId: m5.ids.nasDocs });
    expect(stale.stale).toBe(true);
    const fresh = await restic.listSnapshots({ repositoryId: m5.ids.nasDocs, refresh: true });
    expect(fresh).toMatchObject({ stale: false, worker: { name: "nas-01" } });

    // A snapshot made on Windows lists drive letters at its root.
    const office = await restic.listSnapshots({ repositoryId: m5.ids.officeLaptop });
    expect(office.snapshots[0].paths).toEqual(["C:\\Users\\ada\\Documents"]);
    const root = await restic.browseSnapshot({
      repositoryId: m5.ids.officeLaptop,
      snapshotId: office.snapshots[0].id,
      path: "",
    });
    expect(root.path).toBe("/");
    expect(root.entries.map((e) => [e.name, e.path, e.type])).toEqual([
      ["C", "/C", ProbeEntryType.DIR],
    ]);
    const docs = await restic.browseSnapshot({
      repositoryId: m5.ids.officeLaptop,
      snapshotId: office.snapshots[0].id,
      path: "/C/Users/ada/Documents",
    });
    expect(docs.entries.map((e) => e.name)).toContain("budget.xlsx");
    expect(
      await violations(
        restic.browseSnapshot({
          repositoryId: m5.ids.officeLaptop,
          snapshotId: office.snapshots[0].id,
          path: "/D",
        }),
      ),
    ).toEqual({ path: "does not exist" });
  });

  it("refuses work on a repository whose data is being deleted", async () => {
    const t = await signIn();
    const restic = createClient(ResticService, t);
    const jobs = createClient(JobService, t);
    const cached = await restic.listSnapshots({ repositoryId: m5.ids.oldLaptop });
    expect(cached.snapshots).toHaveLength(6);
    for (const call of [
      restic.listSnapshots({ repositoryId: m5.ids.oldLaptop, refresh: true }),
      restic.browseSnapshot({
        repositoryId: m5.ids.oldLaptop,
        snapshotId: cached.snapshots[0].id,
        path: "/",
      }),
      restic.updateRepository({ repositoryId: m5.ids.oldLaptop, version: 2n, name: "x" }),
      restic.deleteRepository({ repositoryId: m5.ids.oldLaptop }),
      jobs.runJob({
        workerId: fx.ids.nas,
        definition: {
          operation: { case: "resticCheck", value: { repositoryId: m5.ids.oldLaptop } },
        },
      }),
    ]) {
      const [c, message] = await code(call);
      expect(c).toBe(Code.FailedPrecondition);
      expect(message).toContain("data is being deleted");
    }
    // Asking for the deletion again returns the same job.
    const again = await restic.deleteRepository({
      repositoryId: m5.ids.oldLaptop,
      deleteData: true,
      confirmName: "Old laptop",
    });
    expect(again).toMatchObject({
      deletionJobId: m5.ids.oldLaptopDelete,
      worker: { name: "nas-01" },
    });
  });

  it("checks the delete confirmation and the kinds users may start", async () => {
    const t = await signIn();
    const restic = createClient(ResticService, t);
    const jobs = createClient(JobService, t);
    const sched = createClient(ScheduleService, t);
    expect(
      await violations(
        restic.deleteRepository({
          repositoryId: m5.ids.photosVault,
          deleteData: true,
          confirmName: "photos vault",
        }),
      ),
    ).toEqual({ confirm_name: "type the repository's name to confirm" });
    expect(
      (await restic.getRepository({ repositoryId: m5.ids.photosVault })).repository,
    ).toMatchObject({ deleting: false });
    expect(
      await violations(
        jobs.runJob({
          workerId: fx.ids.nas,
          definition: {
            operation: { case: "resticDeleteData", value: { repositoryId: m5.ids.photosVault } },
          },
        }),
      ),
    ).toEqual({ "definition.restic_delete_data": "is started by deleting the repository" });
    expect(
      await violations(
        jobs.runJob({
          workerId: fx.ids.nas,
          definition: {
            operation: {
              case: "resticBackup",
              value: { repositoryId: m5.ids.photosVault, paths: ["/srv"], useFsSnapshot: true },
            },
          },
        }),
      ),
    ).toHaveProperty(["definition.restic_backup.use_fs_snapshot"]);
    const nightly = (await sched.getSchedule({ scheduleId: m5.ids.nightlyBackup })).schedule!;
    expect(nightly.repository).toMatchObject({ id: m5.ids.photosVault, name: "Photos vault" });
    expect(
      await violations(
        sched.createSchedule({
          input: {
            name: "Restore nightly",
            cron: "0 2 * * *",
            timezone: "UTC",
            target: { target: { case: "workerId", value: fx.ids.nas } },
            definition: {
              operation: {
                case: "resticRestore",
                value: { repositoryId: m5.ids.photosVault, snapshotId: "x", target: "/srv/r" },
              },
            },
          },
        }),
      ),
    ).toEqual({ "definition.restic_restore": "restores cannot be scheduled" });
    const mine = await sched.listSchedules({ repositoryId: m5.ids.photosVault });
    expect(mine.schedules.map((s) => s.name)).toEqual([
      "Monthly deep check: photos",
      "Nightly photos backup",
      "Weekly retention: photos",
    ]);
    expect(
      (await sched.listSchedules({ repositoryId: m5.ids.oldLaptop })).schedules[0],
    ).toMatchObject({ enabled: false, pausedReason: "repository_deleting" });
  });

  it("creates and connects repositories with the API's field errors", async () => {
    const t = await signIn();
    const restic = createClient(ResticService, t);
    const input = {
      name: "Legacy",
      remoteId: fx.ids.minio,
      path: "restic/legacy",
      password: "correct horse",
      mode: RepositoryCreateMode.CONNECT,
    };
    expect(
      await violations(restic.createRepository({ input: { ...input, password: "wrong" } })),
    ).toEqual({ password: "is not this repository's password" });
    expect(
      await violations(restic.createRepository({ input: { ...input, path: "restic/none" } })),
    ).toEqual({ path: "no restic repository at this location" });
    expect(
      await violations(
        restic.createRepository({ input: { ...input, mode: RepositoryCreateMode.INIT } }),
      ),
    ).toEqual({ mode: "a repository already exists here; choose Connect" });
    expect(
      await violations(restic.createRepository({ input: { ...input, remoteId: fx.ids.pooled } })),
    ).toEqual({ remote_id: "this kind of remote cannot hold a restic repository" });
    expect(
      await violations(
        restic.createRepository({ input: { ...input, remoteId: fx.ids.media, path: "repo" } }),
      ),
    ).toEqual({ worker_id: "choose the worker whose disk holds this repository" });
    expect(
      await code(restic.createRepository({ input: { ...input, name: "photos VAULT" } })),
    ).toEqual([Code.AlreadyExists, "a repository with this name already exists"]);
    expect(
      await code(restic.createRepository({ input: { ...input, path: "restic/nas-docs" } })),
    ).toEqual([Code.AlreadyExists, "a repository at this location already exists"]);
    expect(
      await code(restic.createRepository({ input: { ...input, workerId: fx.ids.office } })),
    ).toEqual([Code.Unavailable, "worker office-pc is offline"]);

    const res = await restic.createRepository({ input });
    expect(res.worker?.name).toBe("nas-01");
    expect(res.repository).toMatchObject({
      name: "Legacy",
      path: "restic/legacy",
      stats: { snapshotCount: 0, snapshotsStale: true },
    });
    // The response never carries the password.
    expect(
      JSON.stringify(res, (_k, v: unknown) => (typeof v === "bigint" ? String(v) : v)),
    ).not.toContain("correct horse");
    expect(
      await code(
        restic.updateRepository({ repositoryId: res.repository!.id, version: 9n, name: "x" }),
      ),
    ).toEqual([Code.Aborted, "the repository was changed in the meantime"]);
  });

  it("serves probes: worker folders, remote folders and remote tests", async () => {
    const t = await signIn();
    const probe = createClient(ProbeService, t);
    const roots = await probe.listWorkerFiles({ workerId: fx.ids.nas, path: "" });
    expect(roots.entries.map((e) => e.path)).toEqual(["/srv", "/mnt"]);
    const srv = await probe.listWorkerFiles({ workerId: fx.ids.nas, path: "/srv" });
    expect(srv.entries.map((e) => e.path)).toEqual([
      "/srv/photos",
      "/srv/docs",
      "/srv/shared",
      "/srv/backup",
    ]);
    const months = await probe.listWorkerFiles({
      workerId: fx.ids.nas,
      path: "/srv/photos/2026",
      dirsOnly: true,
    });
    expect(months.entries).toHaveLength(11);
    expect(await violations(probe.listWorkerFiles({ workerId: fx.ids.nas, path: "/etc" }))).toEqual(
      { path: "is outside the worker's allowed folders" },
    );
    expect(await code(probe.listWorkerFiles({ workerId: fx.ids.office, path: "" }))).toEqual([
      Code.Unavailable,
      "worker office-pc is offline",
    ]);
    const [outdated] = await code(probe.listWorkerFiles({ workerId: m5.ids.oldNas, path: "" }));
    expect(outdated).toBe(Code.FailedPrecondition);

    const minio = await probe.listRemoteFiles({ remoteId: fx.ids.minio, path: "" });
    expect(minio.entries.map((e) => e.path)).toEqual(["restic", "archive"]);
    expect(
      (await probe.listRemoteFiles({ remoteId: fx.ids.minio, path: "archive" })).entries[0].path,
    ).toBe("archive/2025");
    expect(await violations(probe.listRemoteFiles({ remoteId: fx.ids.media, path: "" }))).toEqual({
      worker_id: "choose the worker whose disk holds this remote",
    });

    const b2 = await probe.testRemote({ remoteId: fx.ids.b2 });
    expect(b2).toMatchObject({ ok: true, worker: { name: "nas-01" }, entries: 2 });
    expect(b2.usage?.total).toBe(1024n ** 4n);
    expect((await probe.testRemote({ remoteId: fx.ids.minio })).usage?.unsupported).toBe(true);
    const nextcloud = await probe.testRemote({ remoteId: fx.ids.nextcloud });
    expect(nextcloud.ok).toBe(false);
    expect(nextcloud.error).toContain("connection refused");
    expect(await probe.testRemote({ remoteId: fx.ids.b2, workerId: fx.ids.office })).toMatchObject({
      ok: false,
      errorCode: "unavailable",
    });
  });

  it("serves restic jobs with titles, outputs, details and a waiting job", async () => {
    const t = await signIn();
    const jobs = createClient(JobService, t);
    const workers = await createClient(WorkerAdminService, t).listWorkers({});
    const nas = workers.workers.find((w) => w.id === fx.ids.nas)!;
    // The hooks, two-way sync, browser and remote-backup mocks add their
    // features to the up-to-date workers.
    expect(nas.info?.features).toEqual([
      "probe",
      "restic.v2",
      "restic.delete",
      "job.hooks",
      "rclone.bisync",
      "restic.restore_subfolder_paths",
      "restic.remote_source",
    ]);
    // Milestone 6 adds sftp_host_keys to the seven probe operations of m5,
    // and milestone 7 its five scans (sizes, search, compare).
    expect(nas.info?.probeOps).toHaveLength(13);
    expect(nas.info?.probeOps.slice(7)).toEqual([
      "sftp_host_keys",
      "measure_local",
      "measure_remote",
      "restic_measure",
      "restic_find",
      "restic_diff",
    ]);
    expect(workers.workers.find((w) => w.id === m5.ids.oldNas)?.info?.features).toEqual([]);

    const waiting = (await jobs.getJob({ jobId: m5.ids.waitingBackup })).job!;
    expect(waiting).toMatchObject({
      kind: "restic_backup",
      status: JobStatus.QUEUED,
      waitingForJobId: m5.ids.runningBackup,
      title: "Back up /srv/backup/db → NAS documents",
      repository: { name: "NAS documents", deleted: false },
    });
    const backup = (await jobs.getJob({ jobId: m5.ids.lastBackup })).job!;
    expect(backup.output?.output.case).toBe("resticBackup");
    expect(backup.details.map((d) => d.label)).toEqual(["Snapshot", "Files", "Data added"]);
    const del = (await jobs.getJob({ jobId: m5.ids.retiredDelete })).job!;
    expect(del).toMatchObject({
      kind: "restic_delete_data",
      title: "Delete the data of Retired NAS",
      repository: { deleted: true },
    });
    expect(del.output?.output).toMatchObject({
      case: "resticDeleteData",
      value: { directoryRemoved: true, alreadyGone: false },
    });

    const mine = await jobs.listJobs({ repositoryId: m5.ids.oldLaptop });
    expect(mine.jobs.map((j) => j.title)).toEqual([
      "Delete the data of Old laptop",
      "Check Old laptop",
    ]);
    expect(mine.jobs[0].waitingForJobId).toBe(m5.ids.oldLaptopCheck);
    for (const kind of [
      "restic_backup",
      "restic_forget",
      "restic_check",
      "restic_restore",
      "restic_delete_data",
    ]) {
      const page = await jobs.listJobs({ kinds: [kind], pageSize: 5 });
      expect(page.jobs.length).toBeGreaterThan(0);
      expect(page.jobs.every((j) => j.kind === kind && j.title !== "")).toBe(true);
    }
    const warned = await jobs.listJobs({ kinds: ["restic_backup"], pageSize: 100 });
    // The second is the remote-backup mock's empty Inbox, which made no snapshot.
    expect(warned.jobs.flatMap((j) => j.warnings).sort()).toEqual([
      "3 items could not be read",
      remoteBackup.messages.emptyWarning,
    ]);
    const failed = (await jobs.getJob({ jobId: m5.ids.failedCheck })).job!;
    expect(failed.error?.message).toContain("the check found 2 error(s)");
  });

  it("shows another user only their own repositories", async () => {
    const t = await signIn("alice");
    const restic = createClient(ResticService, t);
    const { repositories } = await restic.listRepositories({});
    expect(repositories.map((r) => r.name)).toEqual(["Alice's backups"]);
    expect(repositories[0].remote?.name).toBe("Alice's B2");
    expect((await code(restic.getRepository({ repositoryId: m5.ids.photosVault })))[0]).toBe(
      Code.NotFound,
    );
  });
});
