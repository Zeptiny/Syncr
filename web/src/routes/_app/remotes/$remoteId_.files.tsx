import { createFileRoute } from "@tanstack/react-router";

import { RemoteFilesPage } from "@/features/files/remote-files-page";
import { validateRemoteFilesSearch } from "@/features/files/search";

export const Route = createFileRoute("/_app/remotes/$remoteId_/files")({
  validateSearch: validateRemoteFilesSearch,
  component: RemoteFiles,
});

function RemoteFiles() {
  const { remoteId } = Route.useParams();
  const { path, worker } = Route.useSearch();
  return <RemoteFilesPage key={remoteId} remoteId={remoteId} path={path} worker={worker} />;
}
