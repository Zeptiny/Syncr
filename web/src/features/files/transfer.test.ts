import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";

import type { SelectedItem } from "@/components/app/file-browser/selection";
import { WorkerStatus } from "@/gen/syncr/v1/api_pb";
import {
  FilterAction,
  ProbeEntrySchema,
  ProbeEntryType,
  RcloneTransferMode,
} from "@/gen/syncr/v1/link_pb";
import * as m5 from "@/mock/m5";
import * as fx from "@/test/fixtures";

import {
  moveText,
  planTransfer,
  transferDefinition,
  transferSummary,
  TWO_DISKS,
  workerRule,
  type Endpoint,
  type Place,
  type TransferSource,
} from "./transfer";

const now = Date.UTC(2026, 10, 10, 12);
const workers = m5.workers(now);
const remotes = fx.remotes(now);

const item = (path: string, dir = false): SelectedItem => ({
  path,
  entry: create(ProbeEntrySchema, {
    path,
    name: path.split(/[\\/]/).pop(),
    type: dir ? ProbeEntryType.DIR : ProbeEntryType.FILE,
  }),
});

const nas: Place = { kind: "worker", workerId: fx.ids.nas };
const backupHost: Place = { kind: "worker", workerId: fx.ids.backupHost };
const b2: Place = { kind: "remote", remoteId: fx.ids.b2 };
const minio: Place = { kind: "remote", remoteId: fx.ids.minio };
const photosOnNas: Place = {
  kind: "remote",
  remoteId: fx.ids.photos,
  local: true,
  workerId: fx.ids.nas,
};
const mediaDisk: Place = { kind: "remote", remoteId: fx.ids.media, local: true };

describe("workerRule", () => {
  it("runs on the worker whose disk holds either side", () => {
    expect(workerRule(nas, b2, workers)).toEqual({ kind: "pinned", workerId: fx.ids.nas });
    expect(workerRule(b2, nas, workers)).toEqual({ kind: "pinned", workerId: fx.ids.nas });
    expect(workerRule(nas, nas, workers)).toEqual({ kind: "pinned", workerId: fx.ids.nas });
    // A local remote listed by nas-01 is nas-01's disk.
    expect(workerRule(photosOnNas, minio, workers)).toEqual({
      kind: "pinned",
      workerId: fx.ids.nas,
    });
    // A local remote as the destination is the job's worker's disk: no pin of its own.
    expect(workerRule(nas, mediaDisk, workers)).toEqual({ kind: "pinned", workerId: fx.ids.nas });
  });

  it("refuses two workers' disks", () => {
    expect(workerRule(nas, backupHost, workers)).toEqual({ kind: "refused", message: TWO_DISKS });
    expect(
      workerRule({ ...photosOnNas, workerId: fx.ids.backupHost }, nas, workers),
    ).toHaveProperty("kind", "refused");
    expect(TWO_DISKS).toContain("Copy through a remote instead");
  });

  it("lets the user choose between remotes, the listing worker first", () => {
    const r = workerRule(b2, minio, workers, fx.ids.backupHost);
    expect(r.kind).toBe("choose");
    if (r.kind !== "choose") return;
    expect(r.proposed).toBe(fx.ids.backupHost);
    // Only online workers can run it.
    expect(r.workers.every((w) => w.status === WorkerStatus.ONLINE)).toBe(true);
    expect(r.workers.map((w) => w.id)).not.toContain(fx.ids.office);
    // An offline listing worker is not proposed: the first online one is.
    const off = workerRule(b2, minio, workers, fx.ids.office);
    expect(off.kind === "choose" && off.proposed).toBe(r.workers[0].id);
    expect(workerRule(b2, mediaDisk, workers).kind).toBe("choose");
  });
});

