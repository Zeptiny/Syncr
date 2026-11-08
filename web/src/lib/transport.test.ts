import { createClient, createRouterTransport } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";

import { AuthService } from "@/gen/syncr/v1/auth_pb";

import { csrfHeader } from "./transport";

describe("csrfHeader", () => {
  it("adds X-Syncr-CSRF: 1 to every call", async () => {
    let seen: string | null = null;
    const transport = createRouterTransport(
      ({ service }) =>
        service(AuthService, {
          logout: (_req, ctx) => {
            seen = ctx.requestHeader.get("X-Syncr-CSRF");
            return {};
          },
        }),
      { transport: { interceptors: [csrfHeader] } },
    );
    await createClient(AuthService, transport).logout({});
    expect(seen).toBe("1");
  });
});
