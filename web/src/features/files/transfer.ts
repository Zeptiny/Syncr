// Pure logic of copying or moving a browser selection as a transfer job:
// where each side is, which worker runs the job, what is refused, and the
// RunJob request. No React here, so it is easy to test.
import type { MessageInitShape } from "@bufbuild/protobuf";

import type { SelectedItem } from "@/components/app/file-browser/selection";
import { isInside } from "@/components/app/file-browser/selection";
import type { JobDefinitionSchema, RunJobRequestSchema, Worker } from "@/gen/syncr/v1/api_pb";
import { FilterAction, ProbeEntryType, RcloneTransferMode } from "@/gen/syncr/v1/link_pb";
import type { Remote } from "@/gen/syncr/v1/remotes_pb";
import { joinPath, normalizePath, pathCrumbs, type PathKind } from "@/lib/paths";
import { parseFilterLine, selectionFilters } from "@/lib/rclone-filters";
import { isWithinRoots } from "@/lib/restic-paths";
import { isOnline, sortWorkers } from "@/lib/workers";

/**
 * Place is where one side of a transfer is: a worker's allowed folders, or
 * one of the user's remotes. A remote of the local type is a folder on the
 * disk of the worker that runs the job; workerId names that worker when it
 * is known (the worker the source was listed with).
 */
export type Place =
  | { kind: "worker"; workerId: string }
  | { kind: "remote"; remoteId: string; local?: boolean; workerId?: string };

/** Endpoint is a folder of a place: a native path on a worker, or a path in a remote. */
export interface Endpoint {
  place: Place;
  path: string;
}

/** placeKind is how a place's paths are written. */
export function placeKind(p: Place): Extract<PathKind, "worker" | "remote"> {
  return p.kind;
}

/** samePlace reports whether two places are the same worker's folders or the same remote. */
export function samePlace(a: Place, b: Place): boolean {
  if (a.kind === "worker" && b.kind === "worker") return a.workerId === b.workerId;
  if (a.kind === "remote" && b.kind === "remote") return a.remoteId === b.remoteId;
  return false;
}

/**
 * onDisk writes an endpoint as a folder on a worker's disk, when it is
 * one: a worker's folder, or a folder of a local-type remote whose worker
 * is known (its own, else jobWorker, the worker that runs the job; remotes
 * give its base folder, options.path). A local remote and the worker's own
 * folders are then the same place, for the check of a folder copied into
 * itself.
 */
export function onDisk(
  e: Endpoint,
  remotes: readonly Remote[],
  jobWorker?: string,
): { workerId: string; path: string } | undefined {
  const p = e.place;
  if (p.kind === "worker") return { workerId: p.workerId, path: e.path };
  const workerId = p.workerId || jobWorker;
  if (!p.local || !workerId) return undefined;
  const base = remotes.find((r) => r.id === p.remoteId)?.options.path;
  if (!base) return undefined;
  let path = normalizePath(base, "worker");
  for (const part of e.path.split("/").filter(Boolean)) path = joinPath(path, part, "worker");
  return { workerId, path };
}

/** placeLabel names a place: a worker's or a remote's name. */
export function placeLabel(p: Place, workers: Worker[], remotes: Remote[]): string {
  if (p.kind === "worker") return workers.find((w) => w.id === p.workerId)?.name ?? "a worker";
  return remotes.find((r) => r.id === p.remoteId)?.name ?? "a remote";
}

/** endpointText writes a folder for people: "/srv/docs on nas-01", "B2 offsite: photos". */
export function endpointText(e: Endpoint, workers: Worker[], remotes: Remote[]): string {
  const name = placeLabel(e.place, workers, remotes);
  if (e.place.kind === "worker") return e.path ? `${e.path} on ${name}` : `a folder on ${name}`;
  return e.path ? `${name}: ${e.path}` : `${name} (its root)`;
}

/** Conflict is what a transfer does with a file already in the destination. */
export type Conflict = "keep" | "differ" | "older";

