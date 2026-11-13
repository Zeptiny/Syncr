// Milestone 7 (the file browser) part of the mock: a remote's usage
// (GetRemoteUsage), following the controller's rules and the seeded
// controller's demo answers: B2 and the storage box report numbers, MinIO
// cannot tell, Nextcloud fails, and a remote on a worker's own disk needs
// that worker and answers "unsupported". applyTransfer plays a finished
// copy or move on the demo folders, so the browser shows its result.
import { Code, ConnectError, type ConnectRouter } from "@connectrpc/connect";

import { WorkerStatus, type JobDefinition, type Worker } from "@/gen/syncr/v1/api_pb";
import { FilterAction, RcloneTransferMode } from "@/gen/syncr/v1/link_pb";
import { ProbeService } from "@/gen/syncr/v1/probe_pb";
import type { Remote } from "@/gen/syncr/v1/remotes_pb";
import type { User } from "@/gen/syncr/v1/users_pb";
import * as fx from "@/test/fixtures";

import type { M5, Tree } from "./m5";

const GiB = 1024 ** 3;

/** M7Deps is what the milestone 7 mock shares with the rest of the mock. */
export interface M7Deps {
  me: () => User;
  workers: Worker[];
  remotes: () => Remote[];
  invalid: (v: { field: string; message: string }[]) => ConnectError;
  sleep: (ms: number) => Promise<void>;
  /** probeDelay is how long a probe takes (default 300 ms; tests use 0). */
  probeDelay?: number;
}

/** usageOf is a remote's demo usage, by remote ID; undefined fails like Nextcloud. */
function usageOf(remoteId: string) {
  switch (remoteId) {
    case fx.ids.b2:
      return {
        used: BigInt(Math.round(318.6 * GiB)),
        total: BigInt(1024 * GiB),
        objects: 184_230n,
      };
    case fx.ids.storageBox:
      return {
        used: BigInt(Math.round(412.3 * GiB)),
        total: BigInt(1024 * GiB),
        free: BigInt(Math.round(611.7 * GiB)),
      };
    case fx.ids.nextcloud:
      return undefined;
    case fx.ids.pooled:
      return { used: BigInt(Math.round(2.1 * 1024 * GiB)), total: BigInt(6 * 1024 * GiB) };
    default:
      return { unsupported: true };
  }
}

/**
 * registerM7 adds GetRemoteUsage to a mock router. Register it after the
 * milestone 5 ProbeService, whose unimplemented method it replaces.
 */
export function registerM7(router: ConnectRouter, d: M7Deps): void {
  const delay = () => d.sleep(d.probeDelay ?? 300);
  const online = (w: Worker) => w.status === WorkerStatus.ONLINE;
  router.rpc(ProbeService.method.getRemoteUsage, async (req) => {
    const u = d.me();
    const remote = u.id === fx.ids.admin && d.remotes().find((x) => x.id === req.remoteId);
    if (!remote) throw new ConnectError("remote not found", Code.NotFound);
    const local = !!fx.backends.find((b) => b.id === remote.backendType)?.capabilities?.local;
    if (local && !req.workerId)
      throw d.invalid([
        { field: "worker_id", message: "choose the worker whose disk holds this remote" },
      ]);
    let w: Worker | undefined;
    if (req.workerId) {
      w = d.workers.find((x) => x.id === req.workerId);
      if (!w) throw new ConnectError("worker not found", Code.NotFound);
      if (!online(w)) throw new ConnectError(`worker ${w.name} is offline`, Code.Unavailable);
      const op = local ? "list_local" : "about";
      if (!w.info?.probeOps.includes(op))
        throw new ConnectError(
          `ask the remote for its usage: worker ${w.name} cannot do this; upgrade it`,
          Code.FailedPrecondition,
        );
    } else {
      const usable = d.workers
        .filter((x) => online(x) && x.info?.probeOps.includes("about"))
        .sort((a, b) => a.name.localeCompare(b.name));
      w = usable.find((x) => x.id === fx.ids.nas) ?? usable[0];
      if (!w)
        throw new ConnectError(
          "ask the remote for its usage: no usable worker; upgrade a worker",
          Code.FailedPrecondition,
        );
    }
    const worker = { id: w.id, name: w.name };
    // A worker cannot report usage for its own folders: no probe is sent.
    if (local) return { usage: { unsupported: true }, worker };
    await delay();
    const usage = usageOf(remote.id);
    if (!usage)
      throw new ConnectError(
        "ask the remote for its usage: failed precondition: couldn't reach the remote: dial tcp 192.0.2.40:443: connect: connection refused",
        Code.FailedPrecondition,
      );
    return { usage, worker };
  });
}

