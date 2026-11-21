import { createFileRoute, Outlet } from "@tanstack/react-router";

import { currentSession } from "@/features/auth/guard";
import { DocsRoot } from "@/features/docs/docs-root";

// The docs are public: signed in they render inside the app shell, signed
// out in their own frame. A failing session check only means signed out.
export const Route = createFileRoute("/docs")({
  beforeLoad: async ({ context }) => {
    try {
      return { session: await currentSession(context) };
    } catch {
      return { session: undefined };
    }
  },
  component: Docs,
});

function Docs() {
  const { session } = Route.useRouteContext();
  return (
    <DocsRoot signedIn={!!session?.user}>
      <Outlet />
    </DocsRoot>
  );
}