/** conflictOptions answer "If a file is already there", worded like restore's. */
export const conflictOptions: { value: Conflict; label: string; help: string }[] = [
  {
    value: "keep",
    label: "Keep existing",
    help: "Files that are already there are left alone. Only missing files are copied.",
  },
  {
    value: "differ",
    label: "Replace files that differ",
    help: "A file is replaced when its size or modification time differs.",
  },
  {
    value: "older",
    label: "Replace older files",
    help: "A file is replaced when the copy being sent is newer.",
  },
];

/** conflictText says what a transfer does with files in the way, for a summary. */
export function conflictText(c: Conflict): string {
  switch (c) {
    case "differ":
      return "replaces files that differ";
    case "older":
      return "replaces older files";
    default:
      return "keeps existing files";
  }
}

/**
 * moveText says what a move deletes from the source, for its
 * confirmation. rclone moves a file only when it copies it, or when the
 * destination's copy makes it unneeded: a file skipped because one is
 * already there (Keep existing) stays in the source, while an older one
 * skipped under Replace older files is deleted with the newer copy kept.
 */
export function moveText(c: Conflict, what: string, from: string): string {
  const deleted = `${what} will be deleted from ${from} once copied.`;
  switch (c) {
    case "keep":
      return `${deleted} Files already at the destination are skipped and stay in ${from}.`;
    case "older":
      return `${deleted} Where the destination has a newer copy, it is kept and the older file in ${from} is deleted too.`;
    default:
      return deleted;
  }
}

/**
 * WorkerRule is who runs the transfer. A worker's folders can only be read
 * or written by that worker, so a side on a worker's disk pins the job to
 * it; between two remotes any online worker can, the one that listed the
 * source first.
 */
export type WorkerRule =
  | { kind: "pinned"; workerId: string }
  | { kind: "choose"; proposed: string | undefined; workers: Worker[] }
  | { kind: "refused"; message: string };

/** TWO_DISKS refuses a transfer between two workers' disks. */
export const TWO_DISKS =
  "These folders are on two different workers' disks, and a job runs on one worker. Copy through a remote instead.";

const pinOf = (p: Place | undefined) =>
  p?.kind === "worker" ? p.workerId : p?.kind === "remote" && p.local ? p.workerId : undefined;

/** transferWorkers are the workers that can run a transfer now: online ones, by name. */
export function transferWorkers(workers: Worker[]): Worker[] {
  return sortWorkers(workers).filter(isOnline);
}

/**
 * workerRule decides who runs a transfer from source to destination.
 * listedBy is the worker that listed the source (a remote's browser).
 */
export function workerRule(
  source: Place,
  destination: Place | undefined,
  workers: Worker[],
  listedBy?: string,
): WorkerRule {
  const s = pinOf(source);
  const d = pinOf(destination);
  if (s && d && s !== d) return { kind: "refused", message: TWO_DISKS };
  const pin = s ?? d;
  if (pin) return { kind: "pinned", workerId: pin };
  const usable = transferWorkers(workers);
  const proposed = usable.find((w) => w.id === listedBy)?.id ?? usable[0]?.id;
  return { kind: "choose", proposed, workers: usable };
}

/** TransferSource is what is copied: the selection's items in one place. */
export interface TransferSource {
  place: Place;
  /** items are the selection's uncovered items (none inside another selected folder). */
  items: SelectedItem[];
  /** parent is the items' common parent (SelectionContext.parent). */
  parent: string | undefined;
}

/** TransferForm is the transfer sheet's choices. */
export interface TransferForm {
  destination: Endpoint | undefined;
  /** workerId is the chosen worker when the rule lets the user choose. */
  workerId: string;
  conflict: Conflict;
}

/** TransferPlan is a transfer that can be started. */
export interface TransferPlan {
  workerId: string;
  source: Endpoint;
  destination: Endpoint;
  /** filters are rclone filter lines; empty copies the whole source folder. */
  filters: string[];
}

