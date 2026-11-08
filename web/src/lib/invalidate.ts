import type { DescService } from "@bufbuild/protobuf";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import type { QueryClient } from "@tanstack/react-query";

/** invalidateService marks every cached query of a service as stale. */
export function invalidateService(client: QueryClient, service: DescService): Promise<void> {
  return client.invalidateQueries({
    queryKey: createConnectQueryKey({ schema: service, cardinality: undefined }),
  });
}
