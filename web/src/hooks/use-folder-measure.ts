import {
  createConnectQueryKey,
  createQueryOptions,
  useQuery,
  useTransport,
} from "@connectrpc/connect-query";
import { useQueryClient, type Query } from "@tanstack/react-query";
import { useCallback, useMemo, useSyncExternalStore } from "react";

import {
  ProbeEntryType,
  type ProbeChildMeasurement,
  type ProbeEntry,
  type ProbeMeasurement,
} from "@/gen/syncr/v1/link_pb";
import { ProbeService, type ProbeWorkerRef } from "@/gen/syncr/v1/probe_pb";
import { ResticService } from "@/gen/syncr/v1/restic_pb";
import { durationSeconds } from "@/lib/format";
import { joinPath, normalizePath, rootPath, type PathKind } from "@/lib/paths";

import type { PathSource } from "./use-path-listing";

/** MeasuredFolder is a folder's measured totals, from its own or its parent's measurement. */
export interface MeasuredFolder {
  path: string;
  bytes: bigint;
  files: bigint;
  dirs: bigint;
  /** atLeast: the walk stopped early (or, for a measured folder itself, some entries could not be read). */
  atLeast: boolean;
  /** measuredAt is when the measurement arrived (ms since the epoch). */
  measuredAt: number;
}

/** MeasureResult is the open folder's measurement. */
export interface MeasureResult {
  measurement: ProbeMeasurement;
  worker: ProbeWorkerRef | undefined;
  /** elapsed is how long the worker walked, in seconds. */
  elapsed: number;
  measuredAt: number;
}

/** FolderMeasure is the state of "Calculate sizes" for one browser. */
export interface FolderMeasure {
  /** canMeasure: the open folder can be measured (not a worker's list of allowed folders). */
  canMeasure: boolean;
  /** measure measures the open folder (again). */
  measure: () => void;
  /** forget drops the open folder's measurement (the browser's Refresh). */
  forget: () => void;
  isMeasuring: boolean;
  /** error is the last measurement's failure (see isBusy). */
  error: unknown;
  /** result is the open folder's measurement, until it is forgotten or a job changes the place. */
  result: MeasureResult | undefined;
  /** child is a listed folder's totals in the open folder's measurement. */
  child: (entry: ProbeEntry) => ProbeChildMeasurement | undefined;
  /** folders are every measured folder of the source, by path (the tray's items live anywhere). */
  folders: ReadonlyMap<string, MeasuredFolder>;
  /** folderSize is a folder's measured bytes: the table's Size sort and the tray's total. */
  folderSize: (entry: ProbeEntry) => bigint | undefined;
  /** folderAtLeast says a folder's measured bytes are a lower bound. */
  folderAtLeast: (entry: ProbeEntry) => boolean;
}

type Response = {
  measurement?: ProbeMeasurement;
  worker?: ProbeWorkerRef;
  elapsed?: Parameters<typeof durationSeconds>[0];
};

/**
 * measureKey is the cached measurements of a source (all its folders), for
 * invalidating or reading them. A remote's listing worker is part of it
 * only for remotes on a worker's own disk (local).
 */
export function measureKey(source: PathSource, local = false): readonly unknown[] {
  switch (source.kind) {
    case "worker":
      return createConnectQueryKey({
        schema: ProbeService.method.measureWorkerFolder,
        input: { workerId: source.workerId },
        cardinality: undefined,
      });
    case "remote":
      return createConnectQueryKey({
        schema: ProbeService.method.measureRemoteFolder,
        input: local
          ? { remoteId: source.remoteId, workerId: source.workerId ?? "" }
          : { remoteId: source.remoteId },
        cardinality: undefined,
      });
    default:
      return createConnectQueryKey({
        schema: ResticService.method.measureSnapshotFolder,
        input: { repositoryId: source.repositoryId, snapshotId: source.snapshotId },
        cardinality: undefined,
      });
  }
}

const isDir = (e: ProbeEntry) => e.type === ProbeEntryType.DIR;

/** fresh reports whether a cached measurement is shown: it has data and nothing invalidated it. */
function fresh(q: Query): boolean {
  return q.state.data !== undefined && !q.state.isInvalidated;
}

/**
 * collectFolders gathers every measured folder of the cached measurements:
 * each measured folder's own totals and its children's. The newer figure
 * wins where a folder was measured both ways.
 */
export function collectFolders(
  queries: readonly Query[],
  kind: PathKind,
): Map<string, MeasuredFolder> {
  const out = new Map<string, MeasuredFolder>();
  const put = (f: MeasuredFolder) => {
    const had = out.get(f.path);
    if (!had || had.measuredAt <= f.measuredAt) out.set(f.path, f);
  };
  for (const q of queries) {
    if (!fresh(q)) continue;
    const m = (q.state.data as Response).measurement;
    if (!m) continue;
    const at = q.state.dataUpdatedAt;
    const path = normalizePath(m.path === "" ? rootPath(kind) : m.path, kind);
    put({
      path,
      bytes: m.bytes,
      files: m.files,
      dirs: m.dirs,
      atLeast: !m.complete || m.unreadable > 0n,
      measuredAt: at,
    });
    for (const c of m.children)
      put({
        path: joinPath(path, c.name, kind),
        bytes: c.bytes,
        files: c.files,
        dirs: c.dirs,
        atLeast: !m.complete,
        measuredAt: at,
      });
  }
  return out;
}

