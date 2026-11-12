import { fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it } from "vitest";

import { WorkerStatus, type RunJobRequest } from "@/gen/syncr/v1/api_pb";
import { ResticRestoreOverwrite } from "@/gen/syncr/v1/link_pb";
import * as m5 from "@/mock/m5";
import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { renderInRouter } from "@/test/render";

import type { RestoreWhat } from "./model";
import { RestoreSheet } from "./restore-sheet";

const now = Date.UTC(2026, 10, 10, 12);
const day = new Date(2026, 10, 10, 9);

function open(
  repoId: string,
  what: RestoreWhat,
  tweak?: (w: ReturnType<typeof m5.workers>) => void,
) {
  const api = m5Api(now);
  const state = api.repo(repoId);
  const workers = m5.workers(now);
  tweak?.(workers);
  const snapshot = state.snapshots[0];
  const view = renderInRouter(
    <RestoreSheet
      open
      onOpenChange={() => {}}
      repository={state.repo}
      snapshot={snapshot}
      what={what}
      workers={workers}
      now={day}
    />,
    { transport: api.transport },
  );
  return { api, snapshot, user: userEvent.setup(), ...view };
}

const short = (id: string) => id.slice(0, 8);

describe("RestoreSheet", () => {
  it("starts on the snapshot's worker, in a new dated folder, keeping existing files", async () => {
    const { snapshot } = open(m5.ids.photosVault, { mode: "snapshot" });
    expect(await screen.findByRole("combobox", { name: "Worker" })).toHaveTextContent("nas-01");
    const target = `/srv/restore-${short(snapshot.id)}-20261110`;
    expect(screen.getByLabelText("Target folder")).toHaveValue(target);

    const radios = screen.getAllByRole("radio");
    expect(radios).toHaveLength(4);
    expect(screen.getByRole("radio", { name: /^Keep existing/ })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    expect(radios.filter((r) => r.getAttribute("aria-checked") === "true")).toHaveLength(1);
    expect(screen.getByRole("switch", { name: "Verify after restoring" })).not.toBeChecked();
    expect(screen.getByTestId("restore-summary")).toHaveTextContent(
      `Restores the whole snapshot into ${target} on nas-01 and keeps existing files.`,
    );
  });

  it("falls back to the first usable worker when the snapshot's host is not online", async () => {
    const { snapshot } = open(m5.ids.officeLaptop, { mode: "snapshot" });
    // OFFICE-PC is offline: backup-host is the first online worker with restic.
    expect(await screen.findByRole("combobox", { name: "Worker" })).toHaveTextContent(
      "backup-host",
    );
    expect(screen.getByLabelText("Target folder")).toHaveValue(
      `/srv/backup/restore-${short(snapshot.id)}-20261110`,
    );
  });

  it("uses the Windows worker and path style when the snapshot's host is online", async () => {
    const { snapshot, user } = open(
      m5.ids.officeLaptop,
      { mode: "folder", folder: "/C/Users/ada/Documents/Projects" },
      (workers) => {
        workers.find((w) => w.id === fx.ids.office)!.status = WorkerStatus.ONLINE;
      },
    );
    expect(await screen.findByRole("combobox", { name: "Worker" })).toHaveTextContent("office-pc");
    const target = screen.getByLabelText("Target folder");
    expect(target).toHaveValue(`C:\\Users\\ada\\restore-${short(snapshot.id)}-20261110`);
    // A folder restore can go back where it came from, in the worker's path form.
    await user.click(screen.getByRole("button", { name: "Restore to the original location" }));
    expect(target).toHaveValue("C:\\Users\\ada\\Documents\\Projects");
    expect(screen.getByTestId("restore-summary")).toHaveTextContent(
      "Restores the contents of /C/Users/ada/Documents/Projects into C:\\Users\\ada\\Documents\\Projects on office-pc and keeps existing files.",
    );
  });

  it("offers no original location for items or a whole snapshot", async () => {
    open(m5.ids.photosVault, {
      mode: "items",
      paths: ["/srv/photos/2026/01", "/srv/photos/README.txt"],
    });
    await screen.findByRole("combobox", { name: "Worker" });
    expect(screen.queryByRole("button", { name: "Restore to the original location" })).toBeNull();
    expect(screen.getByText("/srv/photos/README.txt")).toBeInTheDocument();
    expect(screen.getByTestId("restore-summary")).toHaveTextContent("Restores 2 items into");
  });

  it("warns, without blocking, about a target outside the worker's folders", async () => {
    const { user } = open(m5.ids.photosVault, { mode: "snapshot" });
    const target = await screen.findByLabelText("Target folder");
    await user.clear(target);
    await user.type(target, "/home/ada/restore");
    expect(screen.getByText(/Not inside nas-01's allowed folders/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Start restore" })).toBeEnabled();
    await user.clear(target);
    await user.type(target, "/mnt/restore");
    expect(screen.queryByText(/Not inside/)).toBeNull();
  });

  it("sends the chosen overwrite and options, then opens the job", async () => {
    const { user, api, snapshot, location } = open(m5.ids.photosVault, {
      mode: "items",
      paths: ["/srv/photos/2026/01"],
    });
    await user.click(await screen.findByRole("radio", { name: /^Replace older files/ }));
    await user.click(screen.getByRole("switch", { name: "Verify after restoring" }));
    await user.click(screen.getByRole("button", { name: "Start restore" }));
    await waitFor(() => expect(api.of("RunJob")).toHaveLength(1));
    const req = api.of<RunJobRequest>("RunJob")[0];
    expect(req.workerId).toBe(fx.ids.nas);
    expect(req.definition?.operation).toMatchObject({
      case: "resticRestore",
      value: {
        repositoryId: m5.ids.photosVault,
        snapshotId: snapshot.id,
        target: `/srv/restore-${short(snapshot.id)}-20261110`,
        paths: ["/srv/photos/2026/01"],
        subfolder: "",
        verify: true,
        overwrite: ResticRestoreOverwrite.IF_NEWER,
      },
    });
    await waitFor(() => expect(location().pathname).toMatch(/^\/jobs\/job-/));
  });

  it("keeps the proposed worker and folder when the workers are refetched", async () => {
    const api = m5Api(now);
    const state = api.repo(m5.ids.photosVault);
    const snapshot = state.snapshots[0];
    const refetched = m5.workers(now);
    refetched.find((w) => w.id === fx.ids.nas)!.status = WorkerStatus.OFFLINE;
    function Refetching() {
      const [workers, setWorkers] = useState(() => m5.workers(now));
      return (
        <>
          <button type="button" onClick={() => setWorkers(refetched)}>
            refetch
          </button>
          <RestoreSheet
            open
            onOpenChange={() => {}}
            repository={state.repo}
            snapshot={snapshot}
            what={{ mode: "snapshot" }}
            workers={workers}
            now={day}
          />
        </>
      );
    }
    renderInRouter(<Refetching />, { transport: api.transport });
    const target = `/srv/restore-${short(snapshot.id)}-20261110`;
    expect(await screen.findByRole("combobox", { name: "Worker" })).toHaveTextContent("nas-01");
    // The sheet is modal, so the button outside it takes a plain click.
    fireEvent.click(screen.getByText("refetch"));
    expect(
      await screen.findByText("nas-01 is offline: the restore waits until it is back."),
    ).toBeInTheDocument();
    expect(screen.getByRole("combobox", { name: "Worker" })).toHaveTextContent("nas-01");
    expect(screen.getByLabelText("Target folder")).toHaveValue(target);
  });

  it("sends Keep existing when nothing is touched", async () => {
    const { user, api } = open(m5.ids.photosVault, { mode: "snapshot" });
    await user.click(await screen.findByRole("button", { name: "Start restore" }));
    await waitFor(() => expect(api.of("RunJob")).toHaveLength(1));
    expect(api.of<RunJobRequest>("RunJob")[0].definition?.operation?.value).toMatchObject({
      overwrite: ResticRestoreOverwrite.NEVER,
      paths: [],
      subfolder: "",
    });
  });
});
