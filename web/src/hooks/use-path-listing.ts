import { useQuery } from "@connectrpc/connect-query";

import type { ProbeEntry } from "@/gen/syncr/v1/link_pb";
import { ProbeService, type ProbeWorkerRef } from "@/gen/syncr/v1/probe_pb";
import { ResticService } from "@/gen/syncr/v1/restic_pb";
import type { PathKind } from "@/lib/paths";

/**
 * PathSource is a place whose folders can be listed: a worker's allowed
 * folders, a remote (through a worker, chosen by the controller unless
 * workerId is set) or a snapshot's tree.
 */
export type PathSource =
  | { kind: "worker"; workerId: string }
  | { kind: "remote"; remoteId: string; workerId?: string }
  | { kind: "snapshot"; repositoryId: string; snapshotId: string; workerId?: string };

/** PathListing is one listed folder and the state of its request. */
export interface PathListing {
  /** path is the listed path as the worker reports it. */
  path: string;
  entries: ProbeEntry[] | undefined;
  /** truncated says the folder holds more entries than were returned. */
  truncated: boolean;
  /** worker is the worker that answered. */
  worker: ProbeWorkerRef | undefined;
  /**
   * workerId is the worker the listing is about or came from: a worker
   * source's own worker, else the chosen one, else the one that answered.
   */
  workerId: string | undefined;
  /** dataUpdatedAt is when the shown listing arrived (ms since the epoch; 0 before). */
  dataUpdatedAt: number;
  isLoading: boolean;
  isFetching: boolean;
  error: unknown;
  refetch: () => void;
}

/** sourceKind is how the paths of a source are written. */
export function sourceKind(source: PathSource): PathKind {
  return source.kind;
}

/**
 * sourceKey identifies a source: a selection made in one is cleared when
 * the key changes. The listing worker of a remote is part of it only for
 * remotes on a worker's own disk (local), where another worker is another
 * disk; pass local for those.
 */
export function sourceKey(source: PathSource, local = false): string {
  switch (source.kind) {
    case "worker":
      return `worker:${source.workerId}`;
    case "remote":
      return `remote:${source.remoteId}${local ? `@${source.workerId ?? ""}` : ""}`;
    default:
      return `snapshot:${source.repositoryId}:${source.snapshotId}`;
  }
}

/**
 * usePathListing lists one folder of a source. Listings are asked from a
 * worker, so they take a moment and can fail when no worker is reachable;
 * failures are not retried automatically.
 */
export function usePathListing(
  source: PathSource,
  path: string,
  opts: { enabled?: boolean; dirsOnly?: boolean } = {},
): PathListing {
  const enabled = opts.enabled ?? true;
  const dirsOnly = opts.dirsOnly ?? false;
  const common = { retry: false, staleTime: 30_000 } as const;
  const worker = useQuery(
    ProbeService.method.listWorkerFiles,
    { workerId: source.kind === "worker" ? source.workerId : "", path, dirsOnly },
    { ...common, enabled: enabled && source.kind === "worker" && source.workerId !== "" },
  );
  const remote = useQuery(
    ProbeService.method.listRemoteFiles,
    {
      remoteId: source.kind === "remote" ? source.remoteId : "",
      workerId: source.kind === "remote" ? (source.workerId ?? "") : "",
      path,
      dirsOnly,
    },
    { ...common, enabled: enabled && source.kind === "remote" && source.remoteId !== "" },
  );
  const snapshot = useQuery(
    ResticService.method.browseSnapshot,
    {
      repositoryId: source.kind === "snapshot" ? source.repositoryId : "",
      snapshotId: source.kind === "snapshot" ? source.snapshotId : "",
      workerId: source.kind === "snapshot" ? (source.workerId ?? "") : "",
      path,
    },
    { ...common, enabled: enabled && source.kind === "snapshot" && source.snapshotId !== "" },
  );
  const q = source.kind === "worker" ? worker : source.kind === "remote" ? remote : snapshot;
  return {
    path: q.data?.path ?? path,
    entries: q.data?.entries,
    truncated: q.data?.truncated ?? false,
    worker: q.data?.worker,
    workerId:
      source.kind === "worker"
        ? source.workerId
        : source.workerId || q.data?.worker?.id || undefined,
    dataUpdatedAt: q.data ? q.dataUpdatedAt : 0,
    isLoading: q.isLoading,
    isFetching: q.isFetching,
    error: q.error ?? undefined,
    refetch: () => void q.refetch(),
  };
}
