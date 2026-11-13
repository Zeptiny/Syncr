import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import type {
  MeasureRemoteFolderRequest,
  MeasureWorkerFolderRequest,
} from "@/gen/syncr/v1/probe_pb";
import type { MeasureSnapshotFolderRequest } from "@/gen/syncr/v1/restic_pb";
import { BUSY_TEXT } from "@/lib/scans";
import * as m5 from "@/mock/m5";
import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { renderApp } from "@/test/render";

import { invalidatePlaces } from "./refresh";

const now = Date.UTC(2026, 10, 10, 12);
const at = (path: string) => `/workers/${fx.ids.nas}/files?path=${encodeURIComponent(path)}`;
const row = (grid: HTMLElement, name: string) =>
  within(grid)
    .getAllByRole("row")
    .find((r) => within(r).queryByRole("button", { name }) !== null)!;

async function measured(path: string) {
  const user = userEvent.setup();
  const api = m5Api(now);
  const app = renderApp(at(path), api.transport);
  const grid = await screen.findByRole("grid", { name: `Contents of ${path}` });
  await within(grid).findAllByRole("checkbox");
  await user.click(screen.getByRole("button", { name: "Calculate sizes" }));
  return { user, api, grid, ...app };
}

describe("folder sizes", { timeout: 30_000 }, () => {
  it("measures the open folder: its total, each folder's size and share", async () => {
    const { api, grid } = await measured("/srv/photos");
    expect(await screen.findByText(/in 243 files/)).toBeInTheDocument();
    expect(api.of<MeasureWorkerFolderRequest>("MeasureWorkerFolder")).toMatchObject([
      { workerId: fx.ids.nas, path: "/srv/photos" },
    ]);
    const status = screen
      .getAllByRole("status")
      .find((s) => /243 files/.test(s.textContent ?? ""))!;
    expect(status).toHaveTextContent(/^2\.99 GiB in 243 files, measured just now$/);
    const y2025 = row(grid, "2025");
    expect(within(y2025).getByText("1.52 GiB")).toBeInTheDocument();
    expect(y2025).toHaveTextContent(/files, \d+ folders, 51% of this folder/);
    expect(within(row(grid, "2026")).getByText("1.06 GiB")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Measure again" })).toBeEnabled();
  });

  it("says when a walk stopped early and what it could not read", async () => {
    const { grid } = await measured("/mnt");
    const status = await screen.findByText(/Stopped after 41 s/);
    expect(status.closest("[role=status]")).toHaveTextContent(
      "At least 2.70 GiB in 2 files, measured just now. Stopped after 41 s; 1 entry could not be read.",
    );
    expect(row(grid, "media")).toHaveTextContent(
      /^media≥ at least 2\.70 GiB, 2 files, 1 folder, 100%/,
    );
  });

  it("counts an unreadable entry as a lower bound of the folder", async () => {
    await measured("/srv");
    expect(await screen.findByText(/1 entry could not be read/)).toBeInTheDocument();
    expect(screen.getByText("At least 3.13 GiB", { exact: false })).toBeInTheDocument();
  });

  it("shows the busy worker in its own words", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    api.busy.add(fx.ids.nas);
    renderApp(at("/srv/photos"), api.transport);
    await screen.findByRole("button", { name: "2025" });
    await user.click(screen.getByRole("button", { name: "Calculate sizes" }));
    expect(await screen.findByRole("alert")).toHaveTextContent(BUSY_TEXT);
    api.busy.clear();
    await user.click(screen.getByRole("button", { name: "Calculate sizes" }));
    expect(await screen.findByText(/in 243 files/)).toBeInTheDocument();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("keeps sizes across folders until Refresh, and a job's end drops them", async () => {
    const { user, api, router, queryClient } = await measured("/srv/photos");
    await screen.findByText(/in 243 files/);
    // Into 2026 and back: the measurement is still there, not asked again.
    await user.click(screen.getByRole("button", { name: "2026" }));
    await screen.findByRole("grid", { name: "Contents of /srv/photos/2026" });
    expect(screen.getByRole("button", { name: "Calculate sizes" })).toBeInTheDocument();
    act(() => router.history.back());
    const grid = await screen.findByRole("grid", { name: "Contents of /srv/photos" });
    expect(await within(row(grid, "2025")).findByText("1.52 GiB")).toBeInTheDocument();
    expect(api.of("MeasureWorkerFolder")).toHaveLength(1);

    await user.click(screen.getByRole("button", { name: "Refresh" }));
    expect(await screen.findByRole("button", { name: "Calculate sizes" })).toBeInTheDocument();
    expect(within(row(grid, "2025")).queryByText("1.52 GiB")).toBeNull();
    expect(api.of("MeasureWorkerFolder")).toHaveLength(1);

    await user.click(screen.getByRole("button", { name: "Calculate sizes" }));
    expect(await within(row(grid, "2025")).findByText("1.52 GiB")).toBeInTheDocument();
    await act(() => invalidatePlaces(queryClient, [{ kind: "worker", workerId: fx.ids.nas }]));
    await waitFor(() => expect(within(row(grid, "2025")).queryByText("1.52 GiB")).toBeNull());
    expect(api.of("MeasureWorkerFolder")).toHaveLength(2);
  });

  it("totals the selection with measured sizes, at least while a folder is unmeasured", async () => {
    const user = userEvent.setup();
    renderApp(at("/srv/photos"), m5Api(now).transport);
    const grid = await screen.findByRole("grid", { name: "Contents of /srv/photos" });
    await user.click(await within(grid).findByRole("checkbox", { name: "Select 2025" }));
    await user.click(within(grid).getByRole("checkbox", { name: "Select README.txt" }));
    const tray = screen.getByRole("region", { name: "Selection" });
    expect(within(tray).getByText("at least 1.80 KiB")).toBeInTheDocument();
    // An unmeasured folder shows no size beside its remove button.
    const item = within(tray).getByRole("button", { name: "Remove 2025" }).closest("li")!;
    expect(item).not.toHaveTextContent("–");
    expect(item).toHaveTextContent(/^2025$/);

    await user.click(screen.getByRole("button", { name: "Calculate sizes" }));
    await waitFor(() => expect(item).toHaveTextContent(/^20251\.52 GiB$/));
    expect(within(tray).getByText("1.52 GiB", { selector: "header span" })).toBeInTheDocument();
  });

  it("measures a remote's folder through the listing worker", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    renderApp(`/remotes/${fx.ids.minio}/files`, api.transport);
    const grid = await screen.findByRole("grid", { name: "Contents of MinIO archive" });
    await within(grid).findByRole("button", { name: "archive" });
    await user.click(screen.getByRole("button", { name: "Calculate sizes" }));
    expect(await within(row(grid, "archive")).findByText("6.00 GiB")).toBeInTheDocument();
    expect(api.of<MeasureRemoteFolderRequest>("MeasureRemoteFolder")).toMatchObject([
      { remoteId: fx.ids.minio, path: "", workerId: "" },
    ]);
  });

  it("measures a snapshot's folder", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    const snap = api.repo(m5.ids.photosVault).snapshots[0];
    renderApp(
      `/repositories/${m5.ids.photosVault}/snapshots/${snap.id}?path=%2Fsrv%2Fphotos`,
      api.transport,
    );
    const grid = await screen.findByRole("grid", { name: "Contents of /srv/photos" });
    await within(grid).findByRole("button", { name: "2026" });
    await user.click(screen.getByRole("button", { name: "Calculate sizes" }));
    expect(await within(row(grid, "2025")).findByText("1.52 GiB")).toBeInTheDocument();
    expect(api.of<MeasureSnapshotFolderRequest>("MeasureSnapshotFolder")).toMatchObject([
      { repositoryId: m5.ids.photosVault, snapshotId: snap.id, path: "/srv/photos" },
    ]);
  });

  it("offers no measuring of a worker's list of allowed folders", async () => {
    renderApp(`/workers/${fx.ids.nas}/files`, m5Api(now).transport);
    await screen.findByRole("button", { name: "/srv" });
    expect(screen.queryByRole("button", { name: "Calculate sizes" })).toBeNull();
  });
});

describe("a folder that isn't there", () => {
  it("is explained in the words of the place", async () => {
    renderApp(at("/srv/gone"), m5Api(now).transport);
    expect(
      await screen.findByText(/moved or deleted on the worker, or the worker can't read it/),
    ).toBeInTheDocument();
    expect(screen.queryByText(/snapshot/)).toBeNull();
  });

  it("is one the snapshot may not have", async () => {
    const api = m5Api(now);
    const snap = api.repo(m5.ids.photosVault).snapshots[0];
    renderApp(
      `/repositories/${m5.ids.photosVault}/snapshots/${snap.id}?path=%2Fsrv%2Fgone`,
      api.transport,
    );
    expect(await screen.findByText(/This snapshot does not have it/)).toBeInTheDocument();
  });
});
