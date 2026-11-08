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

/** createQueryClient returns the app's QueryClient. */
export function createQueryClient(opts: QueryClientOptions = {}): QueryClient {
  const handle = (err: unknown) => {
    if (isUnauthenticated(err)) opts.onUnauthenticated?.(client);
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
  return client;
}
