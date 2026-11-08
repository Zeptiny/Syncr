import type { Transport } from "@connectrpc/connect";
import { createQueryOptions } from "@connectrpc/connect-query";
import type { QueryClient } from "@tanstack/react-query";
import { redirect } from "@tanstack/react-router";

import { SetupService, type GetSessionResponse } from "@/gen/syncr/v1/auth_pb";
import { sessionQueryOptions } from "@/hooks/use-session";
import { isUnauthenticated } from "@/lib/errors";

interface GuardContext {
  queryClient: QueryClient;
  transport: Transport;
}

/** setupStatusOptions is the GetSetupStatus query (never cached). */
export function setupStatusOptions(transport: Transport) {
  return {
    ...createQueryOptions(SetupService.method.getSetupStatus, {}, { transport }),
    staleTime: 0,
  };
}

/** currentSession returns the session, or undefined when signed out. */
export async function currentSession(ctx: GuardContext): Promise<GetSessionResponse | undefined> {
  try {
    return await ctx.queryClient.fetchQuery(sessionQueryOptions(ctx.transport));
  } catch (err) {
    if (isUnauthenticated(err)) return undefined;
    throw err;
  }
}

/**
 * requireSession is the signed-in guard: without a session it redirects to
 * /setup on a fresh controller, otherwise to /login (and back afterwards).
 */
export async function requireSession(ctx: GuardContext, href: string): Promise<GetSessionResponse> {
  const session = await currentSession(ctx);
  if (session?.user) return session;
  const status = await ctx.queryClient.fetchQuery(setupStatusOptions(ctx.transport));
  if (status.setupRequired) throw redirect({ to: "/setup" });
  throw redirect({ to: "/login", search: { redirect: href } });
}

/** safeRedirect accepts only same-origin paths as a post-login target. */
export function safeRedirect(v: unknown): string | undefined {
  if (typeof v !== "string") return undefined;
  if (!v.startsWith("/") || v.startsWith("//") || v.startsWith("/\\")) return undefined;
  if (v.startsWith("/login") || v.startsWith("/setup")) return undefined;
  return v;
}
