import { create } from "@bufbuild/protobuf";
import { describe, expect, it } from "vitest";

import { WorkerSchema, WorkerStatus } from "@/gen/syncr/v1/api_pb";

import { allowedFoldersHelp, allowedFoldersWarning } from "./allowed-folders";

function worker(name: string, roots: string[] | undefined, status = WorkerStatus.ONLINE) {
  return create(WorkerSchema, {
    id: name,
    name,
    status,
    info: roots ? { os: "linux", localRoots: roots } : undefined,
  });
}

describe("allowedFoldersHelp", () => {
  it("asks for a worker before anything can be browsed", () => {
    expect(allowedFoldersHelp(undefined, undefined)).toBe(
      "An absolute path on the worker, inside its allowed folders. Choose a worker to browse.",
    );
    expect(allowedFoldersHelp(undefined, undefined, true)).toMatch(/^Absolute paths on the worker/);
  });

  it("says why an offline worker cannot be browsed", () => {
    const w = worker("nas", ["/srv"], WorkerStatus.OFFLINE);
    expect(allowedFoldersHelp(w, undefined)).toBe(
      "nas is offline, so its folders cannot be browsed now. Type the path.",
    );
    expect(allowedFoldersHelp(w, undefined, true)).toMatch(/Type the paths\.$/);
  });

  it("names an online worker, unless a note replaces the help", () => {
    const w = worker("nas", ["/srv"]);
    expect(allowedFoldersHelp(w, undefined)).toBe(
      "An absolute path on nas, inside its allowed folders.",
    );
    expect(allowedFoldersHelp(w, "Browse shows nas.")).toBe("Browse shows nas.");
    expect(allowedFoldersHelp(undefined, "No worker is online.")).toBe("No worker is online.");
  });
});

describe("allowedFoldersWarning", () => {
  const nas = worker("nas", ["/srv/photos", "/mnt/media"]);

  it("says nothing for an empty path, a path inside, or a worker not reported in", () => {
    expect(allowedFoldersWarning([""], [nas])).toBeUndefined();
    expect(allowedFoldersWarning(["  "], [nas])).toBeUndefined();
    expect(allowedFoldersWarning(["/srv/photos/2024"], [nas])).toBeUndefined();
    expect(allowedFoldersWarning(["/mnt/media"], [nas])).toBeUndefined();
    expect(allowedFoldersWarning(["/etc"], [worker("new", undefined)])).toBeUndefined();
    expect(allowedFoldersWarning(["/etc"], [])).toBeUndefined();
  });

  it("names the worker's folders for a path outside them", () => {
    expect(allowedFoldersWarning(["/srv/data"], [nas])).toBe(
      "This path is outside nas's allowed folders (/srv/photos, /mnt/media). The worker refuses it, so the job fails.",
    );
    expect(allowedFoldersWarning(["/srv/photos/../etc"], [nas])).toMatch(/^This path is outside/);
  });

  it("names the paths outside when there are several", () => {
    expect(allowedFoldersWarning(["/srv/photos", "/etc"], [nas])).toMatch(
      /^\/etc is outside nas's allowed folders .* The worker refuses it,/,
    );
    expect(allowedFoldersWarning(["/etc", "/opt", "/srv/photos"], [nas])).toMatch(
      /^\/etc and \/opt are outside .* The worker refuses them,/,
    );
  });

  it("says when a worker has no allowed folders at all", () => {
    expect(allowedFoldersWarning(["/srv"], [worker("bare", [])])).toBe(
      "bare has no allowed folders, so it refuses every path on its disks. They are set with local_roots in its configuration.",
    );
  });

  it("names only the workers of a label whose folders leave the path out", () => {
    const other = worker("nas-2", ["/data"]);
    const bare = worker("nas-3", []);
    expect(allowedFoldersWarning(["/srv/photos"], [nas, other])).toBe(
      "This path is outside the allowed folders of nas-2 (/data). Runs on that worker fail.",
    );
    expect(allowedFoldersWarning(["/srv/photos"], [nas, other, bare])).toBe(
      "This path is outside the allowed folders of nas-2 (/data) and nas-3 (none). Runs on those workers fail.",
    );
    expect(
      allowedFoldersWarning(["/srv/photos"], [nas, worker("nas-4", ["/srv"])]),
    ).toBeUndefined();
  });

  it("compares Windows paths without case", () => {
    const pc = create(WorkerSchema, {
      name: "pc",
      info: { os: "windows", localRoots: ["D:\\Shared"] },
    });
    expect(allowedFoldersWarning(["d:\\shared\\docs"], [pc])).toBeUndefined();
    expect(allowedFoldersWarning(["C:\\Windows"], [pc])).toMatch(/outside pc's allowed folders/);
  });
});
