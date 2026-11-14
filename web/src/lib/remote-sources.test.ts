import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";

import { WorkerSchema, WorkerStatus } from "@/gen/syncr/v1/api_pb";
import { ResticRemoteSourceMethod } from "@/gen/syncr/v1/link_pb";

import {
  abilitiesText,
  methodProblem,
  methodRefusal,
  phaseLabel,
  remoteTagId,
  workerAbilities,
} from "./remote-sources";

const MIRROR = ResticRemoteSourceMethod.MIRROR;
const MOUNT = ResticRemoteSourceMethod.MOUNT;

const worker = (
  name: string,
  info?: { features?: string[]; rs?: [boolean, boolean, string, string] },
) =>
  create(WorkerSchema, {
    id: name,
    name,
    status: WorkerStatus.ONLINE,
    info: info && {
      features: info.features ?? ["restic.remote_source"],
      remoteSources: info.rs && {
        mirror: info.rs[0],
        mount: info.rs[1],
        mirrorReason: info.rs[2],
        mountReason: info.rs[3],
      },
    },
  });

// As the dev seed's workers.
const nas = worker("nas-01", { rs: [true, true, "", ""] });
const backupHost = worker("backup-host", { rs: [true, false, "", "/dev/fuse is missing"] });
const office = worker("office-pc", {
  rs: [false, false, "remote_sources.mirror is off in worker.yaml", "WinFsp is not installed"],
});
const attic = worker("attic-nas", { features: ["probe", "restic.v2"] });
const laptop = worker("laptop");

describe("what workers can do for backups of a remote", () => {
  it("names the problem of each method for the worker pickers", () => {
    expect(methodProblem(nas, MOUNT)).toBeUndefined();
    expect(methodProblem(backupHost, MIRROR)).toBeUndefined();
    expect(methodProblem(backupHost, MOUNT)).toBe("can't mount: /dev/fuse is missing");
    expect(methodProblem(office, MIRROR)).toBe(
      "can't mirror: remote_sources.mirror is off in worker.yaml",
    );
    // Without the feature: outdated, whatever the method.
    expect(methodProblem(attic, MIRROR)).toBe("outdated");
    // A worker that never reported is left to the controller.
    expect(methodProblem(laptop, MOUNT)).toBeUndefined();
  });

  it("words a refusal as RunJob does", () => {
    expect(methodRefusal(backupHost, MOUNT)).toBe(
      "backup-host cannot mount a remote: /dev/fuse is missing.",
    );
    expect(methodRefusal(office, MIRROR)).toBe(
      "office-pc cannot keep a mirror of a remote: remote_sources.mirror is off in worker.yaml.",
    );
    expect(methodRefusal(worker("x", { rs: [true, false, "", ""] }), MOUNT)).toBe(
      "x cannot mount a remote: it gave no reason.",
    );
    expect(methodRefusal(attic, MOUNT)).toBe(
      "attic-nas must be upgraded to back up a folder of a remote.",
    );
    expect(methodRefusal(nas, MOUNT)).toBeUndefined();
    expect(methodRefusal(undefined, MOUNT)).toBeUndefined();
  });

  it("lists a worker's report, and nothing for one that reported none", () => {
    const rows = workerAbilities(backupHost);
    expect(rows && abilitiesText(rows)).toBe("Mirror ✓ · Mount ✗ – /dev/fuse is missing");
    expect(workerAbilities(attic)).toBeUndefined();
    expect(workerAbilities(laptop)).toBeUndefined();
  });

  it("reads the snapshot tag and the live phase", () => {
    expect(remoteTagId("syncr-remote:abc")).toBe("abc");
    expect(remoteTagId("syncr-remote:")).toBeUndefined();
    expect(remoteTagId("photos")).toBeUndefined();
    expect(phaseLabel("mirror")).toBe("Mirroring…");
    expect(phaseLabel("backup")).toBe("Backing up…");
    expect(phaseLabel("")).toBe("");
  });
});
