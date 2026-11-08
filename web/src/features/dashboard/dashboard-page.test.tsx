import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import {
  DashboardRange,
  DashboardService,
  type GetDashboardRequest,
} from "@/gen/syncr/v1/dashboard_pb";
import { JobStatus, WorkerStatus } from "@/gen/syncr/v1/api_pb";
import { ScheduleRunStatus, ScheduleService } from "@/gen/syncr/v1/schedules_pb";
import { signedIn } from "@/test/app-api";
import * as fx from "@/test/fixtures";
import { formatInZone } from "@/lib/timezones";
import { mockTransport, renderApp } from "@/test/render";

// uPlot needs a canvas, which jsdom lacks; the charts' data shaping is
// tested on its own.
vi.mock("uplot", () => ({
  default: class {
    static paths = { bars: () => () => null };
    setData() {}
    setSize() {}
    destroy() {}
  },
}));

const now = Date.now();
const HOUR = 3_600_000;

function api(full: boolean) {
  const requests: GetDashboardRequest[] = [];
  const hist = fx.history(now);
  const transport = mockTransport((router) => {
    signedIn(router, now);
    router.service(ScheduleService, {
      listSchedules: () => ({ schedules: full ? hist.schedules : [] }),
    });
    router.service(DashboardService, {
      getDashboard: (req) => {
        requests.push(req);
        const from = Math.floor(now / HOUR) * HOUR - 23 * HOUR;
        if (!full) {
          return {
            from: timestampFromMs(from),
            until: timestampFromMs(from + 24 * HOUR),
            bucketUnit: "hour",
            totals: {},
            activity: [],
            workers: { online: 0, offline: 0, pending: 0, workers: [] },
          };
        }
        return {
          from: timestampFromMs(from),
          until: timestampFromMs(from + 24 * HOUR),
          bucketUnit: "hour",
          totals: {
            succeeded: 47n,
            failed: 2n,
            interrupted: 1n,
            bytes: 5n * 1024n ** 3n,
            files: 1200n,
            successRate: 0.94,
            running: 1n,
            queued: 2n,
            missedRuns: 3n,
          },
          activity: [{ start: timestampFromMs(from + 5 * HOUR), succeeded: 47n, failed: 2n }],
          upcoming: [
            {
              scheduleId: fx.ids.nightly,
              scheduleName: "Nightly photos → B2",
              at: timestampFromMs(now + 3 * HOUR),
              target: { target: { case: "workerLabel", value: "nas" } },
              workerName: "nas-01",
              workerStatus: WorkerStatus.ONLINE,
            },
          ],
          workers: { online: 2, offline: 1, pending: 1, workers: fx.workers(now) },
          recentFailures: fx
            .jobs(now)
            .filter((j) => j.error && j.status === JobStatus.FAILED)
            .slice(0, 2),
          recentMissed: hist.runs.filter((r) => r.status === ScheduleRunStatus.MISSED).slice(0, 2),
        };
      },
    });
  });
  return { transport, requests };
}

describe("DashboardPage", () => {
  it("explains every empty panel", async () => {
    const { transport } = api(false);
    renderApp("/", transport);
    expect(await screen.findByText("No jobs finished in this period.")).toBeInTheDocument();
    expect(screen.getByText("Nothing was transferred in this period.")).toBeInTheDocument();
    expect(screen.getByText(/No runs are scheduled/)).toBeInTheDocument();
    expect(screen.getByText(/No workers yet/)).toBeInTheDocument();
    expect(screen.getByText("Nothing failed or was missed lately.")).toBeInTheDocument();
    expect(screen.getByText("No finished jobs")).toBeInTheDocument();
  });

  it("shows the totals, the next runs and what needs attention", async () => {
    const { transport } = api(true);
    renderApp("/", transport);
    expect(await screen.findByText("94.0%")).toBeInTheDocument();
    expect(screen.getByText("47 of 50 finished jobs")).toBeInTheDocument();
    expect(screen.getByText("5.00 GiB")).toBeInTheDocument();
    expect(screen.getByText("2 failed, 1 interrupted")).toBeInTheDocument();
    const next = screen.getByRole("link", { name: /Nightly photos → B2/ });
    expect(within(next).getByText("nas")).toBeInTheDocument();
    // In the schedule's zone, as on the schedules pages.
    const when = await within(next).findByText(
      formatInZone(new Date(now + 3 * HOUR), "Europe/Berlin", false),
    );
    expect(when).toHaveAttribute(
      "title",
      expect.stringMatching(/^In Europe \/ Berlin\. Your time: /),
    );
    expect(screen.getByText("of 4 online")).toBeInTheDocument();
    expect(screen.getAllByText("Missed").length).toBeGreaterThan(0);
    expect(screen.getByRole("img", { name: /47 succeeded, 2 failed/ })).toBeInTheDocument();
  });

  it("switches the range and, for admins, all users", async () => {
    const user = userEvent.setup();
    const { transport, requests } = api(true);
    renderApp("/", transport);
    await screen.findByText("94.0%");
    expect(requests.at(-1)).toMatchObject({ range: DashboardRange.DAY, allUsers: false });
    expect(requests.at(-1)?.timezone).not.toBe("");
    await user.click(screen.getByRole("radio", { name: "30 days" }));
    await vi.waitFor(() => expect(requests.at(-1)?.range).toBe(DashboardRange.MONTH));
    await user.click(screen.getByRole("switch"));
    await vi.waitFor(() => expect(requests.at(-1)?.allUsers).toBe(true));
  });
});
