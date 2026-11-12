import { Code, ConnectError } from "@connectrpc/connect";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { JobService, JobStatus, WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import { AuthMethod, AuthService, SetupService } from "@/gen/syncr/v1/auth_pb";
import * as fx from "@/test/fixtures";
import { mockTransport, renderApp } from "@/test/render";

const now = Date.parse("2026-03-12T12:00:00Z");

/** api serves one running job whose WatchJob stream fails with err. */
function api(err: ConnectError) {
  const running = fx.jobs(now).find((j) => j.status === JobStatus.RUNNING)!;
  let calls = 0;
  const transport = mockTransport(({ service }) => {
    service(SetupService, { getSetupStatus: () => ({ setupRequired: false }) });
    service(AuthService, {
      getSession: () => ({ user: fx.users(now)[0], method: AuthMethod.SESSION }),
    });
    service(WorkerAdminService, { listWorkers: () => ({ workers: fx.workers(now) }) });
    service(JobService, {
      getJob: () => ({ job: running }),
      listJobSamples: () => ({ samples: [] }),
      // eslint-disable-next-line require-yield
      watchJob: async function* () {
        calls++;
        await Promise.resolve();
        throw err;
      },
    });
  });
  return { transport, job: running, calls: () => calls };
}

describe("job detail stream state", () => {
  it("shows an error, not Finished, when the stream stops on a permanent error", async () => {
    const user = userEvent.setup();
    const { transport, job, calls } = api(
      new ConnectError("watch not allowed", Code.PermissionDenied),
    );
    renderApp(`/jobs/${job.id}`, transport);

    const alert = await screen.findByText(/Live updates stopped/);
    expect(alert).toHaveTextContent("Watch not allowed");
    expect(screen.queryByText("Finished")).not.toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Retry" }));
    await waitFor(() => expect(calls()).toBe(2));
    expect(await screen.findByText(/Live updates stopped/)).toBeInTheDocument();
    expect(screen.queryByText("Finished")).not.toBeInTheDocument();
  });
});
