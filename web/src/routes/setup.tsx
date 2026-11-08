import { createFileRoute, redirect } from "@tanstack/react-router";

import { setupStatusOptions } from "@/features/auth/guard";
import { SetupPage, type SetupStep } from "@/features/setup/setup-page";

export const Route = createFileRoute("/setup")({
  validateSearch: (s: Record<string, unknown>): { step?: "worker" } => ({
    step: s.step === "worker" ? "worker" : undefined,
  }),
  beforeLoad: async ({ context, search }) => {
    // The last step runs after setup completed (signed in as the new admin).
    if (search.step === "worker") return;
    const status = await context.queryClient.fetchQuery(setupStatusOptions(context.transport));
    if (!status.setupRequired) throw redirect({ to: "/" });
  },
  component: Setup,
});

function Setup() {
  const { step } = Route.useSearch();
  const initial: SetupStep = step === "worker" ? "worker" : "code";
  return <SetupPage step={initial} />;
}
