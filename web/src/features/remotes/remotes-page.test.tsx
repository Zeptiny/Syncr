import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { renderApp } from "@/test/render";

describe("help links on the remote pages", () => {
  it("opens the section of the chosen type, in a new tab", async () => {
    const api = m5Api(Date.now());
    const picker = renderApp("/remotes/new", api.transport);
    expect(await screen.findByRole("link", { name: /^Help: Remotes/ })).toHaveAttribute(
      "href",
      "/docs/remotes",
    );
    picker.unmount();

    const smb = renderApp("/remotes/new?type=smb", api.transport);
    // The picker shows until the types are loaded.
    expect(await screen.findByRole("heading", { name: /^New SMB/ })).toBeInTheDocument();
    const help = screen.getByRole("link", { name: /^Help: Remotes/ });
    expect(help).toHaveAttribute("href", "/docs/remotes#smb-windows-share");
    expect(help).toHaveAttribute("target", "_blank");
    smb.unmount();

    renderApp(`/remotes/${fx.ids.vault}`, api.transport);
    expect(await screen.findByRole("link", { name: /^Help: Encrypted remotes/ })).toHaveAttribute(
      "href",
      "/docs/encrypted-remotes",
    );
  });
});

describe("RemotesPage", () => {
  it("opens a remote's browser page, which lists files as well as folders", async () => {
    const user = userEvent.setup();
    const api = m5Api(Date.now());
    const { router } = renderApp("/remotes", api.transport);

    await user.click(await screen.findByRole("button", { name: "Actions for Storage box" }));
    await user.click(await screen.findByRole("menuitem", { name: "Browse" }));
    await waitFor(() =>
      expect(router.state.location.pathname).toBe(`/remotes/${fx.ids.storageBox}/files`),
    );
    await user.click(await screen.findByRole("button", { name: "db-dumps" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ path: "db-dumps" }));

    // Files are listed, not only folders.
    expect(await screen.findByText("dump-2026-11-09.sql.gz")).toBeInTheDocument();
    await waitFor(() =>
      expect(api.of("ListRemoteFiles").at(-1)).toMatchObject({
        remoteId: fx.ids.storageBox,
        path: "db-dumps",
        dirsOnly: false,
      }),
    );
  });

  it("shows where an encrypted remote stores its data, and who uses a remote", async () => {
    const api = m5Api(Date.now());
    renderApp("/remotes", api.transport);

    const vault = await screen.findByRole("row", { name: /Vault/ });
    expect(within(vault).getByText("in")).toBeInTheDocument();
    expect(within(vault).getByText("B2 offsite")).toBeInTheDocument();
    expect(within(vault).getByText("/ vault")).toBeInTheDocument();

    // B2 offsite holds Vault's data; unions still list as before.
    const b2 = screen.getByRole("row", { name: /B2 offsite/ });
    expect(within(b2).getByText("Vault")).toBeInTheDocument();
    const photos = screen.getByRole("row", { name: /^Photos/ });
    expect(within(photos).getByText("Pooled disks")).toBeInTheDocument();
  });

  it("refuses to delete a remote that stores another remote's data, naming it", async () => {
    const user = userEvent.setup();
    const api = m5Api(Date.now());
    renderApp(`/remotes/${fx.ids.b2}`, api.transport);

    await user.click(await screen.findByRole("button", { name: "Delete remote" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(dialog).toHaveTextContent(
      "Vault stores its data in this remote. Delete that remote first: a remote that holds another remote's data cannot be deleted.",
    );
    // Every reason is named, the repository it also holds too.
    expect(dialog).toHaveTextContent(/is a repository on this remote/);
    await waitFor(() =>
      expect(within(dialog).getByRole("button", { name: "Delete remote" })).toBeDisabled(),
    );
    expect(api.of("DeleteRemote")).toHaveLength(0);
  });

  it("warns that deleting an encrypted remote erases the only copy of its password", async () => {
    const user = userEvent.setup();
    const api = m5Api(Date.now());
    renderApp(`/remotes/${fx.ids.vault}`, api.transport);

    await user.click(await screen.findByRole("button", { name: "Delete remote" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(dialog).toHaveTextContent(
      "Its password and salt (second password) are erased too, and Syncr keeps no other copy. The files it stores in B2 offsite / vault are not deleted, but without them nobody can read them.",
    );
    // The usual note stays; the delete is allowed.
    expect(dialog).toHaveTextContent(/Its saved credentials are erased/);
    await waitFor(() =>
      expect(within(dialog).getByRole("button", { name: "Delete remote" })).toBeEnabled(),
    );
  });

  it("adds no such warning for a remote without fixed secrets", async () => {
    const user = userEvent.setup();
    const api = m5Api(Date.now());
    renderApp(`/remotes/${fx.ids.nextcloud}`, api.transport);

    await user.click(await screen.findByRole("button", { name: "Delete remote" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(dialog).toHaveTextContent(/Its saved credentials are erased/);
    expect(dialog).not.toHaveTextContent(/Syncr keeps no other copy/);
  });
});
