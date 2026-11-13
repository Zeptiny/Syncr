import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import type { GetRemoteUsageRequest, ListRemoteFilesRequest } from "@/gen/syncr/v1/probe_pb";
import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { renderApp } from "@/test/render";

import { validateRemoteFilesSearch, validateWorkerFilesSearch } from "./search";

const now = Date.UTC(2026, 10, 10, 12);

const rowsOf = (grid: HTMLElement) => within(grid).getAllByRole("row").slice(1);

describe("files search params", () => {
  it("keeps paths and the worker, nothing else", () => {
    expect(validateWorkerFilesSearch({ path: " /srv/docs ", x: 1 })).toEqual({ path: "/srv/docs" });
    expect(validateWorkerFilesSearch({ path: "" })).toEqual({ path: undefined });
    expect(validateRemoteFilesSearch({ path: "/restic/", worker: "w1" })).toEqual({
      path: "restic",
      worker: "w1",
    });
    expect(validateRemoteFilesSearch({ path: 3 })).toEqual({ path: undefined, worker: undefined });
  });
});

describe("WorkerFilesPage", () => {
  it("browses with the keyboard, keeps the selection across folders and copies the paths", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    const { router } = renderApp(`/workers/${fx.ids.nas}/files`, api.transport);
    expect(await screen.findByRole("heading", { name: "Files on nas-01" })).toBeInTheDocument();
    const top = await screen.findByRole("grid", { name: "Contents of Allowed folders" });
    await within(top).findByRole("button", { name: "/srv" });
    expect(screen.getByText(/^Listed/)).toBeInTheDocument();

    // Keyboard only: into /srv, then docs.
    act(() => rowsOf(top)[1].focus()); // /srv after /mnt
    await user.keyboard("{Enter}");
    await waitFor(() => expect(router.state.location.search).toEqual({ path: "/srv" }));
    const srv = await screen.findByRole("grid", { name: "Contents of /srv" });
    await within(srv).findByRole("button", { name: "docs" });
    await waitFor(() => expect(rowsOf(srv)[0]).toHaveFocus());
    // backup, docs, photos, shared
    await user.keyboard("{ArrowDown}{Enter}");
    const docs = await screen.findByRole("grid", { name: "Contents of /srv/docs" });
    await within(docs).findByText("notes.md");
    await waitFor(() => expect(rowsOf(docs)[0]).toHaveFocus());
    // contracts, invoices, taxes, notes.md, passwords.kdbx: tick the two files.
    await user.keyboard("{End} {ArrowUp}{Shift>} {/Shift}");
    const tray = screen.getByRole("region", { name: "Selection" });
    expect(within(tray).getByRole("heading", { name: "2 selected" })).toBeInTheDocument();
    expect(within(tray).getByText("78.5 KiB")).toBeInTheDocument();

    // Back up and into photos: the selection stays.
    await user.keyboard("{Backspace}");
    await waitFor(() => expect(router.state.location.search).toEqual({ path: "/srv" }));
    // The folder just left keeps the focus.
    const again = await screen.findByRole("grid", { name: "Contents of /srv" });
    await waitFor(() => expect(document.activeElement?.textContent).toContain("docs"));
    await user.click(within(again).getByRole("checkbox", { name: "Select photos" }));
    expect(within(tray).getByRole("heading", { name: "3 selected" })).toBeInTheDocument();
    expect(within(tray).getByText("at least 78.5 KiB")).toBeInTheDocument();

    await user.click(within(tray).getByRole("button", { name: "Copy paths" }));
    expect(await navigator.clipboard.readText()).toBe(
      "/srv/docs/notes.md\n/srv/docs/passwords.kdbx\n/srv/photos",
    );
    await user.click(within(tray).getByRole("button", { name: "Remove notes.md" }));
    expect(within(tray).getByRole("heading", { name: "2 selected" })).toBeInTheDocument();
    await user.click(within(tray).getByRole("button", { name: "Clear" }));
    expect(screen.queryByRole("region", { name: "Selection" })).toBeNull();
  });

  it("goes from a folder that isn't there to its nearest parent", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    const { router } = renderApp(
      `/workers/${fx.ids.nas}/files?path=${encodeURIComponent("/srv/docs/gone/deeper")}`,
      api.transport,
    );
    expect(await screen.findByText("This folder isn't here")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Go to the nearest parent" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ path: "/srv/docs" }));
    expect(await screen.findByText("notes.md")).toBeInTheDocument();
  });

  it("is linked from the worker's page", async () => {
    renderApp(`/workers/${fx.ids.nas}`, m5Api(now).transport);
    expect(await screen.findByRole("link", { name: "Browse files" })).toHaveAttribute(
      "href",
      `/workers/${fx.ids.nas}/files`,
    );
  });
});

