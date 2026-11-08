import { createFileRoute } from "@tanstack/react-router";

import { EditRemotePage } from "@/features/remotes/edit-remote-page";

export const Route = createFileRoute("/_app/remotes/$remoteId")({
  component: EditRemote,
});

function EditRemote() {
  const { remoteId } = Route.useParams();
  return <EditRemotePage key={remoteId} remoteId={remoteId} />;
}
