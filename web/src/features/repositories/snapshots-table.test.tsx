import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import type { ListSnapshotsRequest } from "@/gen/syncr/v1/restic_pb";
import * as m5 from "@/mock/m5";
import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { renderInRouter } from "@/test/render";

import { SnapshotsTable } from "./snapshots-table";

const now = Date.now();

function show(repoId: string) {
  const api = m5Api(now);
  const view = renderInRouter(
    <SnapshotsTable repository={api.repo(repoId).repo} workers={m5.workers(now)} />,
    { transport: api.transport },
  );
  return { api, user: userEvent.setup(), ...view };
}

const requests = (api: ReturnType<typeof m5Api>) =>
  api.of<ListSnapshotsRequest>("ListSnapshots").map((r) => ({
    refresh: r.refresh,
    workerId: r.workerId,
  }));

describe("SnapshotsTable", () => {
  it("lists a fresh cache without asking a worker", async () => {
    const { api } = show(m5.ids.photosVault);
    const table = await screen.findByRole("table", { name: "Snapshots" });
    await waitFor(() => expect(within(table).getAllByRole("row")).toHaveLength(31));
    expect(within(table).getAllByText("nas-01").length).toBe(30);
    expect(within(table).getAllByText("/srv/photos").length).toBe(30);
    expect(within(table).getAllByText("photos").length).toBe(30);
    expect(screen.queryByText("Out of date")).toBeNull();
    expect(requests(api)).toEqual([{ refresh: false, workerId: "" }]);
  });

  it("refreshes an out-of-date cache once when it opens", async () => {
    const { api } = show(m5.ids.nasDocs);
    await screen.findByRole("table", { name: "Snapshots" });
    await waitFor(() =>
      expect(requests(api).slice(0, 2)).toEqual([
        { refresh: false, workerId: "" },
        { refresh: true, workerId: "" },
      ]),
    );
    expect(await screen.findByText(/by nas-01/)).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByText("Out of date")).toBeNull());
    // Once only: the refreshed list does not trigger another refresh.
    expect(requests(api).filter((r) => r.refresh)).toHaveLength(1);
  });

  it("refreshes on demand through the chosen worker", async () => {
    const { api, user } = show(m5.ids.photosVault);
    await screen.findByRole("table", { name: "Snapshots" });
    await user.click(screen.getByRole("combobox", { name: "Worker that lists the snapshots" }));
    await user.click(await screen.findByRole("option", { name: /backup-host/ }));
    await user.click(screen.getByRole("button", { name: "Refresh" }));
    await waitFor(() =>
      expect(requests(api).filter((r) => r.refresh)).toEqual([
        { refresh: true, workerId: fx.ids.backupHost },
      ]),
    );
    expect(await screen.findByText(/by backup-host/)).toBeInTheDocument();
  });

  it("explains a failed refresh and keeps the last list", async () => {
    const { user } = show(m5.ids.photosVault);
    const table = await screen.findByRole("table", { name: "Snapshots" });
    await user.click(screen.getByRole("combobox", { name: "Worker that lists the snapshots" }));
    await user.click(await screen.findByRole("option", { name: /office-pc/ }));
    await user.click(screen.getByRole("button", { name: "Refresh" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(
      "Couldn't refresh the snapshots: Worker office-pc is offline The list below is the last one taken.",
    );
    expect(within(table).getAllByRole("row")).toHaveLength(31);
  });

  it("cannot refresh while the data is being deleted, but still lists", async () => {
    const { api } = show(m5.ids.oldLaptop);
    const table = await screen.findByRole("table", { name: "Snapshots" });
    await waitFor(() => expect(within(table).getAllByRole("row")).toHaveLength(7));
    expect(screen.getByRole("button", { name: "Refresh" })).toBeDisabled();
    expect(screen.getByText(/cannot be refreshed while the data is being deleted/)).toBeVisible();
    expect(requests(api)).toEqual([{ refresh: false, workerId: "" }]);
  });

  it("opens the snapshot browser from a row", async () => {
    const { api, user, location } = show(m5.ids.photosVault);
    const first = api.repo(m5.ids.photosVault).snapshots[0];
    await user.click(await screen.findByRole("row", { name: `Snapshot ${first.id.slice(0, 8)}` }));
    await waitFor(() =>
      expect(location().pathname).toBe(`/repositories/${m5.ids.photosVault}/snapshots/${first.id}`),
    );
  });
});
