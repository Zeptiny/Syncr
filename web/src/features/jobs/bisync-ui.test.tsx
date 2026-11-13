import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { JobSchema, JobStatus, type RunJobRequest } from "@/gen/syncr/v1/api_pb";
import {
  BisyncConflictLoser,
  BisyncResyncReason,
  FilterAction,
  FilterRuleSchema,
  JobErrorCode,
  JobErrorSchema,
} from "@/gen/syncr/v1/link_pb";
import { ScheduleTargetSchema, type CreateScheduleRequest } from "@/gen/syncr/v1/schedules_pb";
import * as bisyncMock from "@/mock/bisync";
import { bisyncApi } from "@/test/bisync-api";
import * as fx from "@/test/fixtures";
import { renderApp } from "@/test/render";

import { bisyncOf, emptyBisync } from "./bisync-definition";
import { emptyDefinition } from "./definition";
import { emptySchedule, validateSchedule } from "../schedules/model";

const now = Date.now();

// These tests drive whole pages through the router, which is slow under a
// parallel run.
describe("two-way sync pages", { timeout: 30_000 }, () => {
  it("offers Resync now after RESYNC_REQUIRED and runs a resync on the same worker", async () => {
    const user = userEvent.setup();
    const api = bisyncApi(now);
    // The newest: the new pair's first run, whose folder was empty.
    const failed = api.failed(JobErrorCode.RESYNC_REQUIRED);
    expect(failed.error?.message).toBe(bisyncMock.messages.emptyFolder);
    const { router } = renderApp(`/jobs/${failed.id}`, api.transport);

    expect(await screen.findByText("Resync required")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Run once anyway" })).not.toBeInTheDocument();
    await user.click(await screen.findByRole("button", { name: "Resync now" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(dialog).toHaveTextContent("nothing is deleted");
    expect(dialog).toHaveTextContent("the newer copy is kept");
    expect(dialog).toHaveTextContent("on backup-host with this job's settings");
    expect(dialog).toHaveTextContent(
      "If a folder is on a drive or a mount point, make sure it is mounted first",
    );
    await user.click(within(dialog).getByRole("button", { name: "Resync now" }));

    await waitFor(() => expect(api.of<RunJobRequest>("RunJob")).toHaveLength(1));
    const req = api.of<RunJobRequest>("RunJob")[0];
    expect(req.workerId).toBe(fx.ids.backupHost);
    expect(req.definition?.operation).toMatchObject({
      case: "rcloneBisync",
      value: { resync: true, force: false, path1: { path: bisyncMock.NEW_PATH } },
    });
    await waitFor(() => expect(router.state.location.pathname).toBe(`/jobs/${api.jobs[0].id}`));
  });

  it("offers no action on a run that a newer run of the pair superseded", async () => {
    const api = bisyncApi(now);
    // The run that needed a resync after the filters changed: the resync
    // and later runs followed it.
    const old = api.jobs.find(
      (j) =>
        j.error?.code === JobErrorCode.RESYNC_REQUIRED && j.scheduleId === bisyncMock.ids.schedule,
    )!;
    renderApp(`/jobs/${old.id}`, api.transport);
    expect(
      await screen.findByText(/A newer run of this pair exists on this worker/),
    ).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "open it" })).toHaveAttribute(
      "href",
      `/jobs/${api.schedule.lastRun?.jobId}`,
    );
    expect(screen.queryByRole("button", { name: "Resync now" })).not.toBeInTheDocument();
  });

  it("sends the job page of a run whose schedule changed to the schedule", async () => {
    const api = bisyncApi(now);
    const newest = api.failed(JobErrorCode.SAFETY_ABORT);
    expect(newest.id).toBe(api.schedule.lastRun?.jobId);
    bisyncOf(api.schedule.definition)!.options!.filters.push(
      create(FilterRuleSchema, { action: FilterAction.EXCLUDE, pattern: "/photos/**" }),
    );
    renderApp(`/jobs/${newest.id}`, api.transport);
    expect(
      await screen.findByText(/Its schedule has other settings or another worker/),
    ).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "the schedule's page" })).toHaveAttribute(
      "href",
      `/schedules/${bisyncMock.ids.schedule}`,
    );
    expect(screen.queryByRole("button", { name: "Run once anyway" })).not.toBeInTheDocument();
  });

  it("offers Run once anyway after SAFETY_ABORT, repeating the worker's counts", async () => {
    const user = userEvent.setup();
    const api = bisyncApi(now);
    const failed = api.failed(JobErrorCode.SAFETY_ABORT);
    renderApp(`/jobs/${failed.id}`, api.transport);

    expect(await screen.findByText("Safety check stopped the sync")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Resync now" })).not.toBeInTheDocument();
    await user.click(await screen.findByRole("button", { name: "Run once anyway" }));
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
    // The result card has the count; no Summary repeats it.
    expect(screen.getByRole("heading", { name: "Two-way sync result" })).toBeInTheDocument();
    expect(screen.queryByRole("heading", { name: "Summary" })).not.toBeInTheDocument();
  });

  it("words a dry run's resync as what it would do", async () => {
    const api = bisyncApi(now);
    const first = api.jobs.find(
      (j) =>
        j.output?.output.case === "rcloneBisync" &&
        j.output.output.value.resyncReason === BisyncResyncReason.FIRST_RUN,
    )!;
    if (first.output?.output.case === "rcloneBisync") first.output.output.value.dryRun = true;
    renderApp(`/jobs/${first.id}`, api.transport);
    expect(
      await screen.findByText(
        /As a dry run, it only found what a resync would do: merge both folders/,
      ),
    ).toBeInTheDocument();
    expect(screen.queryByText(/It merged both folders/)).not.toBeInTheDocument();
  });

  it("does not wait for a total on a finished job", async () => {
    const api = bisyncApi(now);
    renderApp(`/jobs/${api.failed(JobErrorCode.SIDE_UNAVAILABLE).id}`, api.transport);
    expect(await screen.findByText("A folder looked unavailable")).toBeInTheDocument();
    expect(screen.queryByText("Total not known yet")).not.toBeInTheDocument();
  });

  it("offers the same actions on the schedule page for its last run", async () => {
    const user = userEvent.setup();
    const api = bisyncApi(now);
    renderApp(`/schedules/${bisyncMock.ids.schedule}`, api.transport);
    expect(await screen.findByText("Last run: Safety check stopped the sync")).toBeInTheDocument();
    await user.click(await screen.findByRole("button", { name: "Run once anyway" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(dialog).toHaveTextContent("on nas-01 with the schedule's current settings");
    expect(dialog).toHaveTextContent("731 of 1268");
    await user.click(within(dialog).getByRole("button", { name: "Run once anyway" }));
    await waitFor(() => expect(api.of<RunJobRequest>("RunJob")).toHaveLength(1));
    expect(api.of<RunJobRequest>("RunJob")[0]).toMatchObject({
      workerId: fx.ids.nas,
      definition: { operation: { value: { force: true } } },
    });
  });

  it("resyncs the schedule's current settings on its current worker from the schedule page", async () => {
    const user = userEvent.setup();
    const api = bisyncApi(now);
    // The last run needed a resync; since then the schedule got a filter
    // and moved to backup-host.
    const last = api.jobs.find((j) => j.id === api.schedule.lastRun?.jobId)!;
    last.error = create(JobErrorSchema, {
      code: JobErrorCode.RESYNC_REQUIRED,
      message: bisyncMock.messages.resyncRequired,
    });
    bisyncOf(api.schedule.definition)!.options!.filters.push(
      create(FilterRuleSchema, { action: FilterAction.EXCLUDE, pattern: "/photos/**" }),
    );
    api.schedule.target = create(ScheduleTargetSchema, {
      target: { case: "workerId", value: fx.ids.backupHost },
    });
    renderApp(`/schedules/${bisyncMock.ids.schedule}`, api.transport);
    expect(await screen.findByText("Last run: Resync required")).toBeInTheDocument();
    await user.click(await screen.findByRole("button", { name: "Resync now" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(dialog).toHaveTextContent("on backup-host with the schedule's current settings");
    expect(dialog).toHaveTextContent("make sure it is mounted first");
    await user.click(within(dialog).getByRole("button", { name: "Resync now" }));
    await waitFor(() => expect(api.of<RunJobRequest>("RunJob")).toHaveLength(1));
    const req = api.of<RunJobRequest>("RunJob")[0];
    expect(req.workerId).toBe(fx.ids.backupHost);
    const b = req.definition?.operation;
    expect(b).toMatchObject({ case: "rcloneBisync", value: { resync: true, force: false } });
    expect(
      b?.case === "rcloneBisync" ? b.value.options?.filters?.map((f) => f.pattern) : [],
    ).toEqual(["*.tmp", ".~lock.*#", "/photos/**"]);
  });

  it("does not run past the safety check on the schedule page after its settings changed", async () => {
    const api = bisyncApi(now);
    bisyncOf(api.schedule.definition)!.options!.filters.push(
      create(FilterRuleSchema, { action: FilterAction.EXCLUDE, pattern: "/photos/**" }),
    );
    renderApp(`/schedules/${bisyncMock.ids.schedule}`, api.transport);
    expect(await screen.findByText("Last run: Safety check stopped the sync")).toBeInTheDocument();
    expect(
      await screen.findByText(/The schedule's settings changed after this run/),
    ).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "Run once anyway" })).not.toBeInTheDocument();
  });

  it("does not offer the schedule's actions again once a newer run of the pair exists", async () => {
    const api = bisyncApi(now);
    const last = api.jobs.find((j) => j.id === api.schedule.lastRun?.jobId)!;
    api.jobs.unshift(
      create(JobSchema, {
        ...last,
        id: "job-forced",
        createdAt: timestampFromMs(now),
        definition: bisyncMock.definition("force"),
        status: JobStatus.QUEUED,
        error: undefined,
        scheduleId: "",
      }),
    );
    renderApp(`/schedules/${bisyncMock.ids.schedule}`, api.transport);
    expect(await screen.findByText(/A newer run of this pair exists/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "open it" })).toHaveAttribute(
      "href",
      "/jobs/job-forced",
    );
    expect(screen.queryByRole("button", { name: "Run once anyway" })).not.toBeInTheDocument();
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
    // Saved, the toast says where Resync now will be: the schedule's page
    // offers it once that run stopped.
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    expect(
      await screen.findByText(
        /its next run stops with “Resync required”. The schedule's page then offers Resync now/,
      ),
    ).toBeInTheDocument();
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
