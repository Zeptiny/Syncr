import { createFileRoute, redirect } from "@tanstack/react-router";

import { UserRole } from "@/gen/syncr/v1/users_pb";
import { AddWorkerPage, type AddWorkerTab } from "@/features/workers/add-worker-page";

export const Route = createFileRoute("/_app/workers/new")({
  validateSearch: (s: Record<string, unknown>): { tab?: AddWorkerTab } => ({
    tab: s.tab === "passive" ? "passive" : undefined,
  }),
  beforeLoad: ({ context }) => {
    if (context.session.user?.role !== UserRole.ADMIN) throw redirect({ to: "/workers" });
  },
  component: AddWorker,
});

function AddWorker() {
  const { tab } = Route.useSearch();
  return <AddWorkerPage tab={tab ?? "active"} />;
}
