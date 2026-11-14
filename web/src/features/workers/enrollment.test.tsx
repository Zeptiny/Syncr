import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import {
  EnrollmentTokenSchema,
  EnrollmentTokenState,
  InstallPlatform,
  InstallScope,
  WorkerAdminService,
  WorkerInfoSchema,
  WorkerSchema,
  WorkerStatus,
  type Worker,
} from "@/gen/syncr/v1/api_pb";
import { WorkerMode } from "@/gen/syncr/v1/link_pb";
import { signedIn } from "@/test/app-api";
import { mockTransport, renderApp, renderInRouter } from "@/test/render";

import { ActiveEnrollment } from "./enrollment";
import { enrollmentStage } from "./enrollment-progress";

const now = Date.now();
const tokenId = "0199a3b0-0000-7000-8000-000000000001";
const workerId = "0199a3b0-0000-7000-8000-0000000000aa";
const command = "sudo /usr/local/bin/syncr-worker setup --token 'syncr_et_abc'";

function pendingWorker(): Worker {
  return create(WorkerSchema, {
    id: workerId,
    name: "studio-pc",
    mode: WorkerMode.ACTIVE,
    status: WorkerStatus.PENDING,
    createdAt: timestampFromMs(now),
  });
}

function connectedWorker(localRoots = ["/srv/media"]): Worker {
  const w = pendingWorker();
  w.status = WorkerStatus.ONLINE;
  w.lastSeen = timestampFromMs(now);
  w.info = create(WorkerInfoSchema, {
    workerVersion: "v2.1.0",
    os: "linux",
    arch: "amd64",
    hostname: "studio.lan",
    tools: [
      { name: "rclone", version: "v1.75.2" },
      { name: "restic", version: "0.19.1" },
    ],
    localRoots,
    reportedAt: timestampFromMs(now),
  });
  return w;
}

/**
 * enrollmentApi serves an enrollment the test moves along: the token's
 * state (or gone), and the worker GetWorker returns.
 */
function enrollmentApi(
  initial: { token?: EnrollmentTokenState; gone?: boolean; worker?: Worker } = {},
) {
  const s = {
    token: initial.token ?? EnrollmentTokenState.WAITING,
    gone: initial.gone ?? false,
    worker: initial.worker ?? pendingWorker(),
  };
  const calls = { token: 0, worker: 0 };
  const transport = mockTransport((r) => {
    signedIn(r, now);
    r.service(WorkerAdminService, {
      createEnrollmentToken: () => ({
        token: "syncr_et_abc",
        tokenId,
        expiresAt: timestampFromMs(now + 3_600_000),
        installCommands: [{ platform: InstallPlatform.LINUX, scope: InstallScope.SYSTEM, command }],
      }),
      getEnrollmentToken: (req) => {
        calls.token++;
        expect(req.tokenId).toBe(tokenId);
        if (s.gone) throw new ConnectError("enrollment token not found", Code.NotFound);
        const used = s.token === EnrollmentTokenState.USED;
        return {
          token: create(EnrollmentTokenSchema, {
            id: tokenId,
            state: s.token,
            createdAt: timestampFromMs(now),
            expiresAt: timestampFromMs(now + 3_600_000),
            usedAt: used ? timestampFromMs(now) : undefined,
            workerId: used ? workerId : "",
          }),
        };
      },
      getWorker: (req) => {
        calls.worker++;
        expect(req.workerId).toBe(workerId);
        return { worker: s.worker };
      },
    });
  });
  return { s, calls, transport };
}

/** settled waits long enough for several polls at 20 ms. */
const settled = () => new Promise((r) => setTimeout(r, 150));

describe("enrollmentStage", () => {
  const token = (state: EnrollmentTokenState) => create(EnrollmentTokenSchema, { state });
  it("follows the token, then the worker's first report", () => {
    expect(enrollmentStage(undefined, undefined, false)).toBe("waiting");
    expect(enrollmentStage(token(EnrollmentTokenState.WAITING), undefined, false)).toBe("waiting");
    expect(enrollmentStage(token(EnrollmentTokenState.USED), undefined, false)).toBe("enrolled");
    // Seen by the controller is not enough: it has not reported what it is.
    const seen = pendingWorker();
    seen.status = WorkerStatus.ONLINE;
    expect(enrollmentStage(token(EnrollmentTokenState.USED), seen, false)).toBe("enrolled");
    expect(enrollmentStage(token(EnrollmentTokenState.USED), connectedWorker(), false)).toBe(
      "connected",
    );
    expect(enrollmentStage(token(EnrollmentTokenState.EXPIRED), undefined, false)).toBe("expired");
    expect(enrollmentStage(undefined, undefined, true)).toBe("expired");
  });
});

