import { createFileRoute } from "@tanstack/react-router";

import { RemotesPage } from "@/features/remotes/remotes-page";

export const Route = createFileRoute("/_app/remotes/")({
  component: RemotesPage,
});
