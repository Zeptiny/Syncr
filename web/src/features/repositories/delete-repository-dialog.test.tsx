import { create } from "@bufbuild/protobuf";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import {
  DeleteRepositoryRequestSchema,
  type DeleteRepositoryRequest,
} from "@/gen/syncr/v1/restic_pb";
import * as m5 from "@/mock/m5";
import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { renderWithProviders } from "@/test/render";

import { DeleteRepositoryDialog } from "./delete-repository-dialog";

const now = Date.UTC(2026, 10, 10, 12);

function open(
  opts: { local?: boolean; initialDeleteData?: boolean; activeJobs?: number; repo?: string } = {},
) {
  const api = m5Api(now);
  const state = api.repo(opts.repo ?? m5.ids.photosVault);
  if (opts.local) {
    // The same repository, but in a folder on a worker's disk.
    state.repo.remoteId = fx.ids.media;
    state.repo.remote = {
      $typeName: "syncr.v1.JobRemoteRef",
      id: fx.ids.media,
      name: "Media disk",
      backendType: "local",
      deleted: false,
    };
  }
  const onDone = vi.fn();
  const onOpenChange = vi.fn();
  renderWithProviders(
    <DeleteRepositoryDialog
      open
      onOpenChange={onOpenChange}
      repository={state.repo}
      schedules={api.data.schedules.filter((s) => s.repository?.id === state.repo.id)}
      workers={m5.workers(now)}
      activeJobs={opts.activeJobs}
      initialDeleteData={opts.initialDeleteData}
      onDone={onDone}
    />,
    { transport: api.transport },
  );
  return { api, state, onDone, onOpenChange, user: userEvent.setup() };
}

const sent = (api: ReturnType<typeof m5Api>) => api.of<DeleteRepositoryRequest>("DeleteRepository");
const dataBox = () => screen.getByRole("checkbox", { name: /Also delete the backup data/ });

