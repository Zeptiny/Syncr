import type { ConnectRouter } from "@connectrpc/connect";

import { AuthMethod, AuthService, SetupService } from "@/gen/syncr/v1/auth_pb";

import * as fx from "./fixtures";

/**
 * signedIn registers the services every signed-in page needs: setup is
 * done and the session is the seeded admin (or alice).
 */
export function signedIn(router: ConnectRouter, now: number, user: "admin" | "alice" = "admin") {
  const u = fx.users(now).find((x) => x.username === user);
  router.service(SetupService, { getSetupStatus: () => ({ setupRequired: false }) });
  router.service(AuthService, {
    getSession: () => ({ user: u, method: AuthMethod.SESSION }),
  });
}
