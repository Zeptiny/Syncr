import { createFileRoute } from "@tanstack/react-router";

import { RepositoryDetailPage } from "@/features/repositories/repository-detail-page";
import { validateRepositorySearch } from "@/features/repositories/search";

export const Route = createFileRoute("/_app/repositories/$repositoryId")({
  validateSearch: validateRepositorySearch,
  component: RepositoryDetail,
});

function RepositoryDetail() {
  const { repositoryId } = Route.useParams();
  const { tab } = Route.useSearch();
  return <RepositoryDetailPage key={repositoryId} repositoryId={repositoryId} tab={tab} />;
}
