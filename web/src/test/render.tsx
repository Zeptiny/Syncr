/**
 * Test helpers: providers around a component, the whole app on a memory
 * history, and Connect router transports that stand in for the API.
 */
import { createRouterTransport, type ConnectRouter, type Transport } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryHistory } from "@tanstack/react-router";
import { render, type RenderResult } from "@testing-library/react";

import { TooltipProvider } from "@/components/ui/tooltip";
import { csrfHeader } from "@/lib/transport";
import { ThemeProvider } from "@/lib/theme";
import { App, createApp, type AppRouter } from "@/router";

/** mockTransport builds an in-memory transport from service handlers. */
export function mockTransport(routes: (router: ConnectRouter) => void): Transport {
  return createRouterTransport(routes, { transport: { interceptors: [csrfHeader] } });
}

/** testQueryClient never retries, so failures surface at once. */
export function testQueryClient(): QueryClient {
  return new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: Infinity }, mutations: { retry: false } },
  });
}

/** renderWithProviders renders a component with the app's providers. */
export function renderWithProviders(
  ui: React.ReactElement,
  { transport = mockTransport(() => {}), queryClient = testQueryClient() } = {},
): RenderResult & { queryClient: QueryClient } {
  const result = render(
    <ThemeProvider>
      <TransportProvider transport={transport}>
        <QueryClientProvider client={queryClient}>
          <TooltipProvider>{ui}</TooltipProvider>
        </QueryClientProvider>
      </TransportProvider>
    </ThemeProvider>,
  );
  return { ...result, queryClient };
}

/** renderApp renders the whole app (router, guards) at a path. */
export function renderApp(
  path: string,
  transport: Transport,
): RenderResult & { router: AppRouter } {
  const app = createApp(transport, createMemoryHistory({ initialEntries: [path] }));
  const result = render(<App {...app} />);
  return { ...result, router: app.router };
}
