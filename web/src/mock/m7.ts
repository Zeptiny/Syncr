// Milestone 7 (the file browser) part of the mock: a remote's usage
// (GetRemoteUsage), following the controller's rules and the seeded
// controller's demo answers: B2 and the storage box report numbers, MinIO
// cannot tell, Nextcloud fails, and a remote on a worker's own disk needs
// that worker and answers "unsupported". applyTransfer plays a finished
// copy or move on the demo folders, so the browser shows its result.
//
// The scans follow the seeded controller (internal/devseed/scans.go):
// folder sizes summed from the demo folders (nas-01's /srv has an entry
// that cannot be read, /mnt stops early), snapshots that differ from one
// another (files that change size from time to time, files added later,
// copies cleaned up, a link that became a file), searches over them
// (*.jpg in Photos vault is cut at 1,000 matches) and comparisons. A
// comparison of snapshots more than 20 apart stops early, so the page's
// state without totals can be seen. Every heavy probe takes the worker's
// one heavy slot: a second one at the same time is refused as busy.
import { create } from "@bufbuild/protobuf";
import { durationFromMs, timestampFromMs, timestampMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError, type ConnectRouter } from "@connectrpc/connect";

import { WorkerStatus, type JobDefinition, type Worker } from "@/gen/syncr/v1/api_pb";
import {
  FilterAction,
  ProbeEntrySchema,
  ProbeEntryType,
  RcloneTransferMode,
  ResticDiffModifier,
  type ProbeEntry,
  type ResticDiffChange,
  type ResticFindMatch,
} from "@/gen/syncr/v1/link_pb";
import { ProbeService } from "@/gen/syncr/v1/probe_pb";
import type { Remote } from "@/gen/syncr/v1/remotes_pb";
import { ResticService } from "@/gen/syncr/v1/restic_pb";
import type { User } from "@/gen/syncr/v1/users_pb";
import * as fx from "@/test/fixtures";

import { ids as m5ids, type M5, type RepoState, type Tree } from "./m5";

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
  /** now dates the demo snapshots' files (the m5 data's now). */
  now?: number;
  /** repositories are the m5 repositories, for the snapshot scans. */
  repositories?: () => RepoState[];
  /** files are the m5 demo folders, for measuring them. */
  files?: MockFiles;
  /** busy are workers whose heavy slot is taken (tests): their scans are refused. */
  busy?: ReadonlySet<string>;
}

/** HEAVY_OPS are the probe operations of milestone 7's scans. */
export const HEAVY_OPS = [
  "measure_local",
  "measure_remote",
  "restic_measure",
  "restic_find",
  "restic_diff",
];

/**
 * applyWorkers lets the demo's current workers measure, search and
 * compare: those that list folders advertise the heavy operations.
 */
