import { createFileRoute } from "@tanstack/react-router";

import { JobsPage, validateJobsSearch } from "@/features/jobs/jobs-page";

export const Route = createFileRoute("/_app/jobs/")({
  validateSearch: validateJobsSearch,
  component: Jobs,
});

function Jobs() {
  return <JobsPage search={Route.useSearch()} />;
}
