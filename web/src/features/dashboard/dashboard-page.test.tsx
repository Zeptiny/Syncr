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
import { JobErrorCode } from "@/gen/syncr/v1/link_pb";
import { ScheduleRunStatus, ScheduleService } from "@/gen/syncr/v1/schedules_pb";
import { signedIn } from "@/test/app-api";
import * as fx from "@/test/fixtures";
import { durationFromSeconds } from "@/lib/format";
import { formatInZone } from "@/lib/timezones";
import { mockTransport, renderApp } from "@/test/render";

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
            runTime: durationFromSeconds(50 * 120),
            started: 50n,
            queueWait: durationFromSeconds(50 * 4),
            dispatched: 50n,
          },
          activity: [
            {
              start: timestampFromMs(from + 5 * HOUR),
              succeeded: 47n,
              failed: 2n,
              runTime: durationFromSeconds(50 * 120),
              started: 50n,
              queueWait: durationFromSeconds(50 * 4),
              dispatched: 50n,
            },
          ],
          kinds: [
            {
              key: "rclone_transfer",
              succeeded: 40n,
              failed: 2n,
              bytes: 4n * 1024n ** 3n,
              runTime: durationFromSeconds(42 * 60),
              started: 42n,
            },
            { key: "restic_backup", succeeded: 7n, interrupted: 1n },
          ],
          workerActivity: [
            { key: "w1", name: "nas-01", succeeded: 40n, failed: 2n },
            { key: "w2", name: "pi", succeeded: 7n, interrupted: 1n },
          ],
          failureReasons: [
            { code: JobErrorCode.EXECUTION_FAILED, failed: 2n, kinds: ["rclone_transfer"] },
            { code: JobErrorCode.WORKER_RESTARTED, interrupted: 1n, kinds: ["restic_backup"] },
          ],
          schedules: [
            {
              scheduleId: fx.ids.nightly,
              scheduleName: "Nightly photos → B2",
              kind: "rclone_transfer",
              enabled: true,
              latestRun: { status: ScheduleRunStatus.LAUNCHED, jobStatus: JobStatus.FAILED },
              lastSuccessAt: timestampFromMs(now - 26 * HOUR),
            },
            {
              scheduleId: "s2",
              scheduleName: "Weekly check",
              kind: "restic_check",
              enabled: false,
            },
          ],
          repositories: [
            {
              repositoryId: "r1",
              name: "photos",
              snapshotCount: 14,
              backups: 2n,
              addedBytes: 300n * 1024n ** 2n,
              firstSourceBytes: 10n * 1024n ** 3n,
              lastSourceBytes: 10n * 1024n ** 3n + 200n * 1024n ** 2n,
              added: [0n, 100n * 1024n ** 2n, 200n * 1024n ** 2n],
            },
          ],
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
    // Up next comes before the schedules' health, which lists it too.
    const next = screen.getAllByRole("link", { name: /Nightly photos → B2/ })[0];
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

  it("breaks the jobs down and shows the schedules and the repositories", async () => {
    const { transport } = api(true);
    renderApp("/", transport);
    // The stat cards; the run time and queue wait charts repeat the averages.
    expect(await screen.findByText("1h 40m over 50 jobs")).toBeInTheDocument();
    expect(screen.getAllByText("2m 00s")).toHaveLength(2);
    expect(screen.getAllByText("4s")).toHaveLength(2);
    expect(screen.getByText("Before a worker took the job")).toBeInTheDocument();

    const transfer = screen.getByRole("link", { name: /^Transfer: 42 jobs, 95\.2% succeeded/ });
    expect(transfer).toHaveAttribute("href", expect.stringContaining("kind=rclone_transfer"));
    expect(within(transfer).getByText(/ran 1m 00s on average/)).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /^nas-01: 42 jobs/ })).toHaveAttribute(
      "href",
      expect.stringContaining("worker=w1"),
    );
    // Rows open the finished jobs they count.
    const params = (a: Element | null) =>
      new URL(a?.getAttribute("href") ?? "", "http://ui").searchParams;
    expect(params(transfer).get("status")).toBe(
      [JobStatus.SUCCEEDED, JobStatus.FAILED, JobStatus.INTERRUPTED, JobStatus.CANCELLED].join(","),
    );
    // A cause opens every failed and interrupted job of its kind, and says so.
    const failed = screen.getByText("Execution failed").closest("a");
    expect(params(failed).get("status")).toBe(`${JobStatus.FAILED},${JobStatus.INTERRUPTED}`);
    expect(params(failed).get("kind")).toBe("rclone_transfer");
    expect(failed).toHaveAttribute(
      "title",
      "Opens every failed and interrupted Transfer job of the period, whatever the cause.",
    );
    expect(screen.getByText("Worker restarted")).toBeInTheDocument();

    const schedules = screen.getByRole("region", { name: "Schedules" });
    expect(within(schedules).getByText("of 2 healthy")).toBeInTheDocument();
    expect(within(schedules).getByText("1 need attention")).toBeInTheDocument();
    expect(within(schedules).getByText("3 runs missed in the last 24 hours")).toBeInTheDocument();
    expect(within(schedules).getByText(/last succeeded/)).toBeInTheDocument();
    expect(within(schedules).getByText("Failed")).toBeInTheDocument();
    expect(within(schedules).getByText(/never succeeded/)).toBeInTheDocument();

    const repos = screen.getByRole("region", { name: "Repository growth" });
    expect(within(repos).getByText("300 MiB added · 2 backups")).toBeInTheDocument();
    expect(within(repos).getByText("Source 10.2 GiB, +200 MiB")).toBeInTheDocument();
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
