import { createFileRoute } from "@tanstack/react-router";

import { ComparePage } from "@/features/repositories/compare-page";
import { validateCompareSearch } from "@/features/repositories/search";

export const Route = createFileRoute("/_app/repositories/$repositoryId_/compare")({
  validateSearch: validateCompareSearch,
  component: RepositoryCompare,
});

function RepositoryCompare() {
  const { repositoryId } = Route.useParams();
  const search = Route.useSearch();
  return <ComparePage key={repositoryId} repositoryId={repositoryId} {...search} />;
}
