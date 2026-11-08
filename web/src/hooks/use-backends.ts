import { useQuery } from "@connectrpc/connect-query";
import { useMemo } from "react";

import { BackendService, type Backend } from "@/gen/syncr/v1/remotes_pb";

/**
 * useBackends returns the backend descriptors. They are static for the
 * controller's lifetime, so they are fetched once.
 */
export function useBackends() {
  return useQuery(
    BackendService.method.listBackends,
    {},
    { staleTime: Infinity, gcTime: Infinity },
  );
}

/** useBackendMap indexes the descriptors by ID. */
export function useBackendMap(): Map<string, Backend> {
  const { data } = useBackends();
  return useMemo(() => new Map((data?.backends ?? []).map((b) => [b.id, b])), [data]);
}
