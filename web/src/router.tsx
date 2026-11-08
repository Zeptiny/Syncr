import type { Transport } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { QueryClientProvider, type QueryClient } from "@tanstack/react-query";
import { createRouter, RouterProvider, type RouterHistory } from "@tanstack/react-router";

import { NotFound, RouteError } from "@/components/app/route-states";
import { TooltipProvider } from "@/components/ui/tooltip";
import { forgetSession } from "@/hooks/use-session";
import { createQueryClient } from "@/lib/query-client";
import { ThemeProvider } from "@/lib/theme";
import { routeTree } from "@/routeTree.gen";

/** RouterContext is available to every route's beforeLoad and loader. */
export interface RouterContext {
  queryClient: QueryClient;
  transport: Transport;
}

/** createAppRouter builds the router for a query client and transport. */
export function createAppRouter(context: RouterContext, history?: RouterHistory) {
  return createRouter({
    routeTree,
    context,
    history,
    defaultPreload: "intent",
    defaultPreloadStaleTime: 0,
    scrollRestoration: true,
    defaultErrorComponent: RouteError,
    defaultNotFoundComponent: NotFound,
  });
}

/** AppRouter is the app's router type. */
export type AppRouter = ReturnType<typeof createAppRouter>;

declare module "@tanstack/react-router" {
  interface Register {
    router: AppRouter;
  }
}

/**
 * createApp wires a transport into a query client and router. When any
 * call fails with UNAUTHENTICATED, the session is dropped and the user is
 * sent to /login (and back afterwards).
 */
export function createApp(transport: Transport, history?: RouterHistory) {
  // The handler runs only after a query fails, by when router is set.
  const queryClient = createQueryClient({
    onUnauthenticated: (client) => {
      forgetSession(client);
      const loc = router.state.location;
      if (loc.pathname === "/login" || loc.pathname === "/setup") return;
      void router.navigate({ to: "/login", search: { redirect: loc.href } });
    },
  });
  const router: AppRouter = createAppRouter({ queryClient, transport }, history);
  return { router, queryClient, transport };
}

/** App renders the providers and the router. */
export function App({
  router,
  queryClient,
  transport,
}: {
  router: AppRouter;
  queryClient: QueryClient;
  transport: Transport;
}) {
  return (
    <ThemeProvider>
      <TransportProvider transport={transport}>
        <QueryClientProvider client={queryClient}>
          <TooltipProvider delayDuration={300}>
            <RouterProvider router={router} />
          </TooltipProvider>
        </QueryClientProvider>
      </TransportProvider>
    </ThemeProvider>
  );
}
