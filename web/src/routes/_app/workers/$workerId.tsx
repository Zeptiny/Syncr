import { createFileRoute } from "@tanstack/react-router";

import { WorkerDetailPage } from "@/features/workers/worker-detail-page";

export const Route = createFileRoute("/_app/workers/$workerId")({
  component: WorkerDetail,
});

function WorkerDetail() {
  const { workerId } = Route.useParams();
  return <WorkerDetailPage key={workerId} workerId={workerId} />;
}
