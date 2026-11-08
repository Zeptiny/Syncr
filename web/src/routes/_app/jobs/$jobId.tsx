import { createFileRoute } from "@tanstack/react-router";

import { JobDetailPage } from "@/features/jobs/job-detail-page";

export const Route = createFileRoute("/_app/jobs/$jobId")({
  component: JobDetail,
});

function JobDetail() {
  const { jobId } = Route.useParams();
  return <JobDetailPage key={jobId} jobId={jobId} />;
}
