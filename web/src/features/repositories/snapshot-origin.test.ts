import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";

import { WorkerSchema, WorkerStatus } from "@/gen/syncr/v1/api_pb";
import { ResticSnapshotSchema } from "@/gen/syncr/v1/link_pb";

import { hostWorker, inPlaceParent, outdatedHost } from "./model";
import { filterSnapshots, noFilter } from "./snapshot-groups";
import {
  isRemoteSnapshot,
  originText,
  remoteNames,
  remoteOrigin,
  tagLabel,
  userTags,
} from "./snapshot-origin";

const names = remoteNames([{ id: "nc", name: "Nextcloud" }]);
const remote = create(ResticSnapshotSchema, {
  id: "aaaa",
  host: "nas-01",
  paths: ["/var/lib/syncr/sources/mirror/0123"],
  tags: ["phone", "syncr-remote:nc"],
});
const foreign = create(ResticSnapshotSchema, {
  id: "bbbb",
  host: "nas-01",
  paths: ["/var/lib/syncr/sources/mount/0123"],
  tags: ["syncr-remote:someone-elses"],
});
const folders = create(ResticSnapshotSchema, {
  id: "cccc",
  host: "nas-01",
  paths: ["/srv/photos"],
  tags: ["photos"],
});

describe("snapshots of a remote", () => {
  it("are named after their remote instead of their internal paths", () => {
    expect(remoteOrigin(remote, names)).toBe("From Nextcloud");
    expect(remoteOrigin(foreign, names)).toBe("From a remote");
    expect(remoteOrigin(folders, names)).toBeUndefined();
    expect(originText(remote, names)).toBe("From Nextcloud");
    expect(originText(folders, names)).toBe("/srv/photos");
    expect(isRemoteSnapshot(remote)).toBe(true);
    expect(isRemoteSnapshot(folders)).toBe(false);
  });

  it("show the remote tag as a badge, not among the tags", () => {
    expect(userTags(remote)).toEqual(["phone"]);
    expect(tagLabel("syncr-remote:nc", names)).toBe("From Nextcloud");
    expect(tagLabel("syncr-remote:x", names)).toBe("From a remote");
    expect(tagLabel("phone", names)).toBe("phone");
  });

  it("are found by their remote's name", () => {
    const list = [remote, foreign, folders];
    const ids = (text: string) =>
      filterSnapshots(list, { ...noFilter, text }, names).map((s) => s.id);
    expect(ids("nextcloud")).toEqual(["aaaa"]);
    expect(ids("from a remote")).toEqual(["bbbb"]);
    expect(ids("/srv")).toEqual(["cccc"]);
  });

  it("have no original location: no parent and no host worker", () => {
    const nas = create(WorkerSchema, {
      id: "nas",
      name: "nas-01",
      status: WorkerStatus.ONLINE,
      info: {
        hostname: "nas-01",
        features: ["restic.v2", "restic.restore_subfolder_paths"],
      },
    });
    expect(hostWorker([nas], "nas-01")?.id).toBe("nas");
    expect(hostWorker([nas], "nas-01", remote.tags)).toBeUndefined();
    expect(outdatedHost([nas], "nas-01", remote.tags)).toBeUndefined();
    expect(inPlaceParent(["/2026/11/a.jpg"], remote)).toBeUndefined();
    expect(inPlaceParent(["/srv/photos/a.jpg"], folders)).toBe("/srv/photos");
  });
});
