import { createFileRoute, redirect } from "@tanstack/react-router";

import { UserRole } from "@/gen/syncr/v1/users_pb";
import { UsersPage } from "@/features/users/users-page";

export const Route = createFileRoute("/_app/users/")({
  beforeLoad: ({ context }) => {
    if (context.session.user?.role !== UserRole.ADMIN) throw redirect({ to: "/jobs" });
  },
  component: UsersPage,
});
