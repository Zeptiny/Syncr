import { createFileRoute, Outlet } from "@tanstack/react-router";

import { DocsRoot } from "@/features/docs/docs-root";
import { docsSession } from "@/features/docs/session";

// The docs are public: signed in they render inside the app shell, signed
// out in their own frame. A failing session check only means signed out.
export const Route = createFileRoute("/docs")({
  beforeLoad: async ({ context }) => ({ session: await docsSession(context) }),
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
