import { createFileRoute } from "@tanstack/react-router";

import { SearchPage } from "@/features/repositories/search-page";
import { validateFindSearch } from "@/features/repositories/search";

export const Route = createFileRoute("/_app/repositories/$repositoryId_/search")({
  validateSearch: validateFindSearch,
  component: RepositorySearch,
});

function RepositorySearch() {
  const { repositoryId } = Route.useParams();
  const search = Route.useSearch();
  return <SearchPage key={repositoryId} repositoryId={repositoryId} {...search} />;
}
