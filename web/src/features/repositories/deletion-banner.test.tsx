import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { JobStatus } from "@/gen/syncr/v1/api_pb";
import * as m5 from "@/mock/m5";
import { m5Api } from "@/test/m5-api";
import { renderInRouter } from "@/test/render";

import { DeletionBanner } from "./deletion-banner";

const now = Date.UTC(2026, 10, 10, 12);

function show(repoId: string, props: { waitingFor?: number } = {}) {
  const api = m5Api(now);
  const onTryAgain = vi.fn();
  const onForget = vi.fn();
  const state = api.repo(repoId);
  const view = renderInRouter(
    <DeletionBanner
      repository={state.repo}
      workers={m5.workers(now)}
      waitingFor={props.waitingFor}
      pausedSchedules={api.data.schedules.filter(
        (s) => s.repository?.id === repoId && s.pausedReason === "repository_deleting",
      )}
      onTryAgain={onTryAgain}
      onForget={onForget}
    />,
    { transport: api.transport },
  );
  return { api, state, onTryAgain, onForget, user: userEvent.setup(), ...view };
}

describe("DeletionBanner", () => {
  it("renders nothing for a repository that is not being deleted", async () => {
    show(m5.ids.photosVault);
    await waitFor(() => expect(document.body.firstElementChild).not.toBeNull());
    expect(screen.queryByTestId("deletion-banner")).toBeNull();
    expect(screen.queryByTestId("deletion-failed")).toBeNull();
  });

  it("while deleting: says where and what it waits for, follows the job, offers only view and cancel", async () => {
    const { api, user } = show(m5.ids.oldLaptop, { waitingFor: 1 });
    const banner = await screen.findByTestId("deletion-banner");
    await waitFor(() =>
      expect(banner).toHaveTextContent(
        "Deleting the backup data on nas-01 — waiting for 1 job on this repository first",
      ),
    );
    // The job's live status.
    expect(within(banner).getByText("Queued")).toBeInTheDocument();
    expect(within(banner).getByRole("link", { name: "View job" })).toHaveAttribute(
      "href",
      `/jobs/${m5.ids.oldLaptopDelete}`,
    );
    expect(
      within(banner)
        .getAllByRole("button")
        .map((b) => b.textContent),
    ).toEqual(["Cancel deletion"]);
    expect(screen.queryByRole("button", { name: "Try again" })).toBeNull();

    // Cancelling asks first, then cancels the deletion job.
    await user.click(within(banner).getByRole("button", { name: "Cancel deletion" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(dialog).toHaveTextContent("The deletion has not started, so nothing was removed.");
    expect(api.of("CancelJob")).toHaveLength(0);
    await user.click(within(dialog).getByRole("button", { name: "Cancel deletion" }));
    await waitFor(() =>
      expect(api.of("CancelJob")).toEqual([
        expect.objectContaining({ jobId: m5.ids.oldLaptopDelete }),
      ]),
    );
    expect(api.jobs.find((j) => j.id === m5.ids.oldLaptopDelete)?.status).toBe(JobStatus.CANCELLED);
  });

  it("counts several jobs ahead", async () => {
    show(m5.ids.oldLaptop, { waitingFor: 3 });
    const banner = await screen.findByTestId("deletion-banner");
    await waitFor(() =>
      expect(banner).toHaveTextContent("waiting for 3 jobs on this repository first"),
    );
  });

  it("after a failure: shows the error, the paused schedules and both ways on", async () => {
    const { user, onTryAgain, onForget } = show(m5.ids.archive2019);
    const alert = await screen.findByTestId("deletion-failed");
    expect(alert).toHaveTextContent(
      "Deleting the data failed: repository_locked: another restic client is using the repository; nothing was deleted",
    );
    expect(alert).toHaveTextContent("Its schedule remains paused: Archive 2019 yearly check.");
    expect(within(alert).getByRole("link", { name: "Archive 2019 yearly check" })).toHaveAttribute(
      "href",
      `/schedules/${m5.ids.archiveCheck}`,
    );
    expect(screen.queryByRole("button", { name: "Cancel deletion" })).toBeNull();

    await user.click(within(alert).getByRole("button", { name: "Try again" }));
    expect(onTryAgain).toHaveBeenCalledTimes(1);
    expect(onForget).not.toHaveBeenCalled();
    await user.click(within(alert).getByRole("button", { name: "Forget only" }));
    expect(onForget).toHaveBeenCalledTimes(1);
  });

  it("after a cancellation: warns that files may already be gone", async () => {
    const api = m5Api(now);
    const state = api.repo(m5.ids.archive2019);
    state.repo.deletionError =
      "the deletion was cancelled; some repository files may already be gone";
    renderInRouter(
      <DeletionBanner
        repository={state.repo}
        workers={[]}
        onTryAgain={() => {}}
        onForget={() => {}}
      />,
      { transport: api.transport },
    );
    const alert = await screen.findByTestId("deletion-failed");
    expect(alert).toHaveTextContent("The deletion was cancelled");
    expect(alert).toHaveTextContent("Some repository files may already be gone.");
    expect(within(alert).getByRole("button", { name: "Try again" })).toBeEnabled();
    expect(within(alert).getByRole("button", { name: "Forget only" })).toBeEnabled();
  });
});
