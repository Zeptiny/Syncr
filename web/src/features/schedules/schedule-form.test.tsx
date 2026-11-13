import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import { ValidationErrorSchema, WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import { ContactService, NotificationEvent } from "@/gen/syncr/v1/notify_pb";
import { BackendService, RemoteService } from "@/gen/syncr/v1/remotes_pb";
import {
  MissedRunAction,
  OverlapPolicy,
  ScheduleSchema,
  ScheduleService,
  type ScheduleInput,
} from "@/gen/syncr/v1/schedules_pb";
import { signedIn } from "@/test/app-api";
import * as fx from "@/test/fixtures";
import { mockTransport, renderApp } from "@/test/render";

const now = Date.now();

function api(opts: { reject?: { field: string; message: string } } = {}) {
  const hist = fx.history(now);
  const created: ScheduleInput[] = [];
  const updated: { id: string; version: bigint; input?: ScheduleInput }[] = [];
  const transport = mockTransport((router) => {
    signedIn(router, now);
    router.service(WorkerAdminService, { listWorkers: () => ({ workers: fx.workers(now) }) });
    router.service(RemoteService, { listRemotes: () => ({ remotes: fx.remotes(now) }) });
    router.service(BackendService, { listBackends: () => ({ backends: fx.backends }) });
    router.service(ContactService, { listContacts: () => ({ contacts: fx.contacts(now) }) });
    router.service(ScheduleService, {
      previewSchedule: () => ({ nextRuns: [timestampFromMs(now + 3_600_000)] }),
      listScheduleRuns: () => ({ runs: [] }),
      getSchedule: (req) => ({
        schedule:
          hist.schedules.find((s) => s.id === req.scheduleId) ??
          create(ScheduleSchema, {
            id: req.scheduleId,
            name: "New",
            cron: "30 2 * * *",
            timezone: "UTC",
          }),
      }),
      createSchedule: (req) => {
        if (opts.reject) {
          throw new ConnectError("invalid", Code.InvalidArgument, undefined, [
            { desc: ValidationErrorSchema, value: { violations: [opts.reject] } },
          ]);
        }
        if (req.input) created.push(req.input);
        return { schedule: create(ScheduleSchema, { id: "s-new", name: req.input?.name }) };
      },
      updateSchedule: (req) => {
        updated.push({ id: req.scheduleId, version: req.version, input: req.input });
        return { schedule: hist.schedules.find((s) => s.id === req.scheduleId) };
      },
    });
  });
  return { transport, created, updated };
}

// These tests drive the whole form through the router, which is slow under
// a parallel run.
describe("schedule form", { timeout: 30_000 }, () => {
  it("creates a schedule on a label, with policies and a contact", async () => {
    const user = userEvent.setup();
    const { transport, created } = api();
    const { router } = renderApp("/schedules/new", transport);
    await user.type(await screen.findByLabelText("Name"), "Photos nightly");

    await user.click(screen.getByRole("radio", { name: "Any worker with a label" }));
    await user.type(screen.getByLabelText("Label"), "gpu");
    expect(screen.getByRole("status")).toHaveTextContent("No worker has the label gpu yet");
    await user.clear(screen.getByLabelText("Label"));
    await user.click(screen.getByRole("button", { name: "nas" }));
    expect(screen.getByRole("status")).toHaveTextContent("1 worker has this label: nas-01");

    await user.type(document.getElementById("sched-src-path")!, "/srv/photos");
    await user.type(document.getElementById("sched-dst-path")!, "/srv/backup/photos");

    await user.click(screen.getByRole("radio", { name: /Skip it and notify/ }));
    await user.click(screen.getByRole("radio", { name: /Run both/ }));

    await user.click(screen.getByRole("combobox", { name: "Notify a contact" }));
    await user.click(screen.getByRole("option", { name: "Ops Discord" }));
    await user.click(screen.getByRole("checkbox", { name: "Succeeded" }));

    await user.click(screen.getByRole("button", { name: "Create schedule" }));
    await waitFor(() => expect(created).toHaveLength(1));
    const input = created[0];
    expect(input.name).toBe("Photos nightly");
    expect(input.cron).toBe("30 2 * * *");
    expect(input.timezone).not.toBe("");
    expect(input.target?.target).toEqual({ case: "workerLabel", value: "nas" });
    expect(input.missedRun?.action).toBe(MissedRunAction.SKIP);
    expect(input.missedRun?.maxDelay).toBeUndefined();
    expect(input.overlap).toBe(OverlapPolicy.ALLOW);
    expect(input.enabled).toBe(true);
    expect(input.contacts.map((c) => [c.contactId, c.events])).toEqual([
      [
        fx.ids.opsDiscord,
        [
          NotificationEvent.SUCCEEDED,
          NotificationEvent.FAILED,
          NotificationEvent.INTERRUPTED,
          NotificationEvent.MISSED,
        ],
      ],
    ]);
    const op = input.definition?.operation;
    expect(op?.case === "rcloneTransfer" && op.value.source?.path).toBe("/srv/photos");
    await waitFor(() => expect(router.state.location.pathname).toBe("/schedules/s-new"));
  });

  it("runs late within a chosen window on a specific worker by default", async () => {
    const user = userEvent.setup();
    const { transport } = api();
    renderApp("/schedules/new", transport);
    await user.type(await screen.findByLabelText("Name"), "x");
    await user.click(screen.getByRole("button", { name: "Create schedule" }));
    expect(await screen.findByText("Choose the worker that runs the jobs.")).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /Run it when the worker is back/ })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    expect(screen.getByRole("combobox", { name: "Give up after" })).toHaveTextContent("6 hours");
  });

  it("asks for the worker before the folders, then browses it and warns about paths outside its folders", async () => {
    const user = userEvent.setup();
    const { transport } = api();
    renderApp("/schedules/new", transport);
    const from = await screen.findByRole("group", { name: "From" });
    const path = within(from).getByLabelText("Path");
    const browse = () => within(from).queryByRole("button", { name: /^Browse/ });
    // The worker comes first, so its folders can be browsed while filling the paths.
    expect(
      screen.getByRole("radio", { name: "A specific worker" }).compareDocumentPosition(path) &
        Node.DOCUMENT_POSITION_FOLLOWING,
    ).toBeTruthy();
    expect(browse()).toBeNull();
    expect(path).toHaveAccessibleDescription(
      "An absolute path on the worker, inside its allowed folders. Choose a worker to browse.",
    );

    await user.click(screen.getByRole("combobox", { name: "Worker" }));
    await user.click(await screen.findByRole("option", { name: /office-pc/ }));
    expect(browse()).toBeNull();
    expect(path).toHaveAccessibleDescription(
      "office-pc is offline, so its folders cannot be browsed now. Type the path.",
    );

    await user.click(screen.getByRole("combobox", { name: "Worker" }));
    await user.click(await screen.findByRole("option", { name: /nas-01/ }));
    expect(browse()).toBeInTheDocument();
    await user.type(path, "/srv/data");
    expect(path).toHaveAccessibleDescription(
      "An absolute path on nas-01, inside its allowed folders. This path is outside nas-01's allowed folders (/srv/photos, /mnt/media, /srv/backup). The worker refuses it, so the job fails.",
    );
    await user.clear(path);
    await user.type(path, "/srv/photos/2024");
    expect(within(from).queryByText(/outside nas-01's allowed folders/)).toBeNull();
  });

  it("browses a label's first online worker for a transfer", async () => {
    const user = userEvent.setup();
    const { transport } = api();
    renderApp("/schedules/new", transport);
    const from = await screen.findByRole("group", { name: "From" });
    await user.click(screen.getByRole("radio", { name: "Any worker with a label" }));
    await user.type(screen.getByLabelText("Label"), "nas");
    expect(within(from).getByRole("button", { name: /^Browse/ })).toBeInTheDocument();
    expect(
      within(from).getByText(
        "Absolute paths on the workers labelled nas. Browse shows nas-01, the first one online.",
      ),
    ).toBeInTheDocument();

    await user.clear(screen.getByLabelText("Label"));
    await user.type(screen.getByLabelText("Label"), "desktop");
    expect(within(from).queryByRole("button", { name: /^Browse/ })).toBeNull();
    expect(within(from).getByText(/^No worker labelled desktop is online/)).toBeInTheDocument();
    await user.type(within(from).getByLabelText("Path"), "/srv/data");
    expect(within(from).getByText(/outside office-pc's allowed folders/)).toBeInTheDocument();
  });

  it("shows server errors on their fields", async () => {
    const user = userEvent.setup();
    const { transport } = api({
      reject: { field: "target.worker_label", message: "That label is reserved." },
    });
    renderApp("/schedules/new", transport);
    await user.type(await screen.findByLabelText("Name"), "x");
    await user.click(screen.getByRole("radio", { name: "Any worker with a label" }));
    await user.type(screen.getByLabelText("Label"), "nas");
    await user.type(document.getElementById("sched-src-path")!, "/a");
    await user.type(document.getElementById("sched-dst-path")!, "/b");
    await user.click(screen.getByRole("button", { name: "Create schedule" }));
    expect(await screen.findByText("That label is reserved.")).toBeInTheDocument();
  });

  it("edits a stored schedule with its version", async () => {
    const user = userEvent.setup();
    const { transport, updated } = api();
    renderApp(`/schedules/${fx.ids.nightly}/edit`, transport);
    const name = await screen.findByLabelText("Name");
    expect(name).toHaveValue("Nightly photos → B2");
    expect(screen.getByLabelText("Label")).toHaveValue("nas");
    const policy = screen.getByRole("radiogroup", { name: "If the last run is still going" });
    expect(within(policy).getByRole("radio", { name: /Skip it/ })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    await user.click(screen.getByRole("radio", { name: /Wait, then run/ }));
    await user.click(screen.getByRole("button", { name: "Save changes" }));
    await waitFor(() => expect(updated).toHaveLength(1));
    expect(updated[0]).toMatchObject({ id: fx.ids.nightly, version: 3n });
    expect(updated[0].input?.overlap).toBe(OverlapPolicy.QUEUE);
    expect(updated[0].input?.missedRun?.action).toBe(MissedRunAction.RUN_LATE);
    expect(Number(updated[0].input?.missedRun?.maxDelay?.seconds)).toBe(6 * 3600);
  });
});
