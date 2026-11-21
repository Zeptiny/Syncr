import { create } from "@bufbuild/protobuf";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it } from "vitest";

import { JobService, WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import { DashboardService } from "@/gen/syncr/v1/dashboard_pb";
import {
  ScheduleGroupSchema,
  ScheduleService,
  type MoveSchedulesRequest,
  type ScheduleGroup,
} from "@/gen/syncr/v1/schedules_pb";
import { signedIn } from "@/test/app-api";
import * as fx from "@/test/fixtures";
import { mockTransport, renderApp } from "@/test/render";

const now = Date.now();

function api() {
  const hist = fx.history(now);
  let schedules = hist.schedules;
  const groups: ScheduleGroup[] = [...hist.groups];
  const calls = {
    enabled: [] as [string, boolean][],
    moves: [] as MoveSchedulesRequest[],
    created: [] as string[],
    deletedGroups: [] as string[],
    deleted: [] as string[],
  };
  const transport = mockTransport((router) => {
    signedIn(router, now);
    router.service(WorkerAdminService, { listWorkers: () => ({ workers: fx.workers(now) }) });
    router.service(JobService, { listJobs: () => ({ jobs: [] }) });
    router.service(DashboardService, {});
    router.service(ScheduleService, {
      listSchedules: () => ({ schedules }),
      listScheduleGroups: () => ({ groups }),
      setScheduleEnabled: (req) => {
        calls.enabled.push([req.scheduleId, req.enabled]);
        schedules = schedules.map((s) =>
          s.id === req.scheduleId ? { ...s, enabled: req.enabled } : s,
        );
        return { schedule: schedules.find((s) => s.id === req.scheduleId) };
      },
      moveSchedules: (req) => {
        calls.moves.push(req);
        schedules = schedules.map((s) =>
          req.scheduleIds.includes(s.id) ? { ...s, groupId: req.groupId } : s,
        );
        return { schedules: schedules.filter((s) => req.scheduleIds.includes(s.id)) };
      },
      createScheduleGroup: (req) => {
        calls.created.push(req.name);
        const g = create(ScheduleGroupSchema, { id: `g-${req.name}`, name: req.name });
        groups.push(g);
        return { group: g };
      },
      deleteScheduleGroup: (req) => {
        calls.deletedGroups.push(req.groupId);
        groups.splice(
          groups.findIndex((g) => g.id === req.groupId),
          1,
        );
        schedules = schedules.map((s) => (s.groupId === req.groupId ? { ...s, groupId: "" } : s));
        return {};
      },
      deleteSchedule: (req) => {
        calls.deleted.push(req.scheduleId);
        schedules = schedules.filter((s) => s.id !== req.scheduleId);
        return {};
      },
    });
  });
  return { transport, calls, hist };
}

/** sectionNames lists the table's section headings, in order. */
function sectionNames(table: HTMLElement): string[] {
  return within(table)
    .queryAllByRole("rowheader")
    .map((h) => within(h).getAllByRole("button")[0].textContent ?? "");
}

/** rowNames lists the schedules on the table, in order. */
function rowNames(table: HTMLElement): string[] {
  return within(table)
    .getAllByRole("checkbox", { name: /^Select (?!every)/ })
    .map((c) => c.getAttribute("aria-label")!.replace(/^Select /, ""));
}

describe("schedules page", { timeout: 30_000 }, () => {
  beforeEach(() => window.localStorage.clear());

  it("lists schedules in their groups, no group last, and remembers collapsed groups", async () => {
    const user = userEvent.setup();
    const { transport } = api();
    renderApp("/schedules", transport);
    const table = await screen.findByRole("table", { name: "Schedules" });
    await waitFor(() => expect(sectionNames(table)).toEqual(["Offsite", "Photos", "No group"]));
    expect(rowNames(table)).toEqual([
      "Offsite mirror",
      "Weekly archive",
      "Nightly photos → B2",
      "Laptop projects",
      "Office docs hourly",
    ]);
    // What each one runs, under its name.
    expect(within(table).getByRole("row", { name: "Nightly photos → B2" })).toHaveTextContent(
      "B2 offsite",
    );

    await user.click(within(table).getByRole("button", { name: /^Offsite/ }));
    expect(rowNames(table)).not.toContain("Offsite mirror");
    expect(JSON.parse(window.localStorage.getItem("syncr-schedule-sections")!)).toEqual({
      group: [fx.ids.groupOffsite],
    });
  });

  it("filters by search and status, and groups by worker", async () => {
    const user = userEvent.setup();
    const { transport } = api();
    const { router } = renderApp("/schedules", transport);
    const table = await screen.findByRole("table", { name: "Schedules" });
    await waitFor(() => expect(rowNames(table)).toHaveLength(5));

    // The storage box is the remote of two of them.
    await user.type(screen.getByRole("searchbox", { name: /Search/ }), "storage box");
    await waitFor(() => expect(rowNames(table)).toEqual(["Offsite mirror", "Weekly archive"]));
    expect(router.state.location.search).toMatchObject({ q: "storage box" });
    await user.click(screen.getByRole("button", { name: "Clear filters" }));
    await waitFor(() => expect(rowNames(table)).toHaveLength(5));

    // The status chips count every schedule; one was paused by Syncr.
    const paused = screen.getByRole("button", { name: /^Paused/ });
    expect(paused).toHaveTextContent("1");
    await user.click(screen.getByRole("button", { name: /^Needs attention/ }));
    await waitFor(() => expect(rowNames(table)).toContain("Laptop projects"));
    expect(rowNames(table).every((n) => n !== "Weekly archive")).toBe(true);
    await user.click(screen.getByRole("button", { name: /^All/ }));

    await user.click(screen.getByRole("combobox", { name: "Group by" }));
    await user.click(screen.getByRole("option", { name: "Worker" }));
    await waitFor(() => expect(sectionNames(table)[0]).not.toBe("Offsite"));
    expect(sectionNames(table)).toContain("Workers labelled nas");
    expect(router.state.location.search).toMatchObject({ by: "target" });
  });

  it("pauses and moves a selection, and creates a group for it", async () => {
    const user = userEvent.setup();
    const { transport, calls } = api();
    renderApp("/schedules", transport);
    const table = await screen.findByRole("table", { name: "Schedules" });
    await waitFor(() => expect(rowNames(table)).toHaveLength(5));

    await user.click(within(table).getByRole("checkbox", { name: "Select Offsite mirror" }));
    await user.click(within(table).getByRole("checkbox", { name: "Select Office docs hourly" }));
    const bar = screen.getByRole("region", { name: "Selected schedules" });
    expect(bar).toHaveTextContent("2 selected");

    await user.click(within(bar).getByRole("button", { name: "Pause" }));
    await waitFor(() => expect(calls.enabled).toHaveLength(2));
    expect(calls.enabled.every(([, on]) => !on)).toBe(true);
    await screen.findByText("Paused 2 schedules");

    await user.click(within(bar).getByRole("button", { name: "Move to group" }));
    await user.click(await screen.findByRole("menuitem", { name: "Photos" }));
    await waitFor(() => expect(calls.moves).toHaveLength(1));
    expect(calls.moves[0].groupId).toBe(fx.ids.groupPhotos);
    expect([...calls.moves[0].scheduleIds].sort()).toEqual(
      [fx.ids.offsite, fx.ids.officeHourly].sort(),
    );
    await screen.findByText("Moved 2 schedules to Photos");

    await user.click(within(bar).getByRole("button", { name: "Move to group" }));
    await user.click(await screen.findByRole("menuitem", { name: "New group…" }));
    const dialog = await screen.findByRole("dialog");
    await user.type(within(dialog).getByLabelText("Name"), "Work");
    await user.click(within(dialog).getByRole("button", { name: "Create group" }));
    await waitFor(() => expect(calls.moves).toHaveLength(2));
    expect(calls.created).toEqual(["Work"]);
    expect(calls.moves[1].groupId).toBe("g-Work");
    await waitFor(() => expect(sectionNames(table)).toContain("Work"));
  });

  it("deletes a group after asking, keeping its schedules", async () => {
    const user = userEvent.setup();
    const { transport, calls } = api();
    renderApp("/schedules", transport);
    const table = await screen.findByRole("table", { name: "Schedules" });
    await waitFor(() => expect(sectionNames(table)).toContain("Offsite"));

    await user.click(within(table).getByRole("button", { name: "Actions for Offsite" }));
    await user.click(await screen.findByRole("menuitem", { name: "Delete group…" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(dialog).toHaveTextContent("Its 2 schedules stay, in no group, and run as before.");
    await user.click(within(dialog).getByRole("button", { name: "Delete group" }));
    await waitFor(() => expect(calls.deletedGroups).toEqual([fx.ids.groupOffsite]));
    await waitFor(() => expect(sectionNames(table)).toEqual(["Photos", "No group"]));
    expect(rowNames(table)).toContain("Offsite mirror");
  });
});
