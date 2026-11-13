import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";

import { ProbeService } from "@/gen/syncr/v1/probe_pb";
import { ResticService } from "@/gen/syncr/v1/restic_pb";
import { fieldViolations } from "@/lib/errors";
import { isBusy } from "@/lib/scans";
import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";

import * as m5 from "./m5";

const now = Date.UTC(2026, 10, 10, 12);

async function failure(p: Promise<unknown>): Promise<[Code, Record<string, string>, string]> {
  try {
    await p;
  } catch (err) {
    if (!(err instanceof ConnectError)) throw err;
    const fields = Object.fromEntries(fieldViolations(err).map((v) => [v.field, v.message]));
    return [err.code, fields, err.rawMessage];
  }
  throw new Error("the call did not fail");
}

// The scans answer like the seeded controller (internal/devseed/scans.go).
describe("mock scans", () => {
  const setup = () => {
    const api = m5Api(now);
    return {
      api,
      probe: createClient(ProbeService, api.transport),
      restic: createClient(ResticService, api.transport),
      snaps: (id: string) => api.repo(id).snapshots,
    };
  };

  it("measures worker folders, one stopping early, with unreadable entries", async () => {
    const { probe } = setup();
    const photos = (await probe.measureWorkerFolder({ workerId: fx.ids.nas, path: "/srv/photos" }))
      .measurement!;
    expect(photos).toMatchObject({ complete: true, unreadable: 0n, files: 243n, dirs: 25n });
    expect(photos.children.map((c) => c.name)).toEqual(["2025", "2026"]); // largest first
    expect(photos.children.reduce((n, c) => n + c.bytes, 0n)).toBeLessThan(photos.bytes);
    const mnt = await probe.measureWorkerFolder({ workerId: fx.ids.nas, path: "/mnt" });
    expect(mnt.measurement).toMatchObject({ complete: false, unreadable: 1n });
    expect(mnt.elapsed?.seconds).toBe(40n);
    expect(
      (await probe.measureWorkerFolder({ workerId: fx.ids.nas, path: "/srv" })).measurement
        ?.unreadable,
    ).toBe(1n);
    expect(
      await failure(probe.measureWorkerFolder({ workerId: fx.ids.nas, path: "/srv/nope" })),
    ).toEqual([Code.InvalidArgument, { path: "does not exist" }, expect.any(String)]);
    expect(
      (await failure(probe.measureWorkerFolder({ workerId: fx.ids.nas, path: "/etc" })))[1],
    ).toEqual({
      path: "is outside the worker's allowed folders",
    });
    expect(
      (
        await failure(
          probe.measureWorkerFolder({ workerId: fx.ids.nas, path: "/srv/docs/notes.md" }),
        )
      )[0],
    ).toBe(Code.FailedPrecondition);
    // old-nas lists nothing and measures nothing.
    expect(
      (await failure(probe.measureWorkerFolder({ workerId: m5.ids.oldNas, path: "/volume1" })))[0],
    ).toBe(Code.FailedPrecondition);
  });

  it("refuses a second scan on the same worker as busy", async () => {
    const { api, probe, restic } = setup();
    api.busy.add(fx.ids.nas);
    const err = await failure(probe.measureWorkerFolder({ workerId: fx.ids.nas, path: "/srv" }));
    expect(err[0]).toBe(Code.Unavailable);
    expect(err[2]).toMatch(
      /^measure the worker's folder: unavailable: worker "nas-01" is already measuring or searching/,
    );
    const busy = restic.findInRepository({ repositoryId: m5.ids.photosVault, patterns: ["x"] });
    await expect(busy).rejects.toSatisfy(isBusy);
  });

  it("measures remotes, which need the worker of a local one", async () => {
    const { probe } = setup();
    const root = (await probe.measureRemoteFolder({ remoteId: fx.ids.minio, path: "" }))
      .measurement!;
    expect(root.children.map((c) => c.name)).toEqual(["archive", "restic"]);
    expect(
      (await failure(probe.measureRemoteFolder({ remoteId: fx.ids.photos, path: "" })))[1],
    ).toEqual({
      worker_id: "choose the worker whose disk holds this remote",
    });
  });

  it("searches snapshots that differ: versions, a cut, one snapshot", async () => {
    const { restic, snaps } = setup();
    const list = snaps(m5.ids.photosVault);
    const albums = await restic.findInRepository({
      repositoryId: m5.ids.photosVault,
      patterns: ["albums.json"],
      ignoreCase: true,
    });
    expect(albums).toMatchObject({ truncated: false, complete: true, snapshotsSearched: 30 });
    expect(new Set(albums.matches.map((m) => m.entry?.size)).size).toBeGreaterThan(5);
    const jpg = await restic.findInRepository({
      repositoryId: m5.ids.photosVault,
      patterns: ["*.jpg"],
    });
    expect(jpg).toMatchObject({ truncated: true, complete: false });
    expect(jpg.matches).toHaveLength(1000);
    const one = await restic.findInRepository({
      repositoryId: m5.ids.photosVault,
      patterns: ["/srv/photos/2026"],
      snapshotIds: [list[3].id.slice(0, 8)],
    });
    // A folder's match brings its subtree.
    expect(one.snapshotsSearched).toBe(1);
    expect(one.matches.length).toBeGreaterThan(100);
    expect(
      (
        await failure(restic.findInRepository({ repositoryId: m5.ids.photosVault, patterns: [] }))
      )[1],
    ).toEqual({
      patterns: "must have at least one pattern",
    });
    expect(
      (
        await failure(
          restic.findInRepository({
            repositoryId: m5.ids.photosVault,
            patterns: ["a"],
            snapshotIds: ["deadbeef"],
          }),
        )
      )[1],
    ).toEqual({ snapshot_ids: "deadbeef is not a snapshot of this repository" });
  });

  it("compares snapshots and refuses what the API refuses", async () => {
    const { restic, snaps } = setup();
    const list = snaps(m5.ids.nasDocs);
    const diff = await restic.compareSnapshots({
      repositoryId: m5.ids.nasDocs,
      fromSnapshotId: list[1].id,
      toSnapshotId: list[0].id,
    });
    expect(diff).toMatchObject({ complete: true, truncated: false });
    expect(diff.stats).toBeDefined();
    const same = restic.compareSnapshots({
      repositoryId: m5.ids.nasDocs,
      fromSnapshotId: list[0].id,
      toSnapshotId: list[0].id.slice(0, 8),
    });
    expect((await failure(same))[1]).toEqual({
      to_snapshot_id: "must be another snapshot than from_snapshot_id",
    });
    const missing = restic.compareSnapshots({
      repositoryId: m5.ids.nasDocs,
      fromSnapshotId: list[1].id,
      toSnapshotId: list[0].id,
      path: "/srv/docs/notes.md",
    });
    expect((await failure(missing))[1]).toEqual({ path: "is not a folder in both snapshots" });
  });
});
