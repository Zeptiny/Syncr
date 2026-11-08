import { createFileRoute } from "@tanstack/react-router";

import { JobsPage } from "@/features/jobs/jobs-page";
import { validateJobsSearch } from "@/features/jobs/search";

export const Route = createFileRoute("/_app/jobs/")({
  validateSearch: validateJobsSearch,
  component: Jobs,
});

function Jobs() {
  return <JobsPage search={Route.useSearch()} />;
}
