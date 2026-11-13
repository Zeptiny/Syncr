import { createFileRoute } from "@tanstack/react-router";

import { validateSnapshotSearch } from "@/features/repositories/search";
import { SnapshotBrowserPage } from "@/features/repositories/snapshot-browser-page";

export const Route = createFileRoute("/_app/repositories/$repositoryId_/snapshots/$snapshotId")({
  validateSearch: validateSnapshotSearch,
  component: SnapshotBrowser,
});

function SnapshotBrowser() {
  const { repositoryId, snapshotId } = Route.useParams();
  const { path, worker } = Route.useSearch();
  return (
    <SnapshotBrowserPage
      key={`${repositoryId}:${snapshotId}`}
      repositoryId={repositoryId}
      snapshotId={snapshotId}
      path={path}
      worker={worker}
    />
  );
}
