import { screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import * as m5 from "@/mock/m5";
import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { renderWithProviders } from "@/test/render";

import { TestRemoteButton, type TestRemoteButtonProps } from "../test-remote-button";

const now = Date.UTC(2026, 10, 10, 12);

function show(props: TestRemoteButtonProps) {
  const api = m5Api(now);
  renderWithProviders(<TestRemoteButton {...props} />, { transport: api.transport });
  return { api, user: userEvent.setup() };
}

const press = (user: ReturnType<typeof userEvent.setup>) =>
  user.click(screen.getByRole("button", { name: "Test remote" }));

describe("TestRemoteButton", () => {
  it("shows the worker, the latency, what it saw and the space used", async () => {
    const { user, api } = show({ remoteId: fx.ids.b2 });
    await press(user);
    const ok = await screen.findByTestId("test-remote-ok");
    expect(ok).toHaveTextContent("The remote answered through nas-01 in 412 ms");
    expect(ok).toHaveTextContent("2 items at the top: restic, photos");
    const bar = screen.getByRole("progressbar", { name: "Space used" });
    expect(bar).toHaveAttribute("aria-valuenow", "31");
    expect(ok).toHaveTextContent("319 GiB of 1.00 TiB (31%)");
    expect(api.of("TestRemote")).toEqual([
      expect.objectContaining({ remoteId: fx.ids.b2, workerId: "", path: "" }),
    ]);
  });

  it("says so when the storage reports no usage", async () => {
    const { user } = show({ remoteId: fx.ids.minio });
    await press(user);
    const ok = await screen.findByTestId("test-remote-ok");
    expect(ok).toHaveTextContent("does not report how much space is used");
    expect(screen.queryByRole("progressbar")).not.toBeInTheDocument();
  });

  it("shows a failed test with its error and code", async () => {
    const { user } = show({ remoteId: fx.ids.nextcloud });
    await press(user);
    const failed = await screen.findByTestId("test-remote-failed");
    expect(failed).toHaveTextContent("The test failed: execution_failed");
    expect(failed).toHaveTextContent("connection refused");
    expect(failed).toHaveTextContent("Tested through nas-01");
  });

  it("tests the unsaved form values, and never shows a typed secret back", async () => {
    const input = () => ({
      name: "New bucket",
      backendType: "s3",
      options: { endpoint: "https://s3.example.net" },
      secrets: { secret_access_key: "s3cr3t-value" },
    });
    const { user, api } = show({ input });
    await press(user);
    await screen.findByTestId("test-remote-ok");
    expect(api.of("TestRemote")[0]).toMatchObject({
      remoteId: "",
      input: { name: "New bucket", secrets: { secret_access_key: "s3cr3t-value" } },
    });
    expect(document.body).not.toHaveTextContent("s3cr3t-value");
  });

  it("asks for a worker when the remote is a folder on a worker's disk", async () => {
    const workers = m5.workers(now);
    const { user, api } = show({ remoteId: fx.ids.photos, workers });
    await press(user);
    const error = await screen.findByTestId("test-remote-error");
    expect(error).toHaveTextContent("choose the worker whose disk holds this remote");
    // The picker appears; with a worker the test runs.
    await user.click(screen.getByRole("combobox"));
    await user.click(await screen.findByRole("option", { name: /nas-01/ }));
    await press(user);
    expect(await screen.findByTestId("test-remote-ok")).toHaveTextContent("through nas-01");
    expect(api.of("TestRemote").at(-1)).toMatchObject({ workerId: fx.ids.nas });
  });

  it("reports an unavailable worker from the response, not as a crash", async () => {
    const workers = m5.workers(now);
    const { user } = show({ remoteId: fx.ids.b2, workers, needsWorker: true });
    await user.click(screen.getByRole("combobox"));
    await user.click(await screen.findByRole("option", { name: /office-pc/ }));
    await press(user);
    const failed = await screen.findByTestId("test-remote-failed");
    expect(failed).toHaveTextContent("The test failed: Worker unavailable");
    expect(failed).toHaveTextContent("worker office-pc is offline");
  });
});
