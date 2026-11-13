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
 */
export async function invalidatePlaces(client: QueryClient, places: Place[]): Promise<void> {
  const keys: readonly unknown[][] = places.flatMap((p): unknown[][] =>
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
      : [
          createConnectQueryKey({
            schema: ProbeService.method.listRemoteFiles,
            input: { remoteId: p.remoteId },
            cardinality: undefined,
          }),
          createConnectQueryKey({
            schema: ProbeService.method.getRemoteUsage,
            input: { remoteId: p.remoteId },
            cardinality: undefined,
          }),
          createConnectQueryKey({
            schema: ProbeService.method.measureRemoteFolder,
            input: { remoteId: p.remoteId },
            cardinality: undefined,
          }),
        ],
  );
  await Promise.all(keys.map((queryKey) => client.invalidateQueries({ queryKey })));
}
