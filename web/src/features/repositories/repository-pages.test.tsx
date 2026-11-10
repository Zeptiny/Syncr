import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import type { RunJobRequest } from "@/gen/syncr/v1/api_pb";
import {
  RepositoryCreateMode,
  type CreateRepositoryRequest,
  type UpdateRepositoryRequest,
} from "@/gen/syncr/v1/restic_pb";
import * as m5 from "@/mock/m5";
import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { renderApp } from "@/test/render";

const now = Date.now();

// These tests drive whole pages through the router, which is slow under a
// parallel run.
describe("repository pages, on the mock", { timeout: 30_000 }, () => {
  it("renders a repository: header, figures, tabs and its snapshots", async () => {
    const api = m5Api(now);
    renderApp(`/repositories/${m5.ids.photosVault}`, api.transport);
    expect(await screen.findByRole("heading", { name: "Photos vault" })).toBeInTheDocument();
    const main = within(screen.getByRole("main"));
    expect(main.getByText("B2 offsite")).toBeInTheDocument();
    expect(main.getByText("restic/photos")).toBeInTheDocument();
    for (const label of ["Snapshots", "Latest snapshot", "Latest size", "Last check"])
      expect(main.getAllByText(label).length).toBeGreaterThan(0);
    expect(main.getByText("no errors found")).toBeInTheDocument();
    for (const action of ["Back up now", "Check now", "Retention…"])
      expect(main.getByRole("link", { name: action })).toBeInTheDocument();
    expect(main.getByRole("link", { name: "Back up now" })).toHaveAttribute(
      "href",
      `/jobs/new?kind=restic_backup&repository=${m5.ids.photosVault}`,
    );
    expect(main.getByRole("button", { name: "Delete" })).toBeEnabled();
    expect(main.getAllByRole("tab").map((t) => t.textContent)).toEqual([
      "Snapshots",
      "Jobs",
      "Schedules (3)",
      "Settings",
    ]);
    const table = await main.findByRole("table", { name: "Snapshots" });
    await waitFor(() => expect(within(table).getAllByRole("row")).toHaveLength(31));
    expect(screen.queryByTestId("deletion-banner")).toBeNull();
  });

  it("locks a repository whose data is being deleted, and keeps the tabs readable", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    renderApp(`/repositories/${m5.ids.oldLaptop}`, api.transport);
    expect(await screen.findByRole("heading", { name: /Old laptop/ })).toHaveTextContent(
      "Deleting data…",
    );
    const main = within(screen.getByRole("main"));
    const banner = await main.findByTestId("deletion-banner");
    await waitFor(() =>
      expect(banner).toHaveTextContent(
        "Deleting the backup data on nas-01 — waiting for 1 job on this repository first",
      ),
    );
    for (const action of ["Back up now", "Check now", "Retention…", "Edit", "Delete"])
      expect(main.getByRole("button", { name: action })).toBeDisabled();
    expect(main.queryByRole("link", { name: "Back up now" })).toBeNull();
    expect(within(banner).getByRole("link", { name: "View job" })).toBeInTheDocument();
    expect(within(banner).getByRole("button", { name: "Cancel deletion" })).toBeEnabled();

    const table = await main.findByRole("table", { name: "Snapshots" });
    await waitFor(() => expect(within(table).getAllByRole("row")).toHaveLength(7));
    await user.click(main.getByRole("tab", { name: "Schedules (1)" }));
    expect(await main.findByText("Old laptop weekly backup")).toBeInTheDocument();
    expect(main.getByText("Paused automatically")).toBeInTheDocument();
    await user.click(main.getByRole("tab", { name: "Jobs" }));
    const jobs = await main.findByRole("table", { name: "Jobs" });
    expect(await within(jobs).findByText("Delete the data of Old laptop")).toBeInTheDocument();
    expect(within(jobs).getByRole("link", { name: /Waiting for repository/ })).toHaveAttribute(
      "href",
      `/jobs/${m5.ids.oldLaptopCheck}`,
    );
    await user.click(main.getByRole("tab", { name: "Settings" }));
    expect(await main.findByRole("button", { name: "Delete repository" })).toBeDisabled();
    expect(main.getByRole("button", { name: "Save changes" })).toBeDisabled();
  });

  it("after a failed deletion offers Try again with the data option, and Forget only", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    renderApp(`/repositories/${m5.ids.archive2019}`, api.transport);
    const failed = await screen.findByTestId("deletion-failed");
    expect(failed).toHaveTextContent("another restic client is using the repository");
    await user.click(within(failed).getByRole("button", { name: "Try again" }));
    let dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByRole("checkbox")).toBeChecked();
    expect(
      within(dialog).getByRole("button", { name: "Delete repository and data" }),
    ).toBeDisabled();
    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    await waitFor(() => expect(screen.queryByRole("alertdialog")).toBeNull());
    await user.click(within(failed).getByRole("button", { name: "Forget only" }));
    dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByRole("checkbox")).not.toBeChecked();
    expect(within(dialog).getByRole("button", { name: "Forget repository" })).toBeEnabled();
  });

  it("never shows the stored password; a new one maps the server's refusal to its field", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    const { router } = renderApp(`/repositories/${m5.ids.photosVault}?tab=settings`, api.transport);
    const password = await screen.findByLabelText("New password");
    expect(password).toHaveValue("");
    expect(password).toHaveAttribute("type", "password");
    await user.type(password, "wrong-guess");
    await user.click(screen.getByRole("button", { name: "Update password" }));
    expect(await screen.findByText("Is not this repository's password")).toBeInTheDocument();
    expect(api.of<UpdateRepositoryRequest>("UpdateRepository")[0]).toMatchObject({
      repositoryId: m5.ids.photosVault,
      name: "Photos vault",
      password: "wrong-guess",
    });
    expect(router.state.location.href).not.toContain("wrong-guess");

    await user.clear(password);
    await user.type(password, "right-one");
    await user.click(screen.getByRole("button", { name: "Update password" }));
    await waitFor(() => expect(api.of("UpdateRepository")).toHaveLength(2));
    // Saved: the field is empty again (the page shows the new version).
    await waitFor(() => expect(screen.getByLabelText("New password")).toHaveValue(""));
    expect(document.body).not.toHaveTextContent("right-one");
  });

  it("lists repositories; a deleting one offers only its deletion job", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    renderApp("/repositories", api.transport);
    const table = await screen.findByRole("table", { name: "Repositories" });
    await waitFor(() => expect(within(table).getAllByRole("row")).toHaveLength(6));
    const row = (name: string) => within(table).getByRole("row", { name });
    expect(within(row("Photos vault")).getByText("30")).toBeInTheDocument();
    expect(within(row("Photos vault")).getByText("B2 offsite")).toBeInTheDocument();
    expect(within(row("Old laptop")).getByText("Deleting data…")).toBeInTheDocument();
    expect(within(row("Archive 2019")).getByText("Deletion failed")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Repositories" })).toHaveAttribute(
      "href",
      "/repositories",
    );

    await user.click(
      within(row("Old laptop")).getByRole("button", { name: "Actions for Old laptop" }),
    );
    let items = await screen.findAllByRole("menuitem");
    expect(items.map((i) => i.textContent)).toEqual(["View deletion job"]);
    expect(items[0]).toHaveAttribute("href", `/jobs/${m5.ids.oldLaptopDelete}`);
    await user.keyboard("{Escape}");

    await user.click(
      within(row("Photos vault")).getByRole("button", { name: "Actions for Photos vault" }),
    );
    items = await screen.findAllByRole("menuitem");
    expect(items.map((i) => i.textContent)).toEqual([
      "Back up now",
      "Browse snapshots",
      "Check",
      "Settings",
    ]);
  });

  it("shows another user only their own repositories", async () => {
    const api = m5Api(now, "alice");
    renderApp("/repositories", api.transport);
    const table = await screen.findByRole("table", { name: "Repositories" });
    await waitFor(() => expect(within(table).getAllByRole("row")).toHaveLength(2));
    expect(within(table).getByText("Alice's backups")).toBeInTheDocument();
  });

  it("connects a repository: server field errors land on fields, the password stays out of the URL", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    const { router } = renderApp(`/repositories/new?remote=${fx.ids.minio}`, api.transport);
    await user.click(await screen.findByRole("radio", { name: /^Connect to an existing one/ }));
    expect(screen.queryByLabelText("Repeat the password")).toBeNull();
    await waitFor(() =>
      expect(screen.getByRole("combobox", { name: "Remote" })).toHaveTextContent("MinIO archive"),
    );
    await user.type(screen.getByLabelText("Name"), "Legacy");
    await user.type(screen.getByLabelText("Path"), "restic/legacy");
    await user.type(screen.getByLabelText("Password"), "wrong-pass");
    await user.click(screen.getByRole("button", { name: "Connect repository" }));
    expect(await screen.findByText("Is not this repository's password")).toBeInTheDocument();
    expect(screen.getByLabelText("Password")).toHaveAttribute("aria-invalid", "true");
    expect(api.of<CreateRepositoryRequest>("CreateRepository")[0].input).toMatchObject({
      name: "Legacy",
      remoteId: fx.ids.minio,
      path: "restic/legacy",
      password: "wrong-pass",
      mode: RepositoryCreateMode.CONNECT,
      workerId: "",
    });

    // A path without a repository is a path error.
    await user.clear(screen.getByLabelText("Path"));
    await user.type(screen.getByLabelText("Path"), "restic/nothing");
    await user.clear(screen.getByLabelText("Password"));
    await user.type(screen.getByLabelText("Password"), "good-pass");
    await user.click(screen.getByRole("button", { name: "Connect repository" }));
    expect(await screen.findByText("No restic repository at this location")).toBeInTheDocument();

    await user.clear(screen.getByLabelText("Path"));
    await user.type(screen.getByLabelText("Path"), "restic/legacy");
    await user.click(screen.getByRole("button", { name: "Connect repository" }));
    await waitFor(() =>
      expect(router.state.location.pathname).toMatch(/^\/repositories\/7d3a1c9e-/),
    );
    expect(router.state.location.href).not.toMatch(/pass/);
    expect(await screen.findByRole("heading", { name: "Legacy" })).toBeInTheDocument();
  });

  it("creates a repository: generated password, confirmation, local remotes need a worker", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    renderApp("/repositories/new", api.transport);
    await user.type(await screen.findByLabelText("Name"), "New vault");
    await user.click(screen.getByRole("button", { name: "Create repository" }));
    expect(await screen.findByText("Choose the remote that holds the repository.")).toBeVisible();
    expect(screen.getByText("Enter the repository's password.")).toBeVisible();
    expect(api.of("CreateRepository")).toHaveLength(0);

    await user.click(screen.getByRole("button", { name: "Generate" }));
    const password = screen.getByLabelText<HTMLInputElement>("Password");
    expect(password.value).toMatch(/^[A-Za-z0-9]{24}$/);
    expect(screen.getByLabelText("Repeat the password")).toHaveValue(password.value);
    expect(
      screen.getByText(/Store this password safely; without it the backups cannot be read/),
    ).toBeVisible();
    await user.type(screen.getByLabelText("Repeat the password"), "x");
    await user.click(screen.getByRole("button", { name: "Create repository" }));
    expect(await screen.findByText("The two passwords differ.")).toBeVisible();

    // Only remotes that can hold a repository are offered; a local one preselects a worker.
    await user.click(screen.getByRole("combobox", { name: "Remote" }));
    const options = (await screen.findAllByRole("option")).map((o) => o.textContent);
    expect(options.some((o) => o?.startsWith("Pooled disks"))).toBe(false);
    await user.click(screen.getByRole("option", { name: /^Media disk/ }));
    await waitFor(() =>
      expect(screen.getByRole("combobox", { name: /^Worker/ })).toHaveTextContent("backup-host"),
    );
    expect(
      screen.getByText(/folder on a worker's disk, so that worker has to do it/),
    ).toBeVisible();
  });

  it("browses a snapshot, selects items and opens the restore sheet with Keep existing", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    const snap = api.repo(m5.ids.photosVault).snapshots[0];
    const { router } = renderApp(
      `/repositories/${m5.ids.photosVault}/snapshots/${snap.id}?path=%2Fsrv%2Fphotos`,
      api.transport,
    );
    const table = await screen.findByRole("table", { name: "Contents of /srv/photos" });
    await within(table).findByRole("button", { name: "2026" });
    // Folders first, then files.
    const names = within(table)
      .getAllByRole("row")
      .slice(1)
      .map((r) => r.querySelector("td:nth-child(2)")?.textContent);
    expect(names).toEqual(["2025", "2026", "catalog.lrcat", "README.txt"]);
    expect(screen.getByRole("button", { name: "Restore selected (0)" })).toBeDisabled();
    for (const n of ["2025", "catalog.lrcat", "README.txt"])
      await user.click(within(table).getByRole("checkbox", { name: `Select ${n}` }));
    const restore = screen.getByRole("button", { name: "Restore selected (3)" });
    expect(restore).toBeEnabled();
    // The snapshot's details sit next to the files.
    expect(screen.getAllByText("nas-01").length).toBeGreaterThan(0);

    await user.click(restore);
    const sheet = await screen.findByRole("dialog");
    expect(within(sheet).getByText("3 items")).toBeInTheDocument();
    expect(within(sheet).getByRole("radio", { name: /^Keep existing/ })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    await user.click(within(sheet).getByRole("button", { name: "Start restore" }));
    await waitFor(() => expect(api.of("RunJob")).toHaveLength(1));
    expect(api.of<RunJobRequest>("RunJob")[0].definition?.operation?.value).toMatchObject({
      paths: ["/srv/photos/2025", "/srv/photos/catalog.lrcat", "/srv/photos/README.txt"],
      snapshotId: snap.id,
    });
    await waitFor(() => expect(router.state.location.pathname).toMatch(/^\/jobs\//));
  });

  it("opens a folder from the list and from the breadcrumbs", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    const snap = api.repo(m5.ids.officeLaptop).snapshots[0];
    const { router } = renderApp(
      `/repositories/${m5.ids.officeLaptop}/snapshots/${snap.id}`,
      api.transport,
    );
    await user.click(await screen.findByRole("button", { name: "C" }));
    await user.click(await screen.findByRole("button", { name: "Users" }));
    await waitFor(() => expect(router.state.location.search).toEqual({ path: "/C/Users" }));
    // A Windows snapshot shows its first step as a drive.
    const crumbs = within(screen.getByRole("navigation", { name: "Breadcrumbs" }));
    expect(crumbs.getByRole("button", { name: "C:" })).toBeInTheDocument();
    await user.click(crumbs.getByRole("button", { name: "Snapshot" }));
    await waitFor(() => expect(router.state.location.search).toEqual({}));
    expect(screen.getByRole("button", { name: "Restore this folder…" })).toBeDisabled();
  });
});
