import { createClient } from "@connectrpc/connect";
import { describe, expect, it } from "vitest";

import { JobService, WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import { AuthService } from "@/gen/syncr/v1/auth_pb";
import { DashboardRange, DashboardService } from "@/gen/syncr/v1/dashboard_pb";
import { ChannelService, ContactService, DeliveryStatus } from "@/gen/syncr/v1/notify_pb";
import { ScheduleRunStatus, ScheduleService } from "@/gen/syncr/v1/schedules_pb";
import * as fx from "@/test/fixtures";

import { createMockTransport } from "./transport";

// The dev mock backs the screenshots; every M4 call must serialize.
describe("mock transport, milestone 4", () => {
  it("serves schedules, contacts, deliveries, the dashboard and labels", async () => {
    const t = createMockTransport();
    await createClient(AuthService, t).login({ username: "admin", password: "syncr-demo-admin" });
    const sched = createClient(ScheduleService, t);
    const contacts = createClient(ContactService, t);

    const { schedules } = await sched.listSchedules({});
    expect(schedules.map((s) => s.name)).toEqual([
      "Archive 2019 yearly check",
      "Laptop projects",
      "Monthly deep check: photos",
      "Nightly photos → B2",
      "Nightly photos backup",
      "Office docs hourly",
      "Office documents backup",
      "Offsite mirror",
      "Old laptop weekly backup",
      "Weekly archive",
      "Weekly retention: photos",
    ]);
    const office = schedules.find((s) => s.id === fx.ids.officeHourly)!;
    expect(office.lastRun).toMatchObject({ status: ScheduleRunStatus.PENDING, coalesced: 2 });
    expect(schedules.find((s) => s.id === fx.ids.laptopSync)?.pausedReason).toBe("worker_removed");

    const runs = await sched.listScheduleRuns({ scheduleId: fx.ids.nightly, pageSize: 20 });
    expect(runs.runs.some((r) => r.status === ScheduleRunStatus.MISSED)).toBe(true);
    expect(runs.nextPageToken).not.toBe("");

    const preview = await sched.previewSchedule({ cron: "0 3 * * 0", timezone: "UTC", count: 5 });
    expect(preview.nextRuns).toHaveLength(5);
    await expect(sched.previewSchedule({ cron: "0 3 * *", timezone: "UTC" })).rejects.toThrow();

    expect((await createClient(ChannelService, t).listChannels({})).channels).toHaveLength(3);
    const list = await contacts.listContacts({});
    expect(list.contacts.find((c) => c.id === fx.ids.adminMail)?.usedBy.length).toBe(2);
    const dels = await contacts.listDeliveries({ pageSize: 500 });
    const statuses = new Set(dels.deliveries.map((d) => d.status));
    for (const s of [
      DeliveryStatus.PENDING,
      DeliveryStatus.SENDING,
      DeliveryStatus.DELIVERED,
      DeliveryStatus.DEAD,
    ])
      expect(statuses).toContain(s);

    for (const range of [DashboardRange.DAY, DashboardRange.WEEK, DashboardRange.MONTH]) {
      const d = await createClient(DashboardService, t).getDashboard({ range, timezone: "UTC" });
      expect(d.activity.length).toBe(
        range === DashboardRange.DAY ? 24 : range === DashboardRange.WEEK ? 7 : 30,
      );
    }

    const w = await createClient(WorkerAdminService, t).listWorkers({ label: "nas" });
    expect(w.workers.map((x) => x.name)).toEqual(["nas-01"]);
    const jobs = await createClient(JobService, t).listJobs({
      scheduleId: fx.ids.offsite,
      pageSize: 5,
    });
    expect(jobs.jobs.every((j) => j.schedule?.name === "Offsite mirror")).toBe(true);
  });
});
