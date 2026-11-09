import type { Transport } from "@connectrpc/connect";
import { createConnectQueryKey, createQueryOptions, useQuery } from "@connectrpc/connect-query";
import type { QueryClient, QueryKey } from "@tanstack/react-query";

import { AuthMethod, AuthService } from "@/gen/syncr/v1/auth_pb";
import { UserRole, type User } from "@/gen/syncr/v1/users_pb";

/** sessionQueryOptions is the GetSession query, shared by guards and hooks. */
export function sessionQueryOptions(transport: Transport) {
  return {
    ...createQueryOptions(AuthService.method.getSession, {}, { transport }),
    staleTime: 60_000,
  };
}

/** sessionKey matches the GetSession query for any transport. */
export const sessionKey: QueryKey = createConnectQueryKey({
  schema: AuthService.method.getSession,
  cardinality: undefined,
});

/** isSessionKey reports whether a query key belongs to GetSession. */
export function isSessionKey(key: QueryKey): boolean {
  const props = key[1] as { serviceName?: string; methodName?: string } | undefined;
  return props?.serviceName === AuthService.typeName && props.methodName === "GetSession";
}

/** forgetSession drops the cached session so the next guard asks again. */
export function forgetSession(client: QueryClient): void {
  client.removeQueries({ queryKey: sessionKey });
}

/**
 * endSession is the one place that runs when a session ends, whatever the
 * reason (sign out, UNAUTHENTICATED from any call, or the login page opening
 * on a stale cache). It cancels in-flight queries and empties the query and
 * mutation caches, so nothing one user loaded (jobs, remotes, contacts, typed
 * passwords) can be shown to the next. Live streams are tied to mounted
 * components and stop when the router leaves the page.
 */
export function endSession(client: QueryClient): void {
  void client.cancelQueries();
  client.clear();
}

/** Session is the signed-in user as the UI needs it. */
export interface Session {
  user: User | undefined;
  isAdmin: boolean;
  /** viaToken is true when the page authenticated with an API token. */
  viaToken: boolean;
  isPending: boolean;
}

/** useSession returns the signed-in user (already loaded by the route guard). */
export function useSession(): Session {
  const q = useQuery(AuthService.method.getSession, {}, { staleTime: 60_000 });
  const user = q.data?.user;
  return {
    user,
    isAdmin: user?.role === UserRole.ADMIN,
    viaToken: q.data?.method === AuthMethod.API_TOKEN,
    isPending: q.isPending,
  };
}

/** displayName is the name to show for a user. */
export function displayName(user: User | undefined): string {
  if (!user) return "";
  return user.displayName || user.username;
}
