import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { JobStatus, type RunJobRequest } from "@/gen/syncr/v1/api_pb";
import { BisyncConflictLoser, JobErrorCode } from "@/gen/syncr/v1/link_pb";
import type { CreateScheduleRequest } from "@/gen/syncr/v1/schedules_pb";
import * as bisyncMock from "@/mock/bisync";
import { bisyncApi } from "@/test/bisync-api";
import * as fx from "@/test/fixtures";
import { renderApp } from "@/test/render";

import { emptyBisync } from "./bisync-definition";
import { emptyDefinition } from "./definition";
import { emptySchedule, validateSchedule } from "../schedules/model";

const now = Date.now();

// These tests drive whole pages through the router, which is slow under a
// parallel run.
describe("two-way sync pages", { timeout: 30_000 }, () => {
  it("offers Resync now after RESYNC_REQUIRED and runs a resync on the same worker", async () => {
    const user = userEvent.setup();
    const api = bisyncApi(now);
    const failed = api.failed(JobErrorCode.RESYNC_REQUIRED);
    const { router } = renderApp(`/jobs/${failed.id}`, api.transport);

    expect(await screen.findByText("Resync required")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Run once anyway" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Resync now" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(dialog).toHaveTextContent("nothing is deleted");
    expect(dialog).toHaveTextContent("the newer copy is kept");
    expect(dialog).toHaveTextContent("on nas-01");
    await user.click(within(dialog).getByRole("button", { name: "Resync now" }));

    await waitFor(() => expect(api.of<RunJobRequest>("RunJob")).toHaveLength(1));
    const req = api.of<RunJobRequest>("RunJob")[0];
    expect(req.workerId).toBe(fx.ids.nas);
    expect(req.definition?.operation).toMatchObject({
      case: "rcloneBisync",
      value: { resync: true, force: false, path1: { path: bisyncMock.LOCAL_PATH } },
    });
    await waitFor(() => expect(router.state.location.pathname).toBe(`/jobs/${api.jobs[0].id}`));
  });

  it("offers Run once anyway after SAFETY_ABORT, repeating the worker's counts", async () => {
    const user = userEvent.setup();
    const api = bisyncApi(now);
    const failed = api.failed(JobErrorCode.SAFETY_ABORT);
    renderApp(`/jobs/${failed.id}`, api.transport);

    expect(await screen.findByText("Safety check stopped the sync")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Resync now" })).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Run once anyway" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(dialog).toHaveTextContent("731 of 1268");
    expect(dialog).toHaveTextContent("deletions included");
    await user.click(within(dialog).getByRole("button", { name: "Run once anyway" }));

    await waitFor(() => expect(api.of<RunJobRequest>("RunJob")).toHaveLength(1));
    expect(api.of<RunJobRequest>("RunJob")[0].definition?.operation).toMatchObject({
      value: { resync: false, force: true },
    });
  });

  it("offers no override after SIDE_UNAVAILABLE, only a hint", async () => {
    const api = bisyncApi(now);
    renderApp(`/jobs/${api.failed(JobErrorCode.SIDE_UNAVAILABLE).id}`, api.transport);
    expect(await screen.findByText("A folder looked unavailable")).toBeInTheDocument();
    expect(screen.getByText(/RCLONE_TEST file/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Resync now" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Run once anyway" })).not.toBeInTheDocument();
  });

  it("offers the actions to the owner only", async () => {
    const api = bisyncApi(now, "alice");
    renderApp(`/jobs/${api.failed(JobErrorCode.RESYNC_REQUIRED).id}`, api.transport);
    expect(await screen.findByText("Resync required")).toBeInTheDocument();
    expect(screen.getByText(/needs a resync before/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Resync now" })).not.toBeInTheDocument();
  });

  it("shows the conflicts and a resync in the output", async () => {
    const api = bisyncApi(now);
    const conflicted = api.jobs.find((j) => j.warnings.length > 0)!;
    renderApp(`/jobs/${conflicted.id}`, api.transport);
    const list = await screen.findByRole("list", { name: "Conflicts" });
    expect(
      within(list)
        .getAllByRole("listitem")
        .map((li) => li.textContent),
    ).toEqual(bisyncMock.conflicts);
    expect(screen.getAllByText(/both versions kept, renamed/).length).toBeGreaterThan(0);
    expect(screen.getAllByText("Folder A").length).toBeGreaterThan(0);
  });

  it("offers the same actions on the schedule page for its last run", async () => {
    const user = userEvent.setup();
    const api = bisyncApi(now);
    renderApp(`/schedules/${bisyncMock.ids.schedule}`, api.transport);
    expect(await screen.findByText("Last run: Safety check stopped the sync")).toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Run once anyway" }));
    await user.click(
      within(await screen.findByRole("alertdialog")).getByRole("button", {
        name: "Run once anyway",
      }),
    );
    await waitFor(() => expect(api.of<RunJobRequest>("RunJob")).toHaveLength(1));
    expect(api.of<RunJobRequest>("RunJob")[0]).toMatchObject({
      workerId: fx.ids.nas,
      definition: { operation: { value: { force: true } } },
    });
  });

  it("runs a two-way sync from the form, with conflicts kept by side", async () => {
    const user = userEvent.setup();
    const api = bisyncApi(now);
    renderApp("/jobs/new?kind=rclone_bisync", api.transport);
    await screen.findByRole("heading", { name: "Folders" });
    await user.click(screen.getByRole("combobox", { name: /^Worker/ }));
    await user.click(screen.getByRole("option", { name: /nas-01/ }));
    await user.type(document.getElementById("run-a-path")!, "/srv/shared");
    await user.type(document.getElementById("run-b-path")!, "/srv/shared/inner");
    await user.click(screen.getByRole("button", { name: "Run job" }));
    expect(await screen.findByText(/neither may be inside the other/)).toBeInTheDocument();
    expect(api.of("RunJob")).toHaveLength(0);

    await user.clear(document.getElementById("run-b-path")!);
    await user.type(document.getElementById("run-b-path")!, "/mnt/shared");
    await user.click(screen.getByRole("radio", { name: /^By side/ }));
    await user.type(screen.getByLabelText("Delete limit"), "20");
    await user.click(screen.getByRole("switch", { name: "Check access" }));
    await user.click(screen.getByRole("checkbox", { name: "Checksum" }));
    await user.click(screen.getByRole("button", { name: "Run job" }));
    await waitFor(() => expect(api.of<RunJobRequest>("RunJob")).toHaveLength(1));
    expect(api.of<RunJobRequest>("RunJob")[0].definition?.operation).toMatchObject({
      case: "rcloneBisync",
      value: {
        path1: { path: "/srv/shared" },
        path2: { path: "/mnt/shared" },
        resync: false,
        force: false,
        options: {
          maxDeletePercent: 20,
          checkAccess: true,
          conflictLoser: BisyncConflictLoser.PATHNAME,
          compare: [1, 2, 3],
        },
      },
    });
  });

  it("lands the server's errors on the right fields", async () => {
    const user = userEvent.setup();
    const api = bisyncApi(now, "admin", [
      {
        field: "definition.rclone_bisync.path2",
        message: "must differ from path1, and neither may be inside the other",
      },
      { field: "definition.rclone_bisync.options.conflict_suffix", message: "bad suffix" },
      { field: "definition.rclone_bisync.options.max_delete_percent", message: "bad limit" },
      {
        field: "target.worker_label",
        message: "a two-way sync keeps its state on one worker; choose a worker",
      },
    ]);
    renderApp("/schedules/new?kind=rclone_bisync", api.transport);
    await user.type(await screen.findByLabelText("Name"), "Shared");
    await user.type(document.getElementById("sched-a-path")!, "/srv/shared");
    await user.type(document.getElementById("sched-b-path")!, "/mnt/shared");
    await user.click(screen.getByRole("combobox", { name: /^Worker/ }));
    await user.click(screen.getByRole("option", { name: /nas-01/ }));
    await user.click(screen.getByRole("button", { name: "Create schedule" }));
    await waitFor(() => expect(api.of<CreateScheduleRequest>("CreateSchedule")).toHaveLength(1));

    const folderB = screen.getByRole("group", { name: "Folder B" });
    expect(await within(folderB).findByText(/neither may be inside the other/)).toBeInTheDocument();
    expect(document.querySelector('[data-field="sched-bisync-conflict-suffix"]')).toHaveTextContent(
      "bad suffix",
    );
    expect(document.querySelector('[data-field="sched-bisync-max-delete"]')).toHaveTextContent(
      "bad limit",
    );
    expect(screen.getByText(/keeps its state on one worker; choose a worker/)).toBeInTheDocument();
  });

  it("disables the label target for a two-way sync, with the reason", async () => {
    const user = userEvent.setup();
    const api = bisyncApi(now);
    renderApp("/schedules/new", api.transport);
    await user.click(await screen.findByRole("radio", { name: "Any worker with a label" }));
    expect(screen.getByRole("radio", { name: "Any worker with a label" })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    await user.click(screen.getByRole("radio", { name: /^Two-way sync/ }));
    const label = screen.getByRole("radio", { name: "Any worker with a label" });
    expect(label).toBeDisabled();
    expect(label).toHaveAttribute("aria-checked", "false");
    expect(label).toHaveAccessibleDescription(/keeps its state on one worker/);
    expect(screen.getByRole("radio", { name: "A specific worker" })).toHaveAttribute(
      "aria-checked",
      "true",
    );
  });

  it("warns that changed filters of a saved pair need a resync", async () => {
    const user = userEvent.setup();
    const api = bisyncApi(now);
    renderApp(`/schedules/${bisyncMock.ids.schedule}/edit`, api.transport);
    await screen.findByRole("heading", { name: "Filters" });
    expect(screen.queryByText(/next run will stop with/)).not.toBeInTheDocument();
    await user.click(screen.getByRole("button", { name: "Remove filter 2" }));
    expect(screen.getByRole("status", { name: "" })).toHaveTextContent(
      "the pair's next run will stop with “Resync required”",
    );
  });

  it("marks a queued two-way sync that waits for an older one on the same folder", async () => {
    const api = bisyncApi(now);
    const [ahead, waiting] = api.jobs;
    waiting.status = JobStatus.QUEUED;
    waiting.waitingForJobId = ahead.id;
    renderApp(`/jobs/${waiting.id}`, api.transport);

    const chip = await screen.findByRole("link", { name: /Waiting for folder/ });
    expect(chip).toHaveAttribute("href", `/jobs/${ahead.id}`);
    expect(chip).toHaveAttribute(
      "title",
      expect.stringContaining("two-way sync of the same folder"),
    );
    expect(screen.queryByText(/Waiting for repository/)).toBeNull();
  });
});

describe("two-way sync schedules", () => {
  it("refuses a label target in the browser too", () => {
    const s = {
      ...emptySchedule("UTC"),
      name: "Shared",
      target: { kind: "label" as const, workerId: "", label: "nas" },
      definition: {
        ...emptyDefinition("rclone_bisync"),
        bisync: {
          ...emptyBisync(),
          path1: { remoteId: "", path: "/srv/a" },
          path2: { remoteId: "", path: "/srv/b" },
        },
      },
    };
    expect(validateSchedule(s).target).toMatch(/keeps its state on one worker/);
    expect(validateSchedule({ ...s, definition: emptyDefinition("restic_check") }).target).toBe(
      undefined,
    );
  });
});
