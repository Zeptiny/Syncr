import { fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it } from "vitest";

import { WorkerStatus, type RunJobRequest } from "@/gen/syncr/v1/api_pb";
import { ResticRestoreOverwrite } from "@/gen/syncr/v1/link_pb";
import * as hooksMock from "@/mock/hooks";
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
  it("offers no hooks section when the worker defines none", async () => {
    open(m5.ids.photosVault, { mode: "snapshot" });
    await screen.findByRole("combobox", { name: "Worker" });
    expect(screen.queryByText(/^Hooks/)).toBeNull();
    expect(screen.queryByRole("combobox", { name: /^Add a hook/ })).toBeNull();
  });

  it("runs the worker's hooks around a restore", async () => {
    const { api, user } = open(m5.ids.photosVault, { mode: "snapshot" }, hooksMock.applyWorkers);
    await screen.findByRole("combobox", { name: "Worker" });
    expect(
      screen.getByText(/Commands nas-01's operator prepared, run around the restore/),
    ).toBeVisible();
    await user.click(screen.getByRole("combobox", { name: "Add a hook: before the job" }));
    await user.click(await screen.findByRole("option", { name: /stop-paperless/ }));
    await user.click(screen.getByRole("combobox", { name: "Add a hook: always, at the end" }));
    await user.click(await screen.findByRole("option", { name: /start-paperless/ }));
    await user.click(screen.getByRole("button", { name: "Start restore" }));
    await waitFor(() => expect(api.of("RunJob")).toHaveLength(1));
    const def = api.of<RunJobRequest>("RunJob")[0].definition;
    expect(def?.operation.case).toBe("resticRestore");
    expect(def?.hooks).toMatchObject({ before: ["stop-paperless"], after: ["start-paperless"] });
  });

  describe("to the original location", () => {
    const paths = ["/srv/photos/README.txt", "/srv/photos/2026/01"];

    it("proposes the snapshot's machine and the items' folder, keeping existing files", async () => {
      const { user, api, snapshot } = open(m5.ids.photosVault, { mode: "original", paths });
      expect(await screen.findByRole("combobox", { name: "Worker" })).toHaveTextContent("nas-01");
      expect(screen.getByLabelText("Target folder")).toHaveValue("/srv/photos");
      expect(
        screen.getByText(
          "Back where they were: the items go into the target as they were in /srv/photos.",
        ),
      ).toBeInTheDocument();
      expect(screen.getByRole("radio", { name: /^Keep existing/ })).toHaveAttribute(
        "aria-checked",
        "true",
      );
      expect(screen.getByTestId("restore-summary")).toHaveTextContent(
        "Restores 2 items into /srv/photos on nas-01 and keeps existing files.",
      );
      await user.click(screen.getByRole("button", { name: "Start restore" }));
      await waitFor(() => expect(api.of("RunJob")).toHaveLength(1));
      const req = api.of<RunJobRequest>("RunJob")[0];
      expect(req.workerId).toBe(fx.ids.nas);
      expect(req.definition?.operation).toMatchObject({
        case: "resticRestore",
        value: {
          snapshotId: snapshot.id,
          target: "/srv/photos",
          subfolder: "/srv/photos",
          paths,
          excludes: [],
          overwrite: ResticRestoreOverwrite.NEVER,
        },
      });
      // Going back is not a target to propose next time.
      expect(localStorage.getItem(`syncr-restore-target:${m5.ids.photosVault}`)).toBeNull();
    });

    it("lets the user pick when the snapshot's machine is not online", async () => {
      open(m5.ids.officeLaptop, { mode: "original", paths: ["/C/Users/ada/Documents/x.txt"] });
      const worker = await screen.findByRole("combobox", { name: "Worker" });
      expect(worker).toHaveTextContent("Choose a worker");
      expect(screen.getByText(/No online worker is OFFICE-PC/)).toBeInTheDocument();
    });

    it("blocks a target outside the worker's allowed folders", async () => {
      const { user, api } = open(m5.ids.photosVault, { mode: "original", paths });
      await user.click(await screen.findByRole("combobox", { name: "Worker" }));
      await user.click(await screen.findByRole("option", { name: /backup-host/ }));
      expect(screen.getByLabelText("Target folder")).toHaveValue("/srv/photos");
      expect(screen.getByText(/Not inside backup-host's allowed folders/)).toBeInTheDocument();
      expect(screen.getByRole("button", { name: "Start restore" })).toBeDisabled();
      expect(api.of("RunJob")).toHaveLength(0);
    });
  });

  describe("remembered target", () => {
    const key = `syncr-restore-target:${m5.ids.photosVault}`;

    it("proposes the last worker and folder before the dated default", async () => {
      localStorage.setItem(
        key,
        JSON.stringify({ workerId: fx.ids.backupHost, target: "/srv/backup/last" }),
      );
      open(m5.ids.photosVault, { mode: "snapshot" });
      expect(await screen.findByRole("combobox", { name: "Worker" })).toHaveTextContent(
        "backup-host",
      );
      expect(screen.getByLabelText("Target folder")).toHaveValue("/srv/backup/last");
    });

    it("remembers a started restore and ignores what it cannot read", async () => {
      localStorage.setItem(key, "{not json");
      const { user, snapshot } = open(m5.ids.photosVault, {
        mode: "items",
        paths: ["/srv/photos/README.txt"],
      });
      expect(await screen.findByRole("combobox", { name: "Worker" })).toHaveTextContent("nas-01");
      const target = `/srv/restore-${short(snapshot.id)}-20261110`;
      expect(screen.getByLabelText("Target folder")).toHaveValue(target);
      await user.click(screen.getByRole("button", { name: "Start restore" }));
      await waitFor(() =>
        expect(JSON.parse(localStorage.getItem(key) ?? "{}")).toEqual({
          workerId: fx.ids.nas,
          target,
        }),
      );
    });

    it("falls back when the remembered worker is gone or offline", async () => {
      localStorage.setItem(
        key,
        JSON.stringify({ workerId: fx.ids.office, target: "D:\\Shared\\r" }),
      );
      open(m5.ids.photosVault, { mode: "snapshot" });
      expect(await screen.findByRole("combobox", { name: "Worker" })).toHaveTextContent("nas-01");
    });
  });
});
