// Refreshing the file browser's listings after a job changed a folder.
import { createConnectQueryKey } from "@connectrpc/connect-query";
import type { QueryClient } from "@tanstack/react-query";

import { ProbeService } from "@/gen/syncr/v1/probe_pb";

import type { Place } from "./transfer";

/**
 * invalidatePlaces marks the cached listings of places as stale, so the
 * open browser lists them again: every folder of a worker, or of a remote
 * through any worker, and the remote's usage. Their measured folder sizes
 * are dropped too (they are measured again only when asked).
 *
 * A worker's disk is reached two ways, as the worker's folders and as
 * local remotes listed by that worker, so a job that wrote one refreshes
 * the other: a worker's place also refreshes what was listed through it,
 * and a local remote's the folders of its worker.
 */
export async function invalidatePlaces(client: QueryClient, places: Place[]): Promise<void> {
  const disks = places.flatMap((p): Place[] =>
    p.kind === "worker"
      ? [p, { kind: "remote", remoteId: "", local: true, workerId: p.workerId }]
      : p.local && p.workerId
        ? [p, { kind: "worker", workerId: p.workerId }]
        : [p],
  );
  const keys: readonly unknown[][] = disks.flatMap((p): unknown[][] =>
    p.kind === "worker"
      ? [
          createConnectQueryKey({
            schema: ProbeService.method.listWorkerFiles,
            input: { workerId: p.workerId },
            cardinality: undefined,
          }),
          createConnectQueryKey({
            schema: ProbeService.method.measureWorkerFolder,
            input: { workerId: p.workerId },
            cardinality: undefined,
          }),
        ]
      : remoteKeys(p.remoteId ? { remoteId: p.remoteId } : { workerId: p.workerId ?? "" }),
  );
  await Promise.all(keys.map((queryKey) => client.invalidateQueries({ queryKey })));
}

// remoteKeys are the keys of a remote's listings, usage and measurements:
// one remote's, or (by workerId) every remote listed by that worker.
function remoteKeys(input: { remoteId: string } | { workerId: string }): unknown[][] {
  return [
    createConnectQueryKey({
      schema: ProbeService.method.listRemoteFiles,
      input,
      cardinality: undefined,
    }),
    createConnectQueryKey({
      schema: ProbeService.method.getRemoteUsage,
      input,
      cardinality: undefined,
    }),
    createConnectQueryKey({
      schema: ProbeService.method.measureRemoteFolder,
      input,
      cardinality: undefined,
    }),
  ];
}
