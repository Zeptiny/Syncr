import { createFileRoute } from "@tanstack/react-router";

import { WorkersPage } from "@/features/workers/workers-page";
import { validateWorkersSearch } from "@/features/workers/search";

export const Route = createFileRoute("/_app/workers/")({
  validateSearch: validateWorkersSearch,
  component: Workers,
});

function Workers() {
  return <WorkersPage label={Route.useSearch().label} />;
}
