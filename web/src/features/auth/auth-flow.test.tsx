import { Code, ConnectError } from "@connectrpc/connect";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { QueryClient } from "@tanstack/react-query";
import { createMemoryHistory } from "@tanstack/react-router";
import { render } from "@testing-library/react";
import { describe, expect, it } from "vitest";

import { WorkerAdminService, JobService } from "@/gen/syncr/v1/api_pb";
import { AuthMethod, AuthService, SetupService } from "@/gen/syncr/v1/auth_pb";
import { BackendService, RemoteService } from "@/gen/syncr/v1/remotes_pb";
import * as fx from "@/test/fixtures";
import { App, createApp } from "@/router";
import { mockTransport, renderApp } from "@/test/render";

import { safeRedirect } from "./guard";

const now = Date.parse("2026-03-12T12:00:00Z");

function api(opts: { signedIn: boolean; setupRequired?: boolean }) {
  const state = { signedIn: opts.signedIn, csrf: [] as (string | null)[] };
  const transport = mockTransport(({ service }) => {
    service(SetupService, { getSetupStatus: () => ({ setupRequired: !!opts.setupRequired }) });
    service(AuthService, {
      getSession: () => {
        if (!state.signedIn) throw new ConnectError("no session", Code.Unauthenticated);
        return { user: fx.users(now)[0], method: AuthMethod.SESSION };
      },
      login: (req, ctx) => {
        state.csrf.push(ctx.requestHeader.get("x-syncr-csrf"));
        if (req.password !== "syncr-demo-admin")
          throw new ConnectError("bad", Code.Unauthenticated);
        state.signedIn = true;
        return { user: fx.users(now)[0] };
      },
    });
    const authed =
      <T,>(v: T) =>
      () => {
        if (!state.signedIn) throw new ConnectError("no session", Code.Unauthenticated);
        return v;
      };
    service(JobService, { listJobs: authed({ jobs: fx.jobs(now).slice(0, 3) }) });
    service(WorkerAdminService, { listWorkers: () => ({ workers: fx.workers(now) }) });
    service(RemoteService, { listRemotes: () => ({ remotes: fx.remotes(now) }) });
    service(BackendService, { listBackends: () => ({ backends: fx.backends }) });
  });
  return { transport, state };
}

/** servicesCached lists the services that have a query in the cache. */
function servicesCached(client: QueryClient): string[] {
  return client
    .getQueryCache()
    .getAll()
    .map((q) => (q.queryKey[1] as { serviceName?: string } | undefined)?.serviceName ?? "")
    .map((n) => n.split(".").pop() ?? "");
}

describe("auth flow", () => {
  it("sends a signed-out user to /login and back after signing in", async () => {
    const user = userEvent.setup();
    const { transport, state } = api({ signedIn: false });
    const { router } = renderApp("/remotes?x=1", transport);

    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/login");
    expect(router.state.location.search).toEqual({ redirect: "/remotes?x=1" });

    await user.type(screen.getByLabelText("Username"), "admin");
    await user.type(screen.getByLabelText("Password"), "wrong-password");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("don't match");

    await user.clear(screen.getByLabelText("Password"));
    await user.type(screen.getByLabelText("Password"), "syncr-demo-admin");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("heading", { name: "Remotes" })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/remotes");
    expect(state.csrf).toEqual(["1", "1"]);
  });

  it("sends a fresh controller to the setup wizard", async () => {
    const { transport } = api({ signedIn: false, setupRequired: true });
    const { router } = renderApp("/jobs", transport);
    expect(await screen.findByRole("heading", { name: "Set up Syncr" })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/setup");
  });

  it("lets a signed-in user through and away from /login", async () => {
    const { transport } = api({ signedIn: true });
    const { router } = renderApp("/login", transport);
    await waitFor(() => expect(router.state.location.pathname).toBe("/"));
    expect(await screen.findByRole("heading", { name: "Dashboard" })).toBeInTheDocument();
  });

  it("forgets everything cached when the session expires", async () => {
    const { transport, state } = api({ signedIn: true });
    const { router, queryClient } = renderApp("/jobs", transport);
    expect(await screen.findByRole("heading", { name: "Jobs" })).toBeInTheDocument();
    await waitFor(() => expect(servicesCached(queryClient)).toContain("JobService"));

    // The session expires; the next refetch fails with UNAUTHENTICATED.
    state.signedIn = false;
    await queryClient.invalidateQueries();
    await waitFor(() => expect(router.state.location.pathname).toBe("/login"));
    expect(router.state.location.search).toEqual({ redirect: "/jobs" });
    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    await waitFor(() => expect(servicesCached(queryClient)).not.toContain("JobService"));
    expect(servicesCached(queryClient)).not.toContain("WorkerAdminService");
    expect(servicesCached(queryClient)).not.toContain("RemoteService");
  });

  it("drops a stale cache when the login page opens", async () => {
    const { transport } = api({ signedIn: false });
    const app = createApp(transport, createMemoryHistory({ initialEntries: ["/login"] }));
    app.queryClient.setQueryData(["previous-user"], { jobs: ["secret"] });
    render(<App {...app} />);
    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    await waitFor(() => expect(app.queryClient.getQueryData(["previous-user"])).toBeUndefined());
  });

  it("clears the cache when someone signs in", async () => {
    const user = userEvent.setup();
    const { transport } = api({ signedIn: false });
    const { queryClient } = renderApp("/login", transport);
    await screen.findByRole("heading", { name: "Sign in" });
    queryClient.setQueryData(["late"], 1);
    await user.type(screen.getByLabelText("Username"), "admin");
    await user.type(screen.getByLabelText("Password"), "syncr-demo-admin");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("heading", { name: "Dashboard" })).toBeInTheDocument();
    expect(queryClient.getQueryData(["late"])).toBeUndefined();
  });

  it("only redirects to same-origin paths", () => {
    expect(safeRedirect("/jobs?status=5")).toBe("/jobs?status=5");
    expect(safeRedirect("//evil.example")).toBeUndefined();
    expect(safeRedirect("https://evil.example")).toBeUndefined();
    expect(safeRedirect("/\\evil")).toBeUndefined();
    expect(safeRedirect("/login")).toBeUndefined();
    expect(safeRedirect(3)).toBeUndefined();
  });
});
