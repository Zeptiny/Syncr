import { createFileRoute } from "@tanstack/react-router";

import { NewRemotePage } from "@/features/remotes/new-remote-page";

export const Route = createFileRoute("/_app/remotes/new")({
  validateSearch: (s: Record<string, unknown>): { type?: string } => ({
    type: typeof s.type === "string" && s.type ? s.type : undefined,
  }),
  component: NewRemote,
});

function NewRemote() {
  const { type } = Route.useSearch();
  return <NewRemotePage key={type ?? ""} type={type} />;
}