describe("DeleteRepositoryDialog", () => {
  it("forgets only by default: no typed name, no warning, an ID-only request", async () => {
    const { user, api, onDone, state } = open();
    expect(screen.getByRole("alertdialog")).toHaveTextContent(
      "Syncr forgets this repository. The backups stay in B2 offsite: restic/photos.",
    );
    expect(dataBox()).not.toBeChecked();
    expect(dataBox().closest("label")).toHaveTextContent(
      "Also delete the backup data in B2 offsite: restic/photos",
    );
    expect(screen.queryByLabelText(/to confirm/)).toBeNull();
    expect(screen.queryByText(/This cannot be undone/)).toBeNull();
    expect(screen.queryByLabelText("Run on")).toBeNull();
    // The schedules using it are listed.
    for (const name of [
      "Nightly photos backup",
      "Weekly retention: photos",
      "Monthly deep check: photos",
    ])
      expect(screen.getByText(name)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Delete repository and data" })).toBeNull();

    const forget = screen.getByRole("button", { name: "Forget repository" });
    expect(forget).toBeEnabled();
    await user.click(forget);
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(sent(api)).toEqual([
      create(DeleteRepositoryRequestSchema, { repositoryId: m5.ids.photosVault }),
    ]);
    expect(sent(api)[0]).toMatchObject({ deleteData: false, confirmName: "", workerId: "" });
    expect(onDone.mock.calls[0][1]).toBe(false);
    expect(state.deleted).toBe(true);
  });

  it("deleting the data warns, lists what pauses and needs the exact name", async () => {
    const { user, api, onDone, state } = open({ activeJobs: 2 });
    await user.click(dataBox());
    const dialog = screen.getByRole("alertdialog");
    expect(dialog).toHaveTextContent(
      "Deletes restic's config, keys, data, index, snapshots and locks there. Other files are kept. This cannot be undone.",
    );
    expect(dialog).toHaveTextContent("A versioned bucket keeps older versions");
    expect(dialog).toHaveTextContent("These schedules are paused at once:");
    expect(screen.getByText("Nightly photos backup")).toBeInTheDocument();
    expect(dialog).toHaveTextContent("2 queued or running jobs on this repository finish first.");
    // Automatic worker by default for a remote that any worker reaches.
    expect(screen.getByRole("combobox", { name: "Run on" })).toHaveTextContent("Automatic");

    const destroy = screen.getByRole("button", { name: "Delete repository and data" });
    expect(screen.queryByRole("button", { name: "Forget repository" })).toBeNull();
    expect(destroy).toBeDisabled();
    const name = screen.getByLabelText(/Type Photos vault to confirm/);
    await user.type(name, "photos vault");
    expect(destroy).toBeDisabled();
    await user.clear(name);
    await user.type(name, "Photos vaul");
    expect(destroy).toBeDisabled();
    await user.type(name, "t");
    expect(destroy).toBeEnabled();

    await user.click(destroy);
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(sent(api)).toEqual([
      create(DeleteRepositoryRequestSchema, {
        repositoryId: m5.ids.photosVault,
        deleteData: true,
        confirmName: "Photos vault",
        workerId: "",
      }),
    ]);
    const [res, deleteData] = onDone.mock.calls[0] as [{ deletionJobId: string }, boolean];
    expect(deleteData).toBe(true);
    expect(res.deletionJobId).not.toBe("");
    expect(state.repo.deleting).toBe(true);
    expect(api.data.schedules.find((s) => s.id === m5.ids.nightlyBackup)).toMatchObject({
      enabled: false,
      pausedReason: "repository_deleting",
    });
  });

  it("sends the chosen worker as worker_id", async () => {
    const { user, api, onDone } = open();
    await user.click(dataBox());
    await user.click(screen.getByRole("combobox", { name: "Run on" }));
    await user.click(await screen.findByRole("option", { name: /backup-host/ }));
    await user.type(screen.getByLabelText(/to confirm/), "Photos vault");
    await user.click(screen.getByRole("button", { name: "Delete repository and data" }));
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(sent(api)[0]).toMatchObject({
      repositoryId: m5.ids.photosVault,
      deleteData: true,
      confirmName: "Photos vault",
      workerId: fx.ids.backupHost,
    });
  });

  it("requires a worker for a repository on a worker's disk", async () => {
    const { user, api, onDone } = open({ local: true });
    await user.click(dataBox());
    const destroy = screen.getByRole("button", { name: "Delete repository and data" });
    await user.type(screen.getByLabelText(/to confirm/), "Photos vault");
    // The name matches, but no worker is chosen and Automatic is not offered.
    expect(destroy).toBeDisabled();
    expect(screen.getByText(/on a worker's disk: choose that worker/)).toBeInTheDocument();
    await user.click(screen.getByRole("combobox", { name: "Run on" }));
    expect(screen.queryByRole("option", { name: "Automatic" })).toBeNull();
    await user.click(await screen.findByRole("option", { name: /nas-01/ }));
    expect(destroy).toBeEnabled();
    await user.click(destroy);
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(sent(api)[0]).toMatchObject({ deleteData: true, workerId: fx.ids.nas });
  });

  it("a forget of a local repository needs no worker", async () => {
    const { user, api, onDone } = open({ local: true });
    await user.click(screen.getByRole("button", { name: "Forget repository" }));
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(sent(api)[0]).toMatchObject({ deleteData: false, workerId: "", confirmName: "" });
  });

  it("unticking goes back to a plain forget and sends nothing of the data option", async () => {
    const { user, api, onDone } = open();
    await user.click(dataBox());
    await user.type(screen.getByLabelText(/to confirm/), "Photos vault");
    await user.click(dataBox());
    expect(screen.queryByLabelText(/to confirm/)).toBeNull();
    await user.click(screen.getByRole("button", { name: "Forget repository" }));
    await waitFor(() => expect(onDone).toHaveBeenCalled());
    expect(sent(api)).toEqual([
      create(DeleteRepositoryRequestSchema, { repositoryId: m5.ids.photosVault }),
    ]);
  });

  it("opens with the data option for Try again, still asking for the name", () => {
    open({ initialDeleteData: true, repo: m5.ids.archive2019 });
    expect(dataBox()).toBeChecked();
    expect(screen.getByRole("button", { name: "Delete repository and data" })).toBeDisabled();
    expect(screen.getByLabelText(/Type Archive 2019 to confirm/)).toHaveValue("");
  });

  it("shows the server's refusal of the name on the field and stays open", async () => {
    const { user, state, onDone, onOpenChange } = open();
    await user.click(dataBox());
    await user.type(screen.getByLabelText(/to confirm/), "Photos vault");
    // Renamed elsewhere in the meantime: the server compares with its own name.
    state.repo.name = "Photos vault 2";
    await user.click(screen.getByRole("button", { name: "Delete repository and data" }));
    expect(await screen.findByText("Type the repository's name to confirm.")).toBeInTheDocument();
    expect(onDone).not.toHaveBeenCalled();
    expect(onOpenChange).not.toHaveBeenCalledWith(false);
    expect(state.repo.deleting).toBe(false);
  });
});