/** MockFiles are the demo folders of the m5 mock (M5.files). */
export type MockFiles = M5["files"];

const isTree = (v: Tree | number | undefined): v is Tree => typeof v === "object";

/**
 * folderAt returns the demo folder of a job location (a worker's path when
 * remoteId is empty), creating it when create is set, as rclone creates a
 * missing destination.
 */
function folderAt(
  files: MockFiles,
  workerId: string,
  loc: { remoteId: string; path: string } | undefined,
  create: boolean,
): Tree | undefined {
  if (!loc) return undefined;
  let at: Tree | undefined;
  let parts: string[];
  if (loc.remoteId) {
    at = files.remotes[loc.remoteId] ??= {};
    parts = loc.path.split("/").filter(Boolean);
  } else {
    const roots = files.workers[workerId] ?? {};
    const windows = /^[A-Za-z]:/.test(loc.path);
    const norm = (p: string) =>
      windows ? p.toLowerCase().replace(/[\\/]+$/, "") : p.replace(/\/+$/, "");
    const sep = windows ? "\\" : "/";
    const root = Object.keys(roots).find(
      (r) => norm(loc.path) === norm(r) || norm(loc.path).startsWith(norm(r) + sep),
    );
    if (!root) return undefined;
    at = roots[root];
    parts = loc.path.slice(root.length).split(/[\\/]/).filter(Boolean);
  }
  for (const p of parts) {
    if (!isTree(at[p])) {
      if (!create || at[p] !== undefined) return undefined;
      at[p] = {};
    }
    at = at[p];
  }
  return at;
}

/** unescapeName undoes rclone-filters' escaping of one pattern. */
function unescapeName(p: string): string {
  return p.replace(
    /\[(.)\]|\\(.)/gu,
    (_, cls: string | undefined, esc: string | undefined) => cls ?? esc ?? "",
  );
}

/**
 * applyTransfer plays a finished transfer on the demo folders: the items
 * its filters name ("+ /rel", "+ /rel/**", then "- **"), or the whole
 * source without filters, are copied into the destination; a move also
 * takes them from the source. A dry run or another kind changes nothing.
 */
export function applyTransfer(files: MockFiles, workerId: string, def: JobDefinition | undefined) {
  const op = def?.operation;
  if (op?.case !== "rcloneTransfer" || op.value.options?.dryRun) return;
  const t = op.value;
  const from = folderAt(files, workerId, t.source, false);
  const to = folderAt(files, workerId, t.destination, true);
  if (!from || !to || from === to) return;
  const keep = t.options?.ignoreExisting ?? false;
  const rules = (t.options?.filters ?? [])
    .filter((f) => f.action === FilterAction.INCLUDE)
    .map((f) => unescapeName(f.pattern.replace(/^\//, "").replace(/\/\*\*$/, "")));
  const names = rules.length ? rules : Object.keys(from);
  for (const rel of names) {
    const parts = rel.split("/");
    const name = parts.pop() ?? "";
    let src: Tree | undefined = from;
    let dst: Tree = to;
    for (const p of parts) {
      src = isTree(src?.[p]) ? src[p] : undefined;
      if (!isTree(dst[p])) dst[p] = {};
      dst = dst[p];
    }
    const v = src?.[name];
    if (!src || v === undefined) continue;
    if (!(keep && dst[name] !== undefined)) dst[name] = structuredClone(v);
    if (t.mode === RcloneTransferMode.MOVE) delete src[name];
  }
}
