import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { renderWithProviders } from "@/test/render";

import { PathBrowserDialog, type PathBrowserDialogProps } from "../path-browser";

const now = Date.UTC(2026, 10, 10, 12);

function open(props: Partial<PathBrowserDialogProps> & Pick<PathBrowserDialogProps, "source">) {
  const api = m5Api(now);
  const onSelect = vi.fn();
  const onOpenChange = vi.fn();
  renderWithProviders(
    <PathBrowserDialog
      open
      onOpenChange={onOpenChange}
      title="Browse"
      mode="dir"
      onSelect={onSelect}
      {...props}
    />,
    { transport: api.transport },
  );
  return { api, onSelect, onOpenChange, user: userEvent.setup() };
}

const row = (name: string) => screen.getByRole("button", { name });

describe("PathBrowserDialog", () => {
  it("lists a worker's allowed folders, opens folders and goes back up", async () => {
    const { user, api, onSelect, onOpenChange } = open({
      source: { kind: "worker", workerId: fx.ids.nas },
    });
    // The top is the list of allowed folders; it cannot itself be chosen.
    expect(await screen.findByRole("button", { name: "/srv" })).toBeInTheDocument();
    expect(row("/mnt")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Select" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Up one folder" })).toBeDisabled();

    await user.click(row("/srv"));
    await user.click(await screen.findByRole("button", { name: "photos" }));
    expect(await screen.findByRole("button", { name: "2026" })).toBeInTheDocument();
    expect(screen.getByLabelText("Path")).toHaveValue("/srv/photos");
    // Folder mode asks the worker for folders only.
    expect(screen.queryByText("catalog.lrcat")).not.toBeInTheDocument();
    expect(api.of("ListWorkerFiles").at(-1)).toMatchObject({
      workerId: fx.ids.nas,
      path: "/srv/photos",
      dirsOnly: true,
    });

    await user.click(screen.getByRole("button", { name: "Up one folder" }));
    expect(await screen.findByRole("button", { name: "docs" })).toBeInTheDocument();
    // One more step up from an allowed folder is their list again.
    await user.click(screen.getByRole("button", { name: "Up one folder" }));
    expect(await screen.findByRole("button", { name: "/mnt" })).toBeInTheDocument();

    await user.click(row("/mnt"));
    await screen.findByRole("button", { name: "media" });
    await user.click(screen.getByRole("button", { name: "Select /mnt" }));
    expect(onSelect).toHaveBeenCalledWith(["/mnt"]);
    expect(onOpenChange).toHaveBeenCalledWith(false);
  });

  it("jumps to a typed path on Enter and shows the worker's refusal", async () => {
    const { user } = open({ source: { kind: "worker", workerId: fx.ids.nas } });
    await screen.findByRole("button", { name: "/srv" });
    const path = screen.getByLabelText("Path");
    await user.clear(path);
    await user.type(path, "/srv/docs/{Enter}");
    expect(await screen.findByRole("button", { name: "invoices" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Select /srv/docs" })).toBeEnabled();

    await user.clear(path);
    await user.type(path, "/etc{Enter}");
    expect(await screen.findByRole("alert")).toHaveTextContent("Couldn't list this folder");
  });

  it("browses a remote from its root and returns the folder that is open", async () => {
    const { user, api, onSelect } = open({
      source: { kind: "remote", remoteId: fx.ids.minio },
      rootLabel: "MinIO archive",
    });
    await screen.findByRole("button", { name: "restic" });
    // A remote's root is a valid choice.
    expect(screen.getByRole("button", { name: "Select the remote's root" })).toBeEnabled();
    await user.click(row("restic"));
    await user.click(await screen.findByRole("button", { name: "legacy" }));
    await waitFor(() =>
      expect(api.of("ListRemoteFiles").at(-1)).toMatchObject({
        remoteId: fx.ids.minio,
        path: "restic/legacy",
      }),
    );
    // Breadcrumbs lead back.
    const crumbs = within(screen.getByRole("navigation", { name: "Breadcrumbs" }));
    expect(crumbs.getByText("legacy")).toHaveAttribute("aria-current", "page");
    await user.click(crumbs.getByRole("button", { name: "restic" }));
    await screen.findByRole("button", { name: "nas-docs" });
    await user.click(screen.getByRole("button", { name: "Select restic" }));
    expect(onSelect).toHaveBeenCalledWith(["restic"]);
  });

  it("selects several files and folders when asked to", async () => {
    const { user, onSelect } = open({
      source: { kind: "worker", workerId: fx.ids.nas },
      mode: "any",
      multiple: true,
      initialPath: "/srv/docs",
    });
    await user.click(await screen.findByRole("checkbox", { name: "Select notes.md" }));
    await user.click(screen.getByRole("checkbox", { name: "Select invoices" }));
    await user.click(screen.getByRole("button", { name: "Select 2 items" }));
    expect(onSelect).toHaveBeenCalledWith(["/srv/docs/notes.md", "/srv/docs/invoices"]);
  });

  it("filters the open folder in the browser", async () => {
    const { user } = open({
      source: { kind: "worker", workerId: fx.ids.nas },
      mode: "any",
      initialPath: "/srv/docs",
    });
    await screen.findByText("notes.md");
    await user.type(screen.getByLabelText("Filter this folder"), "pass");
    expect(screen.getByText("passwords.kdbx")).toBeInTheDocument();
    expect(screen.queryByText("notes.md")).not.toBeInTheDocument();
    expect(screen.getByText("1 of 5")).toBeInTheDocument();
  });
});
