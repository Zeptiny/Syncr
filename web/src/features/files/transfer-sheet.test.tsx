import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import type { RunJobRequest } from "@/gen/syncr/v1/api_pb";
import { FilterAction, RcloneTransferMode } from "@/gen/syncr/v1/link_pb";
import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { renderApp } from "@/test/render";

const now = Date.UTC(2026, 10, 10, 12);

type User = ReturnType<typeof userEvent.setup>;

/** openDocs opens nas-01's /srv/docs and ticks names. */
async function openDocs(user: User, ...names: string[]) {
  const api = m5Api(now);
  const view = renderApp(
    `/workers/${fx.ids.nas}/files?path=${encodeURIComponent("/srv/docs")}`,
    api.transport,
  );
  const grid = await screen.findByRole("grid", { name: "Contents of /srv/docs" });
  for (const n of names)
    await user.click(await within(grid).findByRole("checkbox", { name: `Select ${n}` }));
  return { api, grid, ...view };
}

async function openSheet(user: User) {
  const tray = screen.getByRole("region", { name: "Selection" });
  await user.click(within(tray).getByRole("button", { name: "Copy or move…" }));
  return screen.findByRole("dialog", { name: /^(Copy|Move) / });
}

async function chooseDestination(user: User, sheet: HTMLElement, where: RegExp, folder: string) {
  await user.click(within(sheet).getByRole("combobox", { name: "Where" }));
  await user.click(await screen.findByRole("option", { name: where }));
  const input = within(sheet).getByRole("textbox", { name: "Folder" });
  await user.clear(input);
  await user.type(input, folder);
}

const transferOf = (req: RunJobRequest | undefined) => {
  const op = req?.definition?.operation;
  if (op?.case !== "rcloneTransfer") throw new Error("not a transfer");
  return op.value;
};

