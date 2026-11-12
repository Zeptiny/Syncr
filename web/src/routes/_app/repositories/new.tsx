import { createFileRoute } from "@tanstack/react-router";

import { NewRepositoryPage } from "@/features/repositories/new-repository-page";
import { validateNewRepositorySearch } from "@/features/repositories/search";

export const Route = createFileRoute("/_app/repositories/new")({
  validateSearch: validateNewRepositorySearch,
  component: NewRepository,
});

function NewRepository() {
  const { remote } = Route.useSearch();
  return <NewRepositoryPage remote={remote} />;
}
