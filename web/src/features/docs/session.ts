import type { Transport } from "@connectrpc/connect";
import type { QueryClient } from "@tanstack/react-query";

import { currentSession } from "@/features/auth/guard";
import type { GetSessionResponse } from "@/gen/syncr/v1/auth_pb";
import { sessionQueryOptions } from "@/hooks/use-session";
import { isUnauthenticated } from "@/lib/errors";

/**
 * docsSession is the session of a docs reader, or undefined when signed out
 * or when the check fails (the docs are public). A signed-out answer is
 * reused as long as a signed-in one is (the session query's staleTime), so a
 * signed-out reader moving between pages does not ask on every navigation.
 * Signing in (or out) empties the cache, which ends the reuse.
 */
export async function docsSession(ctx: {
  queryClient: QueryClient;
  transport: Transport;
}): Promise<GetSessionResponse | undefined> {
  const options = sessionQueryOptions(ctx.transport);
  const state = ctx.queryClient.getQueryState(options.queryKey);
  if (
    state?.status === "error" &&
    isUnauthenticated(state.error) &&
    Date.now() - state.errorUpdatedAt < options.staleTime
  ) {
    return undefined;
  }
  try {
    return await currentSession(ctx);
  } catch {
    return undefined;
  }
}