/**
 * useFolderMeasure measures the open folder of a browser in one pass
 * ("Calculate sizes"): the folder's totals and each child folder's. It
 * runs only when asked (measure). Results stay cached, for every folder of
 * the source, until the browser's Refresh forgets them or a job started
 * from the page invalidates them (invalidatePlaces). A measurement that
 * stopped early, or that could not read some entries, is a lower bound.
 */
export function useFolderMeasure(
  source: PathSource,
  path: string,
  opts: { local?: boolean; enabled?: boolean } = {},
): FolderMeasure {
  const kind = source.kind;
  const enabled = opts.enabled ?? true;
  const local = opts.local ?? false;
  const canMeasure = enabled && !(kind === "worker" && path === rootPath(kind));
  // Never fetched by itself: only measure() runs it.
  const common = { enabled: false, retry: false, staleTime: Infinity } as const;
  const workerInput = { workerId: source.kind === "worker" ? source.workerId : "", path };
  const remoteInput = {
    remoteId: source.kind === "remote" ? source.remoteId : "",
    workerId: source.kind === "remote" ? (source.workerId ?? "") : "",
    path,
  };
  const snapshotInput = {
    repositoryId: source.kind === "snapshot" ? source.repositoryId : "",
    snapshotId: source.kind === "snapshot" ? source.snapshotId : "",
    workerId: source.kind === "snapshot" ? (source.workerId ?? "") : "",
    path,
  };
  const worker = useQuery(ProbeService.method.measureWorkerFolder, workerInput, common);
  const remote = useQuery(ProbeService.method.measureRemoteFolder, remoteInput, common);
  const snapshot = useQuery(ResticService.method.measureSnapshotFolder, snapshotInput, common);
  const transport = useTransport();
  const openKey = () =>
    kind === "worker"
      ? createQueryOptions(ProbeService.method.measureWorkerFolder, workerInput, { transport })
          .queryKey
      : kind === "remote"
        ? createQueryOptions(ProbeService.method.measureRemoteFolder, remoteInput, { transport })
            .queryKey
        : createQueryOptions(ResticService.method.measureSnapshotFolder, snapshotInput, {
            transport,
          }).queryKey;
  const q = kind === "worker" ? worker : kind === "remote" ? remote : snapshot;

  // Every measured folder of the source, kept in step with the cache.
  const client = useQueryClient();
  const cache = client.getQueryCache();
  const key = measureKey(source, local);
  const keyText = JSON.stringify(key);
  const filters = useMemo(() => ({ queryKey: JSON.parse(keyText) as unknown[] }), [keyText]);
  const subscribe = useCallback((onChange: () => void) => cache.subscribe(onChange), [cache]);
  const signature = useSyncExternalStore(subscribe, () =>
    cache
      .findAll(filters)
      .filter(fresh)
      .map((x) => `${x.queryHash}@${x.state.dataUpdatedAt}`)
      .join("|"),
  );
  const folders = useMemo(
    () => collectFolders(signature ? cache.findAll(filters) : [], kind),
    // signature stands for the cached measurements.
    [signature, kind, cache, filters],
  );

  // A disabled query never reads as stale: ask the cache whether Refresh or
  // a job's end invalidated it (the signature above re-renders on that).
  const data = q.data as Response | undefined;
  const invalidated = cache.find({ queryKey: openKey(), exact: true })?.state.isInvalidated ?? true;
  const shown = data?.measurement && !invalidated ? data : undefined;
  const result: MeasureResult | undefined = shown?.measurement
    ? {
        measurement: shown.measurement,
        worker: shown.worker,
        elapsed: durationSeconds(shown.elapsed),
        measuredAt: q.dataUpdatedAt,
      }
    : undefined;
  const children = useMemo(
    () => new Map((result?.measurement.children ?? []).map((c) => [c.name, c])),
    [result?.measurement],
  );

  return {
    canMeasure,
    measure: () => {
      if (canMeasure) void q.refetch();
    },
    forget: () =>
      void client.invalidateQueries({ queryKey: openKey(), exact: true, refetchType: "none" }),
    isMeasuring: q.isFetching,
    error: q.isFetching ? undefined : (q.error ?? undefined),
    result,
    child: (e) => (isDir(e) ? children.get(e.name) : undefined),
    folders,
    folderSize: (e) => (isDir(e) ? folders.get(normalizePath(e.path, kind))?.bytes : undefined),
    folderAtLeast: (e) => (isDir(e) ? !!folders.get(normalizePath(e.path, kind))?.atLeast : false),
  };
}
