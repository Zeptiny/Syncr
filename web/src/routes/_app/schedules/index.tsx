import { createFileRoute } from "@tanstack/react-router";

import { SchedulesPage } from "@/features/schedules/schedules-page";
import { validateSchedulesSearch } from "@/features/schedules/search";

export const Route = createFileRoute("/_app/schedules/")({
  validateSearch: validateSchedulesSearch,
  component: Schedules,
});

function Schedules() {
  return <SchedulesPage search={Route.useSearch()} />;
}
