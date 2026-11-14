import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";

import type { SelectedItem } from "@/components/app/file-browser/selection";
import { WorkerSchema } from "@/gen/syncr/v1/api_pb";
import { ProbeEntrySchema, ProbeEntryType, ResticRemoteSourceMethod } from "@/gen/syncr/v1/link_pb";
import { FEATURE_REMOTE_SOURCE } from "@/lib/workers";

import {
  draftRunJob,
  draftSchedule,
  readJobDraft,
  remoteBackupDraft,
  type DraftTarget,
} from "./job-drafts";

const item = (path: string, dir = true): SelectedItem => ({
  path,
  entry: create(ProbeEntrySchema, { path, type: dir ? ProbeEntryType.DIR : ProbeEntryType.FILE }),
});

const target = (over: Partial<DraftTarget> = {}): DraftTarget => ({
  place: { kind: "remote", remoteId: "nc" },
  items: [],
  parent: undefined,
  folder: "Photos",
  workerId: "nas",
  ...over,
});

describe("Back up this folder… from the remote browser", () => {
  it("proposes the listing worker only if it can keep a mirror", () => {
    const worker = (id: string, features: string[], mirror?: boolean) =>
      create(WorkerSchema, {
        id,
        info: {
          features,
          remoteSources:
            mirror === undefined ? undefined : { mirror, mirrorReason: mirror ? "" : "off" },
        },
      });
    const workers = [
      worker("nas", [FEATURE_REMOTE_SOURCE], true),
      worker("old", ["probe"]),
      worker("off", [FEATURE_REMOTE_SOURCE], false),
    ];
    expect(remoteBackupDraft(target(), workers)?.workerId).toBe("nas");
    expect(remoteBackupDraft(target({ workerId: "old" }), workers)?.workerId).toBe("");
    expect(remoteBackupDraft(target({ workerId: "off" }), workers)?.workerId).toBe("");
    // Unknown workers (not loaded yet): the listing worker as before.
    expect(remoteBackupDraft(target({ workerId: "old" }))?.workerId).toBe("old");
  });

  it("backs up the open folder through a mirror, on the listing worker", () => {
    const draft = remoteBackupDraft(target());
    expect(draft?.kind).toBe("restic_backup");
    const parsed = readJobDraft({ jobDraft: draft });
    expect(parsed?.workerId).toBe("nas");
    const run = draftRunJob(parsed!);
    expect(run.workerId).toBe("nas");
    expect(run.definition.kind === "restic_backup" && run.definition.backup).toMatchObject({
      source: "remote",
      repositoryId: "",
      remote: { remoteId: "nc", path: "Photos", method: ResticRemoteSourceMethod.MIRROR },
    });
    // A schedule of it targets that worker (a backup of a remote never runs on a label).
    expect(draftSchedule(parsed!).target).toEqual({ kind: "worker", workerId: "nas", label: "" });
  });

  it("backs up one selected folder, or the remote's root", () => {
    const one = remoteBackupDraft(target({ items: [item("Photos/2026")] }));
    expect(readJobDraft({ jobDraft: one })?.definition.operation.value).toMatchObject({
      remoteSource: { path: "Photos/2026" },
    });
    const root = remoteBackupDraft(target({ folder: "" }));
    expect(readJobDraft({ jobDraft: root })?.definition.operation.value).toMatchObject({
      remoteSource: { remoteId: "nc", path: "" },
    });
  });

  it("names no folder for several items, a file, a worker or a Local directory remote", () => {
    expect(remoteBackupDraft(target({ items: [item("a"), item("b")] }))).toBeUndefined();
    expect(remoteBackupDraft(target({ items: [item("a.jpg", false)] }))).toBeUndefined();
    expect(
      remoteBackupDraft(target({ place: { kind: "worker", workerId: "nas" }, folder: "/srv" })),
    ).toBeUndefined();
    expect(
      remoteBackupDraft(target({ place: { kind: "remote", remoteId: "disk", local: true } })),
    ).toBeUndefined();
  });
});
