import { createFileRoute } from "@tanstack/react-router";

import { ContactPage } from "@/features/notifications/contact-page";

export const Route = createFileRoute("/_app/notifications/$contactId")({
  component: Contact,
});

function Contact() {
  const { contactId } = Route.useParams();
  return <ContactPage key={contactId} contactId={contactId} />;
}
