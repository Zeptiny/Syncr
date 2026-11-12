import { MutationCache, QueryCache, QueryClient } from "@tanstack/react-query";

import { isSessionKey } from "@/hooks/use-session";
import { isUnauthenticated, retryable } from "@/lib/errors";

/** QueryClientOptions configures createQueryClient. */
export interface QueryClientOptions {
  /**
   * onUnauthenticated runs when any query or mutation fails with
   * UNAUTHENTICATED (session expired or revoked). The app invalidates the
   * session and navigates to /login.
   */
  onUnauthenticated?: (client: QueryClient) => void;
}

const unauthenticatedHandlers = new WeakMap<QueryClient, (client: QueryClient) => void>();

/**
 * reportUnauthenticated sends err through the session-expiry path of client
 * when it is UNAUTHENTICATED. Calls that bypass the query cache (live
 * streams) use it, so an expired session ends the same way everywhere.
 * It reports whether err was UNAUTHENTICATED.
 */
export function reportUnauthenticated(client: QueryClient, err: unknown): boolean {
  if (!isUnauthenticated(err)) return false;
  unauthenticatedHandlers.get(client)?.(client);
  return true;
}

/** createQueryClient returns the app's QueryClient. */
export function createQueryClient(opts: QueryClientOptions = {}): QueryClient {
  const handle = (err: unknown) => {
    reportUnauthenticated(client, err);
  };
  const client: QueryClient = new QueryClient({
    // A failing GetSession is how the guards detect a signed-out user;
    // they redirect themselves.
    queryCache: new QueryCache({
      onError: (err, query) => {
        if (!isSessionKey(query.queryKey)) handle(err);
      },
    }),
    mutationCache: new MutationCache({ onError: handle }),
    defaultOptions: {
      queries: {
        staleTime: 10_000,
        refetchOnWindowFocus: true,
        retry: (count, err) => count < 2 && retryable(err),
      },
      mutations: { retry: false },
    },
  });
  if (opts.onUnauthenticated) unauthenticatedHandlers.set(client, opts.onUnauthenticated);
  return client;
}