describe("RemoteFilesPage", () => {
  it("shows the usage at the root and lists through the chosen worker", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    const { router } = renderApp(`/remotes/${fx.ids.b2}/files`, api.transport);
    expect(await screen.findByRole("progressbar", { name: "Used" })).toBeInTheDocument();
    expect(screen.getByText(/of 1\.00 TiB/)).toBeInTheDocument();
    await screen.findByRole("button", { name: "restic" });

    await user.click(screen.getByRole("button", { name: /^Listing worker: nas-01/ }));
    await user.click(await screen.findByRole("menuitemradio", { name: "backup-host" }));
    await waitFor(() =>
      expect(router.state.location.search).toEqual({ worker: fx.ids.backupHost }),
    );
    await waitFor(() =>
      expect(api.of<ListRemoteFilesRequest>("ListRemoteFiles").at(-1)).toMatchObject({
        remoteId: fx.ids.b2,
        workerId: fx.ids.backupHost,
        path: "",
      }),
    );
    // Opening a folder keeps the worker; the usage belongs to the root only.
    await user.click(await screen.findByRole("button", { name: "photos" }));
    await waitFor(() =>
      expect(router.state.location.search).toEqual({ path: "photos", worker: fx.ids.backupHost }),
    );
    await waitFor(() => expect(screen.queryByRole("progressbar", { name: "Used" })).toBeNull());
  });

  it("hides the usage when the backend cannot tell", async () => {
    const api = m5Api(now);
    renderApp(`/remotes/${fx.ids.minio}/files`, api.transport);
    await screen.findByRole("button", { name: "restic" });
    await waitFor(() => expect(api.of("GetRemoteUsage")).toHaveLength(1));
    expect(screen.queryByRole("progressbar", { name: "Used" })).toBeNull();
  });

  it("waits for the worker whose disk holds a local remote", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    renderApp(`/remotes/${fx.ids.photos}/files`, api.transport);
    expect(await screen.findByText(/Choose the worker whose disk holds this remote/)).toBeVisible();
    expect(api.of("ListRemoteFiles")).toHaveLength(0);
    expect(api.of("GetRemoteUsage")).toHaveLength(0);

    await user.click(screen.getByRole("button", { name: /^Listing worker: none chosen/ }));
    const menu = await screen.findByRole("menu");
    // No automatic choice; old-nas cannot list remotes.
    expect(within(menu).queryByRole("menuitemradio", { name: "Automatic" })).toBeNull();
    expect(within(menu).queryByRole("menuitemradio", { name: "old-nas" })).toBeNull();
    await user.click(within(menu).getByRole("menuitemradio", { name: "nas-01" }));
    expect(await screen.findByRole("button", { name: "2026" })).toBeInTheDocument();
    await waitFor(() =>
      expect(api.of<GetRemoteUsageRequest>("GetRemoteUsage").at(-1)).toMatchObject({
        remoteId: fx.ids.photos,
        workerId: fx.ids.nas,
      }),
    );
    expect(screen.queryByRole("progressbar", { name: "Used" })).toBeNull();
  });

  it("is linked from the remote's edit page", async () => {
    renderApp(`/remotes/${fx.ids.b2}`, m5Api(now).transport);
    expect(await screen.findByRole("link", { name: "Browse" })).toHaveAttribute(
      "href",
      `/remotes/${fx.ids.b2}/files`,
    );
  });
});
