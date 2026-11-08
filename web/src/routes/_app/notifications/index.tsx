import { createFileRoute } from "@tanstack/react-router";

import { NotificationsPage } from "@/features/notifications/contacts-page";
import { validateNotificationsSearch } from "@/features/notifications/search";

export const Route = createFileRoute("/_app/notifications/")({
  validateSearch: validateNotificationsSearch,
  component: Notifications,
});

function Notifications() {
  return <NotificationsPage search={Route.useSearch()} />;
}
