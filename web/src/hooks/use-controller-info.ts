import type { Transport } from "@connectrpc/connect";
import { createQueryOptions, useQuery } from "@connectrpc/connect-query";

import { SystemService } from "@/gen/syncr/v1/api_pb";

/**
 * controllerInfoOptions is the GetInfo query (public: it works signed out).
 * The build never changes while the page is open, so it is never refetched.
 */
export function controllerInfoOptions(transport: Transport) {
  return {
    ...createQueryOptions(SystemService.method.getInfo, {}, { transport }),
    staleTime: Infinity,
  };
}

/** ControllerInfo is the running controller's build, once known. */
export interface ControllerInfo {
  /** The version, e.g. "v2.3.0", "v2.3.0-rc.1" or "dev"; undefined while loading or on error. */
  version: string | undefined;
  isPending: boolean;
  isError: boolean;
}

/** useControllerInfo returns the running controller's version (GetInfo). */
export function useControllerInfo(): ControllerInfo {
  const q = useQuery(SystemService.method.getInfo, {}, { staleTime: Infinity });
  return { version: q.data?.version, isPending: q.isPending, isError: q.isError };
}