/** TransferErrors are the sheet's problems, by section. */
export type TransferErrors = Partial<Record<"what" | "destination" | "worker" | "form", string>>;

const nameOf = (path: string, kind: PathKind) => pathCrumbs(path, kind).at(-1)?.label ?? path;

function sameFolder(a: string, b: string, kind: PathKind): boolean {
  const x = normalizePath(a, kind);
  const y = normalizePath(b, kind);
  if (kind === "worker" && (/^[A-Za-z]:/.test(x) || x.startsWith("\\\\")))
    return x.replaceAll("/", "\\").toLowerCase() === y.replaceAll("/", "\\").toLowerCase();
  return x === y;
}

/**
 * transferSource works out what rclone reads: the common parent and
 * filters for the items, or, for one of a worker's allowed folders (whose
 * parent the worker may not read), that folder itself, which then arrives
 * under its name (into). worker is the source's worker, for its roots.
 */
export function transferSource(
  src: TransferSource,
  worker: Worker | undefined,
): { path: string; filters: string[]; into: string } | { error: string } {
  const kind = placeKind(src.place);
  if (src.place.kind === "worker" && src.parent !== undefined && worker?.info) {
    const roots = worker.info.localRoots;
    const os = worker.info.os;
    if (!isWithinRoots(src.parent, roots, os)) {
      const [only] = src.items;
      if (src.items.length === 1 && only.entry.type === ProbeEntryType.DIR)
        return { path: only.path, filters: [], into: nameOf(only.path, kind) };
      return {
        error:
          "These items are in different allowed folders. Copy each allowed folder's items separately.",
      };
    }
  }
  const r = selectionFilters(
    src.items.map((i) => ({ path: i.path, dir: i.entry.type === ProbeEntryType.DIR })),
    src.parent,
  );
  if ("error" in r) return r;
  return { path: r.source, filters: r.filters, into: "" };
}

/**
 * planTransfer checks the sheet's choices and builds the transfer, or
 * returns what is wrong: no destination, a folder copied into itself, the
 * items' own folder as the destination, two workers' disks, no worker.
 * remotes give local remotes' folders (see onDisk).
 */
export function planTransfer(
  src: TransferSource,
  form: TransferForm,
  workers: Worker[],
  listedBy?: string,
  remotes: readonly Remote[] = [],
): { plan?: TransferPlan; errors: TransferErrors } {
  const errors: TransferErrors = {};
  const sourceWorker =
    src.place.kind === "worker" ? workers.find((w) => w.id === src.place.workerId) : undefined;
  const from = transferSource(src, sourceWorker);
  if ("error" in from) errors.what = from.error;

  const dest = form.destination;
  const rule = workerRule(src.place, dest?.place, workers, listedBy);
  const runner =
    rule.kind === "pinned" ? rule.workerId : rule.kind === "choose" ? form.workerId : undefined;
  if (!dest) errors.destination = "Choose the folder the items go into.";
  else {
    const kind = placeKind(dest.place);
    const path = normalizePath(dest.path, kind);
    if (kind === "worker" && !/^([A-Za-z]:[\\/]|\/|\\\\)/.test(path))
      errors.destination = "Choose a folder on the worker, inside its allowed folders.";
    else {
      // Compare in the destination's terms, or on the worker's disk when
      // one side is a local remote of that worker and the other its folder.
      const at = (p: string): Endpoint => ({ place: src.place, path: p });
      const same = samePlace(src.place, dest.place);
      const disk = same ? undefined : onDisk({ place: dest.place, path }, remotes, runner);
      const shared = disk && onDisk(at(""), remotes, runner)?.workerId === disk.workerId;
      const k: PathKind = same ? kind : "worker";
      const there = (p: string) => (same ? p : (onDisk(at(p), remotes, runner)?.path ?? p));
      const to = same ? path : (disk?.path ?? path);
      if (same || shared) {
        const into = src.items.find(
          (i) =>
            i.entry.type === ProbeEntryType.DIR &&
            (sameFolder(there(i.path), to, k) || isInside(to, there(i.path), k)),
        );
        if (into)
          errors.destination = `A folder can't be copied into itself: ${nameOf(into.path, placeKind(src.place))} is selected.`;
        else if (src.parent !== undefined && sameFolder(there(src.parent), to, k))
          errors.destination = "The items are already in this folder. Choose another one.";
      }
    }
  }

  let workerId = "";
  if (rule.kind === "refused") errors.worker = rule.message;
  else if (rule.kind === "pinned") workerId = rule.workerId;
  else {
    workerId = form.workerId || rule.proposed || "";
    if (!workerId) errors.worker = "No worker is online to run the job.";
  }

  if (Object.keys(errors).length || "error" in from || !dest) return { errors };
  const kind = placeKind(dest.place);
  const destPath = normalizePath(dest.path, kind);
  return {
    errors,
    plan: {
      workerId,
      source: { place: src.place, path: from.path },
      destination: {
        place: dest.place,
        path: from.into ? joinPath(destPath, from.into, kind) : destPath,
      },
      filters: from.filters,
    },
  };
}

