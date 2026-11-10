import { createFileRoute } from "@tanstack/react-router";

import { RepositoriesPage } from "@/features/repositories/repositories-page";

export const Route = createFileRoute("/_app/repositories/")({
  component: RepositoriesPage,
});
