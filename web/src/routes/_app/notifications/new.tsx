import { createFileRoute } from "@tanstack/react-router";

import { NewContactPage } from "@/features/notifications/new-contact-page";
import { validateNewContactSearch } from "@/features/notifications/search";

export const Route = createFileRoute("/_app/notifications/new")({
  validateSearch: validateNewContactSearch,
  component: NewContact,
});

function NewContact() {
  const { type } = Route.useSearch();
  return <NewContactPage key={type ?? ""} type={type} />;
}