describe("planTransfer", () => {
  const src = (
    items: SelectedItem[],
    parent: string | undefined,
    place: Place = nas,
  ): TransferSource => ({
    place,
    items,
    parent,
  });
  const to = (place: Place, path: string): Endpoint => ({ place, path });
  const plan = (s: TransferSource, d: Endpoint | undefined, workerId = "") =>
    planTransfer(s, { destination: d, workerId, conflict: "keep" }, workers);

  it("reads the common parent and keeps the items with filters", () => {
    const r = plan(
      src([item("/srv/docs/contracts", true), item("/srv/docs/notes.md")], "/srv/docs"),
      to(b2, "inbox/"),
    );
    expect(r.errors).toEqual({});
    expect(r.plan).toEqual({
      workerId: fx.ids.nas,
      source: { place: nas, path: "/srv/docs" },
      destination: { place: b2, path: "inbox" },
      filters: ["+ /contracts/**", "+ /notes.md", "- **"],
    });
  });

  it("copies an allowed folder itself into a folder of its name", () => {
    const r = plan(src([item("/srv", true)], "/"), to(b2, "nas"));
    expect(r.plan).toMatchObject({
      source: { path: "/srv" },
      destination: { path: "nas/srv" },
      filters: [],
    });
    const two = plan(src([item("/srv", true), item("/mnt", true)], "/"), to(b2, "nas"));
    expect(two.errors.what).toMatch(/different allowed folders/);
  });

  it("refuses a folder copied into itself and the items' own folder", () => {
    const s = src([item("/srv/photos/2026", true), item("/srv/photos/README.txt")], "/srv/photos");
    expect(plan(s, to(nas, "/srv/photos/2026")).errors.destination).toBe(
      "A folder can't be copied into itself: 2026 is selected.",
    );
    expect(plan(s, to(nas, "/srv/photos/2026/01")).errors.destination).toMatch(/into itself/);
    expect(plan(s, to(nas, "/srv/photos/")).errors.destination).toBe(
      "The items are already in this folder. Choose another one.",
    );
    // The same path in another place is fine.
    expect(plan(s, to(b2, "srv/photos")).errors).toEqual({});
    expect(plan(s, to(nas, "/srv/backup")).errors).toEqual({});
  });

  it("sees a local remote and its worker's folders as one disk", () => {
    const withRemotes = (s: TransferSource, d: Endpoint) =>
      planTransfer(
        s,
        { destination: d, workerId: "", conflict: "keep" },
        workers,
        undefined,
        remotes,
      );
    const s = src([item("/srv/photos/2026", true), item("/srv/photos/README.txt")], "/srv/photos");
    // Photos is /srv/photos on nas-01's disk (listed by nas-01).
    expect(withRemotes(s, to(photosOnNas, "2026/01")).errors.destination).toMatch(/into itself/);
    expect(withRemotes(s, to(photosOnNas, "")).errors.destination).toMatch(
      /already in this folder/,
    );
    expect(withRemotes(s, to(photosOnNas, "2025")).errors).toEqual({});
    // Media disk has no worker of its own: the job's (nas-01, the source's) disk.
    const media = src([item("/mnt/media/films", true)], "/mnt/media");
    expect(withRemotes(media, to(mediaDisk, "films")).errors.destination).toMatch(/into itself/);
    // From the remote into the worker's folders, the other way round.
    const r = src([item("2026", true)], "", photosOnNas);
    expect(withRemotes(r, to(nas, "/srv/photos/2026/x")).errors.destination).toMatch(/into itself/);
    expect(withRemotes(r, to(nas, "/srv/photos")).errors.destination).toMatch(/already/);
    expect(withRemotes(r, to(nas, "/srv/other")).errors).toEqual({});
    // On another worker's disk it is another place (and refused as two disks).
    expect(
      withRemotes(s, to({ ...photosOnNas, workerId: fx.ids.backupHost }, "2026")).errors
        .destination,
    ).toBeUndefined();
  });

  it("asks for a destination, an absolute worker path, and one worker", () => {
    const s = src([item("/srv/docs/notes.md")], "/srv/docs");
    expect(plan(s, undefined).errors.destination).toBe("Choose the folder the items go into.");
    expect(plan(s, to(nas, "relative")).errors.destination).toMatch(/allowed folders/);
    expect(plan(s, to(backupHost, "/srv/backup")).errors.worker).toBe(TWO_DISKS);
    const remote = src([item("archive/2025", true)], "archive", minio);
    expect(plan(remote, to(b2, ""), fx.ids.backupHost).plan?.workerId).toBe(fx.ids.backupHost);
  });

  it("passes the filters' refusals on", () => {
    expect(plan(src([], undefined), to(b2, "")).errors.what).toBe("Select at least one item.");
  });
});

describe("transferDefinition", () => {
  const p = {
    workerId: fx.ids.nas,
    source: { place: nas, path: "/srv/docs" },
    destination: { place: b2, path: "inbox" },
    filters: ["+ /notes.md", "- **"],
  };
  const opts = (d: ReturnType<typeof transferDefinition>) => {
    const op = d.operation;
    if (op?.case !== "rcloneTransfer") throw new Error("not a transfer");
    return op.value;
  };

  it("maps the conflict choice to rclone's flags", () => {
    expect(opts(transferDefinition(p, "copy", "keep", false)).options).toMatchObject({
      ignoreExisting: true,
      update: false,
    });
    expect(opts(transferDefinition(p, "copy", "older", false)).options).toMatchObject({
      ignoreExisting: false,
      update: true,
    });
    expect(opts(transferDefinition(p, "copy", "differ", false)).options).toMatchObject({
      ignoreExisting: false,
      update: false,
    });
  });

  it("builds a copy, a preview and a move", () => {
    const copy = opts(transferDefinition(p, "copy", "keep", false));
    expect(copy).toMatchObject({
      mode: RcloneTransferMode.COPY,
      source: { remoteId: "", path: "/srv/docs" },
      destination: { remoteId: fx.ids.b2, path: "inbox" },
      options: {
        dryRun: false,
        createEmptySrcDirs: true,
        deleteEmptySrcDirs: false,
        filters: [
          { action: FilterAction.INCLUDE, pattern: "/notes.md" },
          { action: FilterAction.EXCLUDE, pattern: "**" },
        ],
      },
    });
    expect(opts(transferDefinition(p, "copy", "keep", true)).options?.dryRun).toBe(true);
    const move = opts(transferDefinition(p, "move", "keep", false));
    expect(move.mode).toBe(RcloneTransferMode.MOVE);
    expect(move.options?.deleteEmptySrcDirs).toBe(true);
  });

  it("summarises", () => {
    expect(
      transferSummary(
        "move",
        3,
        { place: nas, path: "/srv/docs" },
        { place: b2, path: "" },
        "older",
        workers,
        remotes,
      ),
    ).toBe(
      "Moves 3 items from /srv/docs on nas-01 into B2 offsite (its root) and replaces older files.",
    );
  });
});

describe("moveText", () => {
  it("says what each conflict choice leaves in the source", () => {
    expect(moveText("keep", "2 items", "/srv")).toBe(
      "2 items will be deleted from /srv once copied. Files already at the destination are skipped and stay in /srv.",
    );
    expect(moveText("differ", "2 items", "/srv")).toBe(
      "2 items will be deleted from /srv once copied.",
    );
    expect(moveText("older", "2 items", "/srv")).toMatch(
      /newer copy, it is kept and the older file in \/srv is deleted too\.$/,
    );
  });
});
