import { WorkerStatus, type Worker } from "@/gen/syncr/v1/api_pb";

import { isWithinRoots } from "./restic-paths";

/**
 * allowedFoldersHelp is the help under a field of absolute paths on a
 * job's worker: where the paths must be, and why the folders cannot be
 * browsed when they cannot. worker is the one whose folders are browsed
 * (the job's, or a label's stand-in); note, when set, replaces the help
 * (e.g. to say which of a label's workers is browsed, or that none is
 * online). plural is for a field of several paths.
 */
export function allowedFoldersHelp(
  worker: Worker | undefined,
  note: string | undefined,
  plural = false,
): string {
  if (note) return note;
  const what = plural ? "Absolute paths" : "An absolute path";
  if (!worker)
    return `${what} on the worker, inside its allowed folders. Choose a worker to browse.`;
  if (worker.status !== WorkerStatus.ONLINE)
    return `${worker.name} is offline, so its folders cannot be browsed now. Type the ${plural ? "paths" : "path"}.`;
  return `${what} on ${worker.name}, inside its allowed folders.`;
}

/**
 * allowedFoldersWarning says which of paths lie outside the allowed
 * folders (local_roots) of the workers a job may run on; undefined when
 * none does, or nothing is known yet. workers are the ones that may run
 * the job: one, or every worker with the job's label. A worker that has
 * not reported in is skipped. The check compares text only (the worker
 * also resolves links), so the UI warns and never blocks.
 */
export function allowedFoldersWarning(paths: string[], workers: Worker[]): string | undefined {
  const typed = paths.map((p) => p.trim()).filter((p) => p !== "");
  const outsideOf = (w: Worker) =>
    typed.filter((p) => !isWithinRoots(p, w.info?.localRoots ?? [], w.info?.os));
  const failing = workers.filter((w) => w.info && outsideOf(w).length > 0);
  if (failing.length === 0) return undefined;
  const outside = typed.filter((p) => failing.some((w) => outsideOf(w).includes(p)));
  const subject =
    typed.length === 1
      ? "This path is"
      : outside.length === 1
        ? `${outside[0]} is`
        : `${joinList(outside)} are`;
  if (workers.length === 1) {
    const [w] = failing;
    const roots = w.info?.localRoots ?? [];
    if (roots.length === 0)
      return `${w.name} has no allowed folders, so it refuses every path on its disks. They are set with local_roots in its configuration.`;
    return `${subject} outside ${w.name}'s allowed folders (${roots.join(", ")}). The worker refuses ${outside.length === 1 ? "it" : "them"}, so the job fails.`;
  }
  const folders = failing.map((w) => {
    const roots = w.info?.localRoots ?? [];
    return `${w.name} (${roots.length ? roots.join(", ") : "none"})`;
  });
  return `${subject} outside the allowed folders of ${joinList(folders)}. Runs on ${failing.length === 1 ? "that worker" : "those workers"} fail.`;
}

function joinList(items: string[]): string {
  if (items.length <= 1) return items.join("");
  return `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`;
}
