import { createFileRoute } from "@tanstack/react-router";

import { SchedulesPage } from "@/features/schedules/schedules-page";

export const Route = createFileRoute("/_app/schedules/")({
  component: SchedulesPage,
});
