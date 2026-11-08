import { createFileRoute } from "@tanstack/react-router";

import { DashboardPage } from "@/features/dashboard/dashboard-page";
import { validateDashboardSearch } from "@/features/dashboard/search";

export const Route = createFileRoute("/_app/")({
  validateSearch: validateDashboardSearch,
  component: Dashboard,
});

function Dashboard() {
  return <DashboardPage search={Route.useSearch()} />;
}