export function applyWorkers(workers: Worker[]): void {
  for (const w of workers) {
    if (!w.info?.probeOps.includes("list_local")) continue;
    for (const op of HEAVY_OPS) if (!w.info.probeOps.includes(op)) w.info.probeOps.push(op);
  }
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
 * registerM7 adds GetRemoteUsage and the scans (folder sizes, snapshot
 * search and compare, and per-snapshot listings) to a mock router.
 * Register it after the milestone 5 services, whose methods it replaces.
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
  if (d.files && d.repositories)
    registerScans(router, { ...d, files: d.files, repositories: d.repositories });
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

// ---------------------------------------------------------------------------
// Scans: folder sizes, snapshots that differ, search and compare
// ---------------------------------------------------------------------------

const MIN = 60_000;
const DAY = 24 * 60 * MIN;
const MAX_MATCHES = 1000;
const MAX_CHANGES = 2000;
/** A comparison of snapshots further apart than this stops early (the demo of it). */
const FAR_APART = 20;

/** Node is one file, link or folder of a demo snapshot or folder. */
interface Node {
  path: string;
  name: string;
  type: ProbeEntryType;
  size: number;
  mtime: number;
}

/** Listing is a demo tree by folder: each folder's path to its entries. */
type Listing = Map<string, Node[]>;

/** hashOf is a fixed number for a path (24 bits, FNV-1a). */
function hashOf(p: string): number {
  let h = 0x811c9dc5;
  for (const c of `syncr mock scan ${p}`) {
    h ^= c.codePointAt(0)!;
    h = Math.imul(h, 0x01000193) >>> 0;
  }
  return h & 0xffffff;
}

/** mtimeOf dates an entry as the m5 listings do. */
const mtimeOf = (now: number, name: string, i: number) =>
  now - (3 + ((name.length * 7 + i * 13) % 200)) * DAY - i * 37 * MIN;

/** flatten lists a demo tree under base ("/x/y" or "" for a remote's root). */
function flatten(tree: Tree, base: string, now: number, sep = "/"): Listing {
  const out: Listing = new Map();
  const walk = (t: Tree, dir: string) => {
    const list: Node[] = [];
    out.set(dir, list);
    Object.entries(t).forEach(([name, v], i) => {
      const path =
        dir === "/" || dir === "" ? `${dir === "/" ? "/" : ""}${name}` : `${dir}${sep}${name}`;
      const isDir = typeof v !== "number";
      list.push({
        path,
        name,
        type: isDir ? ProbeEntryType.DIR : ProbeEntryType.FILE,
        size: isDir ? 0 : Math.round(v),
        mtime: mtimeOf(now, name, i),
      });
      if (isDir) walk(v, path);
    });
  };
  walk(tree, base);
  return out;
}

/** measureListing sums a folder of a listing: totals and each child folder's, largest first. */
function measureListing(l: Listing, dir: string, skip?: (path: string) => boolean) {
  const m = {
    path: dir,
    bytes: 0n,
    files: 0n,
    dirs: 0n,
    children: [] as { name: string; bytes: bigint; files: bigint; dirs: bigint }[],
    childrenTruncated: false,
    complete: true,
    unreadable: 0n,
  };
  const walk = (d: string, child?: (typeof m.children)[number]) => {
    for (const e of l.get(d) ?? []) {
      if (skip?.(e.path)) continue;
      const isDir = e.type === ProbeEntryType.DIR;
      const add = (t: { bytes: bigint; files: bigint; dirs: bigint }) => {
        if (isDir) t.dirs++;
        else {
          t.files++;
          t.bytes += BigInt(e.size);
        }
      };
      add(m);
      if (child) add(child);
      if (isDir) {
        let next = child;
        if (d === dir) {
          next = { name: e.name, bytes: 0n, files: 0n, dirs: 0n };
          m.children.push(next);
        }
        walk(e.path, next);
      }
    }
  };
  walk(dir);
  m.children.sort(
    (a, b) =>
      Number(b.bytes - a.bytes) || Number(b.files - a.files) || a.name.localeCompare(b.name),
  );
  return m;
}

const LIVING = [
  "albums.json",
  "inventory.csv",
  "return-draft.ods",
  "thesis.pdf",
  "[x] todo.md",
  "budget.xlsx",
  "notes.md",
];

/** extraFiles are files the demo snapshots hold beyond the folders the m5 trees show. */
function extraFiles(repoId: string): [string, number][] {
  return repoId === m5ids.photosVault ? [["/srv/photos/2026/albums.json", 18 * 1024]] : [];
}

/**
 * snapshotListing is the tree of a repository's snapshot of age age (0
 * is the newest), the way the seeded controller's snapshots differ: the
 * newest holds the m5 tree, and older ones differ as backups do.
 */
function snapshotListing(r: RepoState, age: number, now: number): Listing {
  const base = flatten(r.tree, "/", now);
  for (const [path, size] of extraFiles(r.repo.id)) {
    const dir = path.slice(0, path.lastIndexOf("/"));
    const list = base.get(dir);
    if (list && !list.some((e) => e.path === path))
      list.push({
        path,
        name: path.slice(dir.length + 1),
        type: ProbeEntryType.FILE,
        size,
        mtime: mtimeOf(now, "albums.json", list.length),
      });
  }
  const snaps = r.snapshots;
  if (snaps.length < 2) return base;
  const at = (a: number) => timestampMs(snaps[Math.min(a, snaps.length - 1)].time!);
  const span = snaps.length - 1;
  const files = [...base.values()]
    .flat()
    .filter((e) => e.type !== ProbeEntryType.DIR)
    .sort((a, b) => a.path.localeCompare(b.path));
  const living = (e: Node) => LIVING.includes(e.name) || hashOf(e.path) % 18 === 1;
  const ordinary = files.filter((e) => !living(e));
  let typed = "";
  if (ordinary.length > 0)
    typed = ordinary.reduce((a, b) => (b.path.length < a.path.length ? b : a)).path;
  const rest = ordinary.filter((e) => e.path !== typed);
  const cleaned = rest.length >= 2 ? rest[0].path : "";
  const newest = rest.length >= 2 ? rest[rest.length - 1].path : "";

  const out: Listing = new Map();
  for (const [dir, list] of base) {
    const kept: Node[] = [];
    for (const e of list) {
      if (e.type === ProbeEntryType.DIR) {
        kept.push(e);
        continue;
      }
      const h = hashOf(e.path);
      if (e.path === typed && age >= 1) {
        kept.push({ ...e, type: ProbeEntryType.SYMLINK, size: 0, mtime: at(span) - 60 * MIN });
        continue;
      }
      if (living(e)) {
        const every = 2 + ((h >> 8) % 3);
        const v = Math.ceil(age / every);
        let size = e.size - v * Math.max(Math.floor(e.size / 25), 37);
        if (size < 1) size = 1 + v;
        const mtime = v === 0 ? e.mtime : at(v * every) - (30 + (h % 150)) * MIN;
        kept.push({ ...e, size, mtime });
        continue;
      }
      const appears = e.path === newest ? 1 : 1 + (Math.floor(h / 9) % span);
      if ((e.path === newest || (h % 9 === 4 && e.path !== cleaned)) && age >= appears) continue;
      kept.push(e);
      const gone = e.path === cleaned ? 1 : 1 + (Math.floor(h / 9) % span);
      if ((e.path === cleaned || h % 9 === 7) && age >= gone) {
        const dot = e.name.lastIndexOf(".");
        const name = dot > 0 ? `${e.name.slice(0, dot)} (1)${e.name.slice(dot)}` : `${e.name} (1)`;
        kept.push({
          ...e,
          name,
          path: `${dir === "/" ? "" : dir}/${name}`,
          mtime: at(span) - 120 * MIN,
        });
      }
    }
    out.set(dir, kept);
  }
  return out;
}

const entryOf = (e: Node): ProbeEntry =>
  create(ProbeEntrySchema, {
    name: e.name,
    path: e.path,
    type: e.type,
    size: BigInt(e.size),
    modTime: timestampFromMs(e.mtime),
    mode:
      e.type === ProbeEntryType.DIR
        ? "drwxr-xr-x"
        : e.type === ProbeEntryType.SYMLINK
          ? "Lrwxrwxrwx"
          : "-rw-r--r--",
  });

/** nodes are every entry below dir of a listing, by path. */
function nodes(l: Listing, dir: string): Node[] {
  const out: Node[] = [];
  const walk = (d: string) => {
    for (const e of l.get(d) ?? []) {
      out.push(e);
      if (e.type === ProbeEntryType.DIR) walk(e.path);
    }
  };
  walk(dir);
  return out.sort((a, b) => (a.path < b.path ? -1 : a.path > b.path ? 1 : 0));
}

/** globPart turns one glob segment (Go's path.Match) into a regular expression. */
function globPart(p: string): RegExp {
  let re = "";
  for (let i = 0; i < p.length; i++) {
    const c = p[i];
    if (c === "*") re += "[^/]*";
    else if (c === "?") re += "[^/]";
    else if (c === "\\" && i + 1 < p.length) re += `\\${p[++i]}`;
    else if (c === "[") {
      const end = p.indexOf("]", i + 2);
      if (end < 0) return /(?!)/;
      let body = p.slice(i + 1, end);
      if (body.startsWith("^")) body = `^${body.slice(1).replace(/[\\\]]/g, "\\$&")}`;
      else body = body.replace(/[\\\]^]/g, "\\$&");
      re += `[${body}]`;
      i = end;
    } else re += c.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  }
  return new RegExp(`^${re}$`, "s");
}

