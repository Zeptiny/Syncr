import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import type { RunJobRequest } from "@/gen/syncr/v1/api_pb";
import type { BrowseSnapshotRequest } from "@/gen/syncr/v1/restic_pb";
import * as m5 from "@/mock/m5";
import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { renderApp } from "@/test/render";

const now = Date.UTC(2026, 10, 10, 12);

function open(index: number, path = "/srv/photos") {
  const api = m5Api(now);
  const snaps = api.repo(m5.ids.photosVault).snapshots;
  // The second snapshot comes from another host and paths.
  snaps[1].host = "laptop";
  snaps[1].paths = ["/home/ada"];
  const view = renderApp(
    `/repositories/${m5.ids.photosVault}/snapshots/${snaps[index].id}?path=${encodeURIComponent(path)}`,
    api.transport,
  );
  return { api, snaps, user: userEvent.setup(), ...view };
}

describe("snapshot browser", () => {
  it("switches to a newer snapshot of the same host and paths, keeping the folder", async () => {
    const { snaps, user, router } = open(2);
    await screen.findByRole("grid", { name: "Contents of /srv/photos" });
    // The laptop's snapshot in between is skipped.
    const newer = await screen.findByRole("button", { name: /^Newer snapshot, / });
    await user.click(newer);
    await waitFor(() =>
      expect(router.state.location.pathname).toBe(
        `/repositories/${m5.ids.photosVault}/snapshots/${snaps[0].id}`,
      ),
    );
    expect(router.state.location.search).toEqual({ path: "/srv/photos" });
    expect(screen.getByRole("button", { name: "Newer snapshot" })).toBeDisabled();

    await user.click(screen.getByRole("button", { name: "Choose a snapshot" }));
    const menu = await screen.findByRole("menu");
    const labels = within(menu)
      .getAllByText(/^(Same host and paths|Other snapshots)$/)
      .map((e) => e.textContent);
    expect(labels).toEqual(["Same host and paths", "Other snapshots"]);
    expect(within(menu).getAllByRole("menuitemradio")).toHaveLength(30);
    await user.click(within(menu).getByRole("menuitemradio", { name: /laptop/ }));
    await waitFor(() =>
      expect(router.state.location.pathname).toBe(
        `/repositories/${m5.ids.photosVault}/snapshots/${snaps[1].id}`,
      ),
    );
    expect(router.state.location.search).toEqual({ path: "/srv/photos" });
  });

  it("lists through the chosen worker and keeps it in the URL", async () => {
    const { api, user, router } = open(0);
    await screen.findByRole("grid", { name: "Contents of /srv/photos" });
    await user.click(await screen.findByRole("button", { name: /^Listing worker: nas-01/ }));
    await user.click(await screen.findByRole("menuitemradio", { name: "backup-host" }));
    await waitFor(() =>
      expect(router.state.location.search).toEqual({
        path: "/srv/photos",
        worker: fx.ids.backupHost,
      }),
    );
    await waitFor(() =>
      expect(api.of<BrowseSnapshotRequest>("BrowseSnapshot").at(-1)).toMatchObject({
        workerId: fx.ids.backupHost,
        path: "/srv/photos",
      }),
    );
  });

  it("shows a folder the snapshot lacks and climbs to the nearest parent", async () => {
    const { user, router } = open(0, "/srv/photos/1999/12");
    expect(await screen.findByText("This folder isn't here")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Go to the nearest parent" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ path: "/srv/photos" }));
    expect(await screen.findByRole("button", { name: "2026" })).toBeInTheDocument();
  });

  it("restores what the tray holds, leaving items inside a selected folder to it", async () => {
    const { api, user } = open(0);
    const grid = await screen.findByRole("grid", { name: "Contents of /srv/photos" });
    await user.click(await within(grid).findByRole("checkbox", { name: "Select README.txt" }));
    await user.click(within(grid).getByRole("button", { name: "2026" }));
    const inner = await screen.findByRole("grid", { name: "Contents of /srv/photos/2026" });
    await user.click(await within(inner).findByRole("checkbox", { name: "Select 01" }));
    await user.click(screen.getByRole("button", { name: "Up one folder" }));
    const back = await screen.findByRole("grid", { name: "Contents of /srv/photos" });
    await user.click(await within(back).findByRole("checkbox", { name: "Select 2026" }));
    const tray = screen.getByRole("region", { name: "Selection" });
    expect(within(tray).getByText("in 2026")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Restore selected (3)" }));
    const sheet = await screen.findByRole("dialog");
    await user.click(within(sheet).getByRole("button", { name: "Start restore" }));
    await waitFor(() => expect(api.of("RunJob")).toHaveLength(1));
    expect(api.of<RunJobRequest>("RunJob")[0].definition?.operation?.value).toMatchObject({
      paths: ["/srv/photos/README.txt", "/srv/photos/2026"],
    });
  });

  it("restores a row from its menu, also back to its original location", async () => {
    const { api, user } = open(0);
    const grid = await screen.findByRole("grid", { name: "Contents of /srv/photos" });
    await user.click(await within(grid).findByRole("button", { name: "Actions for README.txt" }));
    const menu = await screen.findByRole("menu");
    expect(
      within(menu)
        .getAllByRole("menuitem")
        .map((m) => m.textContent),
    ).toEqual(["Restore…", "Restore to original location…", "Copy path"]);
    await user.click(within(menu).getByRole("menuitem", { name: "Restore to original location…" }));
    const sheet = await screen.findByRole("dialog", { name: /^Restore from snapshot/ });
    expect(within(sheet).getByText("1 item")).toBeInTheDocument();
    expect(within(sheet).getByLabelText("Target folder")).toHaveValue("/srv/photos");
    await user.click(within(sheet).getByRole("button", { name: "Start restore" }));
    await waitFor(() => expect(api.of("RunJob")).toHaveLength(1));
    expect(api.of<RunJobRequest>("RunJob")[0].definition?.operation?.value).toMatchObject({
      subfolder: "/srv/photos",
      paths: ["/srv/photos/README.txt"],
      excludes: [],
    });
  });

  it("restores a row as an item", async () => {
    const { user } = open(0);
    const grid = await screen.findByRole("grid", { name: "Contents of /srv/photos" });
    await user.click(await within(grid).findByRole("button", { name: "Actions for 2026" }));
    await user.click(await screen.findByRole("menuitem", { name: "Restore…" }));
    const sheet = await screen.findByRole("dialog", { name: /^Restore from snapshot/ });
    expect(within(sheet).getByText("/srv/photos/2026")).toBeInTheDocument();
    expect(
      within(sheet).getByText(/Files keep their original path under the target/),
    ).toBeInTheDocument();
  });
});
