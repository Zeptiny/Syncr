import { createFileRoute, redirect } from "@tanstack/react-router";

import { currentSession, safeRedirect, setupStatusOptions } from "@/features/auth/guard";
import { LoginPage } from "@/features/auth/login-page";

export const Route = createFileRoute("/login")({
  validateSearch: (s: Record<string, unknown>): { redirect?: string } => ({
    redirect: safeRedirect(s.redirect),
  }),
  beforeLoad: async ({ context, search }) => {
    const session = await currentSession(context);
    if (session?.user) throw redirect({ href: search.redirect ?? "/jobs" });
    const status = await context.queryClient.fetchQuery(setupStatusOptions(context.transport));
    if (status.setupRequired) throw redirect({ to: "/setup" });
  },
  component: Login,
});

function Login() {
  const { redirect } = Route.useSearch();
  return <LoginPage redirect={redirect} />;
}