/**
 * findMatch is restic find's matching: a pattern without "/" matches any
 * name of the path, one with "/" consecutive names (from the top when it
 * starts with "/"), "**" any number of them; a match on a folder matches
 * everything below it.
 */
function findMatch(pattern: string, path: string, icase: boolean): boolean {
  if (icase) {
    pattern = pattern.toLowerCase();
    path = path.toLowerCase();
  }
  const names = path.split("/").filter(Boolean);
  const anchored = pattern.startsWith("/");
  const parts = pattern.split("/").filter(Boolean);
  const prefix = (ps: string[], ns: string[]): boolean => {
    if (ps.length === 0) return true;
    if (ps[0] === "**")
      return ns.some((_, i) => prefix(ps.slice(1), ns.slice(i))) || prefix(ps.slice(1), []);
    return ns.length > 0 && globPart(ps[0]).test(ns[0]) && prefix(ps.slice(1), ns.slice(1));
  };
  for (let i = 0; i < names.length; i++) {
    if (prefix(parts, names.slice(i))) return true;
    if (anchored) break;
  }
  return false;
}

const hexId = /^[0-9a-f]{8,64}$/;

function registerScans(
  router: ConnectRouter,
  d: M7Deps & { files: MockFiles; repositories: () => RepoState[] },
): void {
  const now = d.now ?? Date.now();
  // Scans take a while in the dev mock, so "Measuring…" can be seen.
  const scanDelay = () => d.sleep(d.probeDelay ?? 1500);
  const online = (w: Worker) => w.status === WorkerStatus.ONLINE;
  const running = new Set<string>();
  const ref = (w: Worker) => ({ id: w.id, name: w.name });

  // pick returns the worker of a scan: the chosen one, which must be
  // online and advertise op, or the first usable one (nas-01 first).
  const pick = (what: string, op: string, workerId: string): Worker => {
    if (workerId) {
      const w = d.workers.find((x) => x.id === workerId);
      if (!w) throw new ConnectError("worker not found", Code.NotFound);
      if (!online(w)) throw new ConnectError(`worker ${w.name} is offline`, Code.Unavailable);
      if (!w.info?.probeOps.includes(op))
        throw new ConnectError(
          `${what}: worker ${w.name} cannot do this; upgrade it`,
          Code.FailedPrecondition,
        );
      return w;
    }
    const usable = d.workers
      .filter((x) => online(x) && x.info?.probeOps.includes(op))
      .sort((a, b) => a.name.localeCompare(b.name));
    const w = usable.find((x) => x.id === fx.ids.nas) ?? usable[0];
    if (!w)
      throw new ConnectError(
        `${what}: no usable worker; upgrade a worker`,
        Code.FailedPrecondition,
      );
    return w;
  };
  // heavy holds the worker's one heavy slot while a scan runs.
  const heavy = async <T>(what: string, w: Worker, run: () => T): Promise<T> => {
    if (running.has(w.id) || d.busy?.has(w.id))
      throw new ConnectError(
        `${what}: unavailable: worker "${w.name}" is already measuring or searching; try again in a moment`,
        Code.Unavailable,
      );
    running.add(w.id);
    try {
      await scanDelay();
      return run();
    } finally {
      running.delete(w.id);
    }
  };
  const elapsed = (complete: boolean) => durationFromMs(complete ? 1400 : 40_500);
  const repo = (id: string): RepoState => {
    const u = d.me();
    const r = d.repositories().find((x) => !x.deleted && x.ownerId === u.id && x.repo.id === id);
    if (!r) throw new ConnectError("repository not found", Code.NotFound);
    if (r.repo.deleting)
      throw new ConnectError(
        `the repository's data is being deleted; cancel job ${r.repo.deletionJobId} first`,
        Code.FailedPrecondition,
      );
    return r;
  };
  const ageOf = (r: RepoState, id: string) =>
    id.length >= 8 ? r.snapshots.findIndex((s) => s.id === id || s.id.startsWith(id)) : -1;
  const isLocalRemote = (remoteId: string) =>
    !!fx.backends.find((b) => b.id === d.remotes().find((x) => x.id === remoteId)?.backendType)
      ?.capabilities?.local;
  const resticWorker = (r: RepoState, what: string, op: string, workerId: string) => {
    if (isLocalRemote(r.repo.remoteId) && !workerId)
      throw d.invalid([
        { field: "worker_id", message: "choose the worker whose disk holds this repository" },
      ]);
    return pick(what, op, workerId);
  };
  const listings = new Map<string, Listing>();
  const listingAt = (r: RepoState, age: number) => {
    const key = `${r.repo.id}@${age}`;
    let l = listings.get(key);
    if (!l) listings.set(key, (l = snapshotListing(r, age, now)));
    return l;
  };
  const treePath = (p: string) => "/" + p.split("/").filter(Boolean).join("/");

  // A worker's folders, as m5 lists them (its trees, under the allowed folders).
  const workerListing = (w: Worker, path: string): { listing: Listing; dir: string } => {
    const windows = w.info?.os === "windows";
    const sep = windows ? "\\" : "/";
    const norm = (p: string) => (windows ? p.toLowerCase() : p).replace(/[\\/]+$/, "");
    const root = (w.info?.localRoots ?? []).find(
      (x) => norm(path) === norm(x) || norm(path).startsWith(norm(x) + sep),
    );
    if (!root)
      throw d.invalid([{ field: "path", message: "is outside the worker's allowed folders" }]);
    const base = root.replace(/[\\/]+$/, "");
    const listing = flatten(d.files.workers[w.id]?.[root] ?? {}, base, now, sep);
    const rest = path.slice(root.length).split(/[\\/]/).filter(Boolean);
    const dir = [base, ...rest].join(sep);
    if (!listing.has(dir)) {
      const parent = dir.slice(0, dir.lastIndexOf(sep));
      if (listing.get(parent)?.some((e) => e.path === dir))
        throw new ConnectError(
          `measure the worker's folder: failed precondition: ${dir} is not a folder`,
          Code.FailedPrecondition,
        );
      throw d.invalid([{ field: "path", message: "does not exist" }]);
    }
    return { listing, dir };
  };

  router.rpc(ProbeService.method.measureWorkerFolder, async (req) => {
    d.me();
    if (!req.workerId) throw d.invalid([{ field: "worker_id", message: "is required" }]);
    if (!req.path) throw d.invalid([{ field: "path", message: "is required" }]);
    const what = "measure the worker's folder";
    const w = pick(what, "measure_local", req.workerId);
    const { listing, dir } = workerListing(w, req.path);
    return heavy(what, w, () => {
      // nas-01's disks: the walk runs into the time limit at the music
      // folder, and one entry cannot be read; /srv has one too.
      const partial = w.id === fx.ids.nas && (dir === "/mnt" || dir === "/mnt/media");
      const m = measureListing(listing, dir, partial ? (p) => p >= "/mnt/media/music" : undefined);
      m.complete = !partial;
      if (w.id === fx.ids.nas && (dir === "/srv" || partial)) m.unreadable = 1n;
      return { measurement: m, worker: ref(w), elapsed: elapsed(m.complete) };
    });
  });

  router.rpc(ProbeService.method.measureRemoteFolder, async (req) => {
    const u = d.me();
    const remote = u.id === fx.ids.admin && d.remotes().find((x) => x.id === req.remoteId);
    if (!remote) throw new ConnectError("remote not found", Code.NotFound);
    if (isLocalRemote(remote.id) && !req.workerId)
      throw d.invalid([
        { field: "worker_id", message: "choose the worker whose disk holds this remote" },
      ]);
    if (req.path.split("/").includes(".."))
      throw d.invalid([{ field: "path", message: "must not climb out of the remote" }]);
    const what = "measure the remote's folder";
    const w = pick(what, "measure_remote", req.workerId);
    const listing = flatten(d.files.remotes[remote.id] ?? {}, "", now);
    const dir = req.path.split("/").filter(Boolean).join("/");
    if (!listing.has(dir)) throw d.invalid([{ field: "path", message: "does not exist" }]);
    return heavy(what, w, () => ({
      measurement: measureListing(listing, dir),
      worker: ref(w),
      elapsed: elapsed(true),
    }));
  });

  router.rpc(ResticService.method.browseSnapshot, async (req) => {
    const r = repo(req.repositoryId);
    const w = resticWorker(r, "list the snapshot", "restic_ls", req.workerId);
    await d.sleep(d.probeDelay ?? 300);
    const age = ageOf(r, req.snapshotId);
    if (age < 0) throw d.invalid([{ field: "path", message: "does not exist" }]);
    const dir = treePath(req.path);
    const list = listingAt(r, age).get(dir);
    if (!list) throw d.invalid([{ field: "path", message: "does not exist" }]);
    return { path: dir, entries: list.map(entryOf), truncated: false, worker: ref(w) };
  });

  router.rpc(ResticService.method.measureSnapshotFolder, async (req) => {
    const r = repo(req.repositoryId);
    if (!hexId.test(req.snapshotId))
      throw d.invalid([
        { field: "snapshot_id", message: "must be a snapshot ID of 8 to 64 hex digits" },
      ]);
    const what = "measure the snapshot's folder";
    const w = resticWorker(r, what, "restic_measure", req.workerId);
    const age = ageOf(r, req.snapshotId);
    if (age < 0)
      throw d.invalid([{ field: "snapshot_id", message: "is not a snapshot of this repository" }]);
    const dir = treePath(req.path);
    const listing = listingAt(r, age);
    if (!listing.has(dir))
      throw d.invalid([{ field: "path", message: "is not a folder in this snapshot" }]);
    return heavy(what, w, () => ({
      measurement: measureListing(listing, dir),
      worker: ref(w),
      elapsed: elapsed(true),
    }));
  });

  router.rpc(ResticService.method.findInRepository, async (req) => {
    const r = repo(req.repositoryId);
    const problems: string[] = [];
    if (req.patterns.length === 0) problems.push("must have at least one pattern");
    if (req.patterns.length > 10) problems.push("must have at most 10 patterns");
    req.patterns.forEach((p, i) => {
      const n = req.patterns.length > 1 ? `pattern ${i + 1} ` : "";
      if (!p) problems.push(`${n}must not be empty`);
      else if (new TextEncoder().encode(p).length > 256)
        problems.push(`${n}must be at most 256 bytes`);
      else if (/[\0\r\n]/.test(p))
        problems.push(`${n}must not contain NUL characters or line breaks`);
    });
    if (problems.length) throw d.invalid([{ field: "patterns", message: problems[0] }]);
    if (req.snapshotIds.some((id) => !hexId.test(id)))
      throw d.invalid([
        { field: "snapshot_ids", message: "must be snapshot IDs of 8 to 64 hex digits" },
      ]);
    const missing = req.snapshotIds.find((id) => ageOf(r, id) < 0);
    if (missing)
      throw d.invalid([
        { field: "snapshot_ids", message: `${missing} is not a snapshot of this repository` },
      ]);
    const what = "search the repository";
    const w = resticWorker(r, what, "restic_find", req.workerId);
    return heavy(what, w, () => {
      const ages = r.snapshots
        .map((_, i) => i)
        .filter(
          (i) =>
            req.snapshotIds.length === 0 ||
            req.snapshotIds.some((id) => r.snapshots[i].id.startsWith(id)),
        );
      const matches: Omit<ResticFindMatch, "$typeName" | "$unknown">[] = [];
      let truncated = false;
      let searched = 0;
      for (const age of ages) {
        for (const e of nodes(listingAt(r, age), "/")) {
          if (!req.patterns.some((p) => findMatch(p, e.path, req.ignoreCase))) continue;
          if (matches.length === MAX_MATCHES) {
            truncated = true;
            break;
          }
          matches.push({ snapshotId: r.snapshots[age].id, entry: entryOf(e) });
        }
        searched++;
        if (truncated) break;
      }
      return {
        matches,
        truncated,
        complete: !truncated,
        snapshotsSearched: searched,
        worker: ref(w),
      };
    });
  });

  router.rpc(ResticService.method.compareSnapshots, async (req) => {
    const r = repo(req.repositoryId);
    const bad = (field: string) =>
      d.invalid([{ field, message: "must be a snapshot ID of 8 to 64 hex digits" }]);
    if (!hexId.test(req.fromSnapshotId)) throw bad("from_snapshot_id");
    if (!hexId.test(req.toSnapshotId)) throw bad("to_snapshot_id");
    if (
      req.fromSnapshotId.startsWith(req.toSnapshotId) ||
      req.toSnapshotId.startsWith(req.fromSnapshotId)
    )
      throw d.invalid([
        { field: "to_snapshot_id", message: "must be another snapshot than from_snapshot_id" },
      ]);
    const what = "compare the snapshots";
    const w = resticWorker(r, what, "restic_diff", req.workerId);
    const from = ageOf(r, req.fromSnapshotId);
    const to = ageOf(r, req.toSnapshotId);
    if (from < 0)
      throw d.invalid([
        { field: "from_snapshot_id", message: "is not a snapshot of this repository" },
      ]);
    if (to < 0)
      throw d.invalid([
        { field: "to_snapshot_id", message: "is not a snapshot of this repository" },
      ]);
    const dir = treePath(req.path);
    const older = listingAt(r, from);
    const newer = listingAt(r, to);
    if (!older.has(dir) || !newer.has(dir))
      throw d.invalid([{ field: "path", message: "is not a folder in both snapshots" }]);
    return heavy(what, w, () => {
      const before = new Map(nodes(older, dir).map((e) => [e.path, e]));
      const after = new Map(nodes(newer, dir).map((e) => [e.path, e]));
      const paths = [...new Set([...before.keys(), ...after.keys()])].sort((a, b) =>
        a < b ? -1 : a > b ? 1 : 0,
      );
      const changes: Omit<ResticDiffChange, "$typeName" | "$unknown">[] = [];
      const stats = {
        changedFiles: 0n,
        addedFiles: 0n,
        addedDirs: 0n,
        addedBytes: 0n,
        removedFiles: 0n,
        removedDirs: 0n,
        removedBytes: 0n,
      };
      let truncated = false;
      const add = (e: Node, modifier: ResticDiffModifier) => {
        if (changes.length === MAX_CHANGES) truncated = true;
        else
          changes.push({
            path: e.type === ProbeEntryType.DIR ? `${e.path}/` : e.path,
            modifier,
          });
      };
      for (const p of paths) {
        const a = before.get(p);
        const b = after.get(p);
        if (!a && b) {
          add(b, ResticDiffModifier.ADDED);
          if (b.type === ProbeEntryType.DIR) stats.addedDirs++;
          else {
            stats.addedFiles++;
            stats.addedBytes += BigInt(b.size);
          }
        } else if (a && !b) {
          add(a, ResticDiffModifier.REMOVED);
          if (a.type === ProbeEntryType.DIR) stats.removedDirs++;
          else {
            stats.removedFiles++;
            stats.removedBytes += BigInt(a.size);
          }
        } else if (a && b && a.type !== b.type) add(b, ResticDiffModifier.TYPE);
        else if (a && b && b.type === ProbeEntryType.FILE && a.size !== b.size) {
          add(b, ResticDiffModifier.CHANGED);
          stats.changedFiles++;
          stats.addedBytes += BigInt(b.size);
          stats.removedBytes += BigInt(a.size);
        }
      }
      // Far apart, the demo comparison runs into the time limit: what was
      // found until then, without restic's statistics.
      if (Math.abs(from - to) > FAR_APART)
        return {
          changes: changes.slice(0, Math.ceil(changes.length / 2)),
          truncated: false,
          complete: false,
          worker: ref(w),
        };
      return { changes, stats, truncated, complete: true, worker: ref(w) };
    });
  });
}