describe("TransferSheet", { timeout: 30_000 }, () => {
  it("previews, then copies a selection from a worker's folder to a remote", async () => {
    const user = userEvent.setup();
    const { api } = await openDocs(user, "contracts", "notes.md");
    const sheet = await openSheet(user);
    expect(within(sheet).getByRole("heading", { name: "Copy 2 items" })).toBeInTheDocument();
    expect(within(sheet).getByText("From /srv/docs on nas-01")).toBeInTheDocument();
    expect(within(sheet).getByRole("list", { name: "Items" })).toHaveTextContent(
      "contractsnotes.md",
    );
    expect(within(sheet).getByTestId("transfer-worker")).toHaveTextContent(
      "nas-01 runs the job: the files are on its disk.",
    );
    // Keep existing is the default.
    expect(within(sheet).getByRole("radio", { name: /^Keep existing/ })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    await chooseDestination(user, sheet, /^B2 offsite/, "inbox");
    expect(within(sheet).getByTestId("transfer-summary")).toHaveTextContent(
      "Copies 2 items from /srv/docs on nas-01 into B2 offsite: inbox and keeps existing files.",
    );

    await user.click(within(sheet).getByRole("button", { name: "Preview" }));
    await waitFor(() => expect(api.of<RunJobRequest>("RunJob")).toHaveLength(1));
    const preview = api.of<RunJobRequest>("RunJob")[0];
    expect(preview.workerId).toBe(fx.ids.nas);
    expect(transferOf(preview)).toMatchObject({
      mode: RcloneTransferMode.COPY,
      source: { remoteId: "", path: "/srv/docs" },
      destination: { remoteId: fx.ids.b2, path: "inbox" },
      options: {
        dryRun: true,
        ignoreExisting: true,
        update: false,
        createEmptySrcDirs: true,
        deleteEmptySrcDirs: false,
        filters: [
          { action: FilterAction.INCLUDE, pattern: "/contracts/**" },
          { action: FilterAction.INCLUDE, pattern: "/notes.md" },
          { action: FilterAction.EXCLUDE, pattern: "**" },
        ],
      },
    });
    expect(await screen.findByText("Preview queued")).toBeInTheDocument();
    await waitFor(() => expect(screen.queryByRole("dialog")).toBeNull());
    act(() => api.finish(api.jobs[0].id));
    expect(await screen.findByText("Preview finished")).toBeInTheDocument();

    // A preview changes nothing; the copy does.
    const again = await openSheet(user);
    await chooseDestination(user, again, /^B2 offsite/, "inbox");
    await user.click(within(again).getByRole("radio", { name: /^Replace older files/ }));
    await user.click(within(again).getByRole("button", { name: "Copy" }));
    await waitFor(() => expect(api.of<RunJobRequest>("RunJob")).toHaveLength(2));
    expect(transferOf(api.of<RunJobRequest>("RunJob")[1]).options).toMatchObject({
      dryRun: false,
      ignoreExisting: false,
      update: true,
    });
    act(() => api.finish(api.jobs[0].id));
    expect(await screen.findByText("Copied 2 items into B2 offsite: inbox")).toBeInTheDocument();
  });

  it("asks before a move, then refreshes the folder the items left", async () => {
    const user = userEvent.setup();
    const { api, grid } = await openDocs(user, "notes.md");
    const sheet = await openSheet(user);
    await user.click(within(sheet).getByRole("radio", { name: /^Move/ }));
    expect(within(sheet).getByRole("heading", { name: "Move 1 item" })).toBeInTheDocument();
    await chooseDestination(user, sheet, /^Folders on nas-01/, "/srv/backup");
    await user.click(within(sheet).getByRole("button", { name: "Move…" }));
    const confirm = await screen.findByRole("alertdialog", { name: "Move 1 item?" });
    // Keep existing (the default): what is skipped stays in the source.
    expect(confirm).toHaveTextContent(
      "1 item will be deleted from /srv/docs on nas-01 once copied. Files already at the destination are skipped and stay in /srv/docs on nas-01.",
    );
    expect(api.of("RunJob")).toHaveLength(0);
    await user.click(within(confirm).getByRole("button", { name: "Move" }));
    await waitFor(() => expect(api.of<RunJobRequest>("RunJob")).toHaveLength(1));
    const req = transferOf(api.of<RunJobRequest>("RunJob")[0]);
    expect(req).toMatchObject({
      mode: RcloneTransferMode.MOVE,
      destination: { remoteId: "", path: "/srv/backup" },
      options: { deleteEmptySrcDirs: true, createEmptySrcDirs: true, dryRun: false },
    });

    const listed = api.of("ListWorkerFiles").length;
    act(() => api.finish(api.jobs[0].id));
    expect(
      await screen.findByText("Move of 1 item into /srv/backup on nas-01 finished"),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        "Files that were already at the destination were skipped and stay in the source. The job's page has the counts.",
      ),
    ).toBeInTheDocument();
    await waitFor(() => expect(api.of("ListWorkerFiles").length).toBeGreaterThan(listed));
    await waitFor(() => expect(within(grid).queryByText("notes.md")).toBeNull());
    // The moved item left the selection when the move started.
    expect(screen.queryByRole("region", { name: "Selection" })).toBeNull();
    expect(within(grid).getByText("passwords.kdbx")).toBeInTheDocument();
  });

  it("refuses a folder copied into itself, its own folder and two workers' disks", async () => {
    const user = userEvent.setup();
    const { api } = await openDocs(user, "taxes", "notes.md");
    const sheet = await openSheet(user);
    await chooseDestination(user, sheet, /^Folders on nas-01/, "/srv/docs/taxes/2025");
    await user.click(within(sheet).getByRole("button", { name: "Copy" }));
    expect(
      await within(sheet).findByText("A folder can't be copied into itself: taxes is selected."),
    ).toBeInTheDocument();

    const input = within(sheet).getByRole("textbox", { name: "Folder" });
    await user.clear(input);
    await user.type(input, "/srv/docs");
    await user.click(within(sheet).getByRole("button", { name: "Copy" }));
    expect(
      await within(sheet).findByText("The items are already in this folder. Choose another one."),
    ).toBeInTheDocument();

    await chooseDestination(user, sheet, /^Folders on backup-host/, "/srv/backup");
    expect(within(sheet).getByRole("alert")).toHaveTextContent(
      "These folders are on two different workers' disks, and a job runs on one worker. Copy through a remote instead.",
    );
    expect(within(sheet).getByRole("button", { name: "Copy" })).toBeDisabled();
    await user.click(within(sheet).getByRole("button", { name: "Preview" }));
    expect(api.of("RunJob")).toHaveLength(0);
  });

  it("copies between remotes through the listing worker and opens the job editor", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    const { router } = renderApp(`/remotes/${fx.ids.minio}/files?path=archive`, api.transport);
    const grid = await screen.findByRole("grid", { name: "Contents of archive" });
    await user.click(await within(grid).findByRole("button", { name: "Actions for 2025" }));
    await user.click(await screen.findByRole("menuitem", { name: "Copy or move…" }));
    const sheet = await screen.findByRole("dialog", { name: "Copy 1 item" });
    await chooseDestination(user, sheet, /^B2 offsite/, "old");
    // nas-01 listed MinIO; by name alone backup-host would come first.
    expect(within(sheet).getByRole("combobox", { name: "Worker" })).toHaveTextContent("nas-01");

    await user.click(within(sheet).getByRole("button", { name: /Open in the job editor/ }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/jobs/new"));
    expect(api.of("RunJob")).toHaveLength(0);
    const from = await screen.findByRole("group", { name: "From" });
    expect(within(from).getByLabelText("Path")).toHaveValue("archive");
    expect(within(screen.getByRole("group", { name: "To" })).getByLabelText("Path")).toHaveValue(
      "old",
    );
    expect(screen.getByText("2 filters")).toBeInTheDocument();
  });
});