describe("ActiveEnrollment", { timeout: 30_000 }, () => {
  it("follows the worker from the command to its first connection, then stops asking", async () => {
    const user = userEvent.setup();
    const api = enrollmentApi();
    const onConnected = vi.fn();
    const r = renderInRouter(<ActiveEnrollment onConnected={onConnected} pollMs={20} />, {
      transport: api.transport,
    });
    await user.click(await screen.findByRole("button", { name: "Generate install command" }));

    const progress = await screen.findByRole("region", { name: "Adding the worker" });
    expect(screen.getByText(command)).toBeVisible();
    expect(progress).toHaveTextContent(
      /Waiting for it to run\. The command expires in 60 minutes\./,
    );
    expect(screen.getByRole("status")).toHaveTextContent("Waiting for the command to run.");
    await waitFor(() => expect(api.calls.token).toBeGreaterThan(2));
    expect(api.calls.worker).toBe(0);

    api.s.token = EnrollmentTokenState.USED;
    await waitFor(() => expect(progress).toHaveTextContent("As studio-pc."));
    // The command is spent: it is no longer offered.
    expect(screen.queryByText(command)).toBeNull();
    expect(progress).toHaveTextContent("Waiting for it to connect.");
    expect(screen.getByRole("status")).toHaveTextContent(
      "studio-pc enrolled. Waiting for it to connect.",
    );
    expect(within(progress).queryByRole("link")).toBeNull();
    const tokenCalls = api.calls.token;
    await waitFor(() => expect(api.calls.worker).toBeGreaterThan(2));
    expect(api.calls.token).toBe(tokenCalls);
    expect(onConnected).not.toHaveBeenCalled();

    api.s.worker = connectedWorker();
    await waitFor(() =>
      expect(screen.getByRole("status")).toHaveTextContent("studio-pc is connected."),
    );
    expect(progress).toHaveTextContent("studio.lan");
    expect(progress).toHaveTextContent("linux/amd64");
    expect(progress).toHaveTextContent("v2.1.0");
    expect(progress).toHaveTextContent("rclone v1.75.2, restic 0.19.1");
    expect(progress).toHaveTextContent("/srv/media");
    expect(progress).not.toHaveTextContent("local_roots");
    const open = within(progress).getByRole("link", { name: "Open studio-pc" });
    expect(open).toHaveAttribute("href", `/workers/${workerId}`);
    expect(open).toHaveAttribute("data-variant", "default");
    expect(onConnected).toHaveBeenCalledTimes(1);
    expect(onConnected.mock.calls[0][0]).toMatchObject({ id: workerId });

    const workerCalls = api.calls.worker;
    await settled();
    expect(api.calls.worker).toBe(workerCalls);
    expect(onConnected).toHaveBeenCalledTimes(1);

    await user.click(open);
    expect(r.location().pathname).toBe(`/workers/${workerId}`);
  });

  it("warns when the worker has no allowed folders", async () => {
    const user = userEvent.setup();
    const api = enrollmentApi({
      token: EnrollmentTokenState.USED,
      worker: connectedWorker([]),
    });
    renderInRouter(<ActiveEnrollment pollMs={20} />, { transport: api.transport });
    await user.click(await screen.findByRole("button", { name: "Generate install command" }));
    const progress = await screen.findByRole("region", { name: "Adding the worker" });
    await waitFor(() =>
      expect(progress).toHaveTextContent(
        "None, so jobs on this worker cannot read or write its own disks",
      ),
    );
    expect(progress).toHaveTextContent("add them to local_roots");
  });

  it.each([
    ["expired", { token: EnrollmentTokenState.EXPIRED }],
    ["deleted", { gone: true }],
  ])("says when the command %s unused, offers a new one and stops asking", async (_, initial) => {
    const user = userEvent.setup();
    const api = enrollmentApi(initial);
    renderInRouter(<ActiveEnrollment pollMs={20} />, { transport: api.transport });
    await user.click(await screen.findByRole("button", { name: "Generate install command" }));

    expect(await screen.findByRole("status")).toHaveTextContent(
      "This command expired before a worker used it. Generate a new command to add a worker.",
    );
    expect(screen.queryByText(command)).toBeNull();
    expect(screen.queryByRole("region", { name: "Adding the worker" })).toBeNull();
    expect(screen.getByRole("button", { name: "Generate a new command" })).toBeEnabled();
    const calls = api.calls.token;
    await settled();
    expect(api.calls.token).toBe(calls);
  });

  it("stops asking when the page is left", async () => {
    const user = userEvent.setup();
    const api = enrollmentApi();
    const r = renderInRouter(<ActiveEnrollment pollMs={20} />, { transport: api.transport });
    await user.click(await screen.findByRole("button", { name: "Generate install command" }));
    await waitFor(() => expect(api.calls.token).toBeGreaterThan(1));
    r.unmount();
    const calls = api.calls.token;
    await settled();
    expect(api.calls.token).toBe(calls);
  });

  it("makes Continue to Syncr the setup wizard's main button once the worker connects", async () => {
    const user = userEvent.setup();
    const api = enrollmentApi({ token: EnrollmentTokenState.USED, worker: connectedWorker() });
    renderApp("/setup?step=worker", api.transport);
    const next = await screen.findByRole("button", { name: "Continue to Syncr" });
    expect(next).toHaveAttribute("data-variant", "outline");
    await user.click(await screen.findByRole("button", { name: "Generate install command" }));

    const progress = await screen.findByRole("region", { name: "Adding the worker" });
    await waitFor(() => expect(next).toHaveAttribute("data-variant", "default"));
    expect(within(progress).getByRole("link", { name: "Open studio-pc" })).toHaveAttribute(
      "data-variant",
      "outline",
    );
  });
});