/** TransferOp is what the sheet starts: a copy or a move. */
export type TransferOp = "copy" | "move";

const location = (e: Endpoint) => ({
  remoteId: e.place.kind === "remote" ? e.place.remoteId : "",
  path: e.path,
});

/**
 * transferDefinition is the job of a plan. Empty folders are copied too (a
 * selected empty folder must arrive), and a move removes the folders it
 * emptied: with the filters, rclone 1.75 removes only those.
 */
export function transferDefinition(
  plan: TransferPlan,
  op: TransferOp,
  conflict: Conflict,
  dryRun: boolean,
): MessageInitShape<typeof JobDefinitionSchema> {
  return {
    operation: {
      case: "rcloneTransfer",
      value: {
        mode: op === "move" ? RcloneTransferMode.MOVE : RcloneTransferMode.COPY,
        source: location(plan.source),
        destination: location(plan.destination),
        options: {
          dryRun,
          ignoreExisting: conflict === "keep",
          update: conflict === "older",
          createEmptySrcDirs: true,
          deleteEmptySrcDirs: op === "move",
          filters: plan.filters.map((l) => {
            const f = parseFilterLine(l);
            return {
              action: f.include ? FilterAction.INCLUDE : FilterAction.EXCLUDE,
              pattern: f.pattern,
            };
          }),
        },
      },
    },
  };
}

/** transferRequest is the RunJob request of a plan. */
export function transferRequest(
  plan: TransferPlan,
  op: TransferOp,
  conflict: Conflict,
  dryRun: boolean,
): MessageInitShape<typeof RunJobRequestSchema> {
  return { workerId: plan.workerId, definition: transferDefinition(plan, op, conflict, dryRun) };
}

/** itemsText counts items: "1 item", "3 items". */
export function itemsText(n: number): string {
  return n === 1 ? "1 item" : `${n.toLocaleString("en-US")} items`;
}

/**
 * transferSummary says in one line what the transfer does: "Copies 3 items
 * from /srv/docs on nas-01 into B2 offsite: inbox and keeps existing files."
 */
export function transferSummary(
  op: TransferOp,
  n: number,
  from: Endpoint | undefined,
  to: Endpoint | undefined,
  conflict: Conflict,
  workers: Worker[],
  remotes: Remote[],
): string {
  const verb = op === "move" ? "Moves" : "Copies";
  const src = from ? ` from ${endpointText(from, workers, remotes)}` : "";
  const chosen = to && (to.place.kind === "remote" || to.path.trim() !== "");
  const dst = chosen ? endpointText(to, workers, remotes) : "a folder you still have to choose";
  return `${verb} ${itemsText(n)}${src} into ${dst} and ${conflictText(conflict)}.`;
}
