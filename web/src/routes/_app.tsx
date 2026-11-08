import { createFileRoute, Outlet } from "@tanstack/react-router";

import { AppShell } from "@/components/app/app-shell";
import { requireSession } from "@/features/auth/guard";

export const Route = createFileRoute("/_app")({
  beforeLoad: async ({ context, location }) => ({
    session: await requireSession(context, location.href),
  }),
  component: Layout,
});

function Layout() {
  return (
    <AppShell>
      <Outlet />
    </AppShell>
  );
}
