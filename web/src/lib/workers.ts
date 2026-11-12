import { WorkerStatus, type Worker } from "@/gen/syncr/v1/api_pb";

const order: Record<WorkerStatus, number> = {
  [WorkerStatus.ONLINE]: 0,
  [WorkerStatus.OFFLINE]: 1,
  [WorkerStatus.PENDING]: 2,
  [WorkerStatus.UNSPECIFIED]: 3,
};

/** sortWorkers puts online workers first, then by name. */
export function sortWorkers(workers: Worker[]): Worker[] {
  return [...workers].sort(
    (a, b) => order[a.status] - order[b.status] || a.name.localeCompare(b.name),
  );
}

/** The link features a worker reports (internal/link/features.go). */
export const FEATURE_PROBE = "probe";
export const FEATURE_RESTIC = "restic.v2";
export const FEATURE_RESTIC_DELETE = "restic.delete";
export const FEATURE_HOOKS = "job.hooks";

/** hasFeature reports whether a worker reported a link feature. */
export function hasFeature(w: Worker | undefined, feature: string): boolean {
  return !!w?.info?.features.includes(feature);
}

/** isOnline reports whether a worker is connected. */
export function isOnline(w: Worker | undefined): boolean {
  return w?.status === WorkerStatus.ONLINE;
}

/**
 * lacksProbes reports whether a worker that has connected is too old for
 * restic repositories and file browsing. A worker that never connected
 * (pending) has reported nothing yet and is not counted.
 */
export function lacksProbes(w: Worker): boolean {
  if (!w.info || w.status === WorkerStatus.PENDING) return false;
  return !hasFeature(w, FEATURE_PROBE) || !hasFeature(w, FEATURE_RESTIC);
}

/**
 * isOutdated reports whether a worker should be upgraded: the controller
 * says so (Worker.outdated: it lacks a link feature this controller knows,
 * or runs an older release; a newer one is not outdated), or it is too old
 * for probes and restic.
 */
export function isOutdated(w: Worker): boolean {
  return w.outdated || lacksProbes(w);
}

/** outdatedHelp explains an outdated worker's badge. */
export function outdatedHelp(w: Worker): string {
  return lacksProbes(w)
    ? "This worker's version cannot browse files or work with restic repositories. Upgrade it."
    : "This worker runs an older release than the controller, or lacks a feature the controller uses. Upgrade it.";
}
