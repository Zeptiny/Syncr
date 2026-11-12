/**
 * The tables render each cell through its column definition. A column
 * list that is rebuilt (a memo whose dependencies change) gives every
 * cell a new component, so React unmounts and mounts all of them: focus
 * and text selection inside the rows are lost and open menus close. These
 * tests check that what happens on each page all the time (the clock
 * ticking, a mutation's state changing) leaves the cells mounted.
 */
import { act, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import { JobService, WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import { DashboardService } from "@/gen/syncr/v1/dashboard_pb";
import { ChannelService, ContactService, DeliveryStatus } from "@/gen/syncr/v1/notify_pb";
import { RemoteService } from "@/gen/syncr/v1/remotes_pb";
import { ScheduleService } from "@/gen/syncr/v1/schedules_pb";
import { UserService } from "@/gen/syncr/v1/users_pb";
import { signedIn } from "@/test/app-api";
import * as fx from "@/test/fixtures";
import { mockTransport, renderApp } from "@/test/render";

const now = Date.now();

/** cells returns the root element of every body cell of a table. */
function cells(table: HTMLElement): Element[] {
  return [...table.querySelectorAll("tbody td > *")];
}

/**
 * remounted counts the cells of a table that are not the nodes they were
 * in before (by identity: their content may have changed).
 */
function remounted(table: HTMLElement, before: Element[]): number {
  const after = cells(table);
  expect(after).toHaveLength(before.length);
  return after.filter((node, i) => node !== before[i]).length;
}

describe("table cells stay mounted", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("on the jobs page while the clock ticks", async () => {
    const hist = fx.history(now);
    const transport = mockTransport((router) => {
      signedIn(router, now);
      router.service(WorkerAdminService, { listWorkers: () => ({ workers: fx.workers(now) }) });
      router.service(RemoteService, { listRemotes: () => ({ remotes: fx.remotes(now) }) });
      router.service(ScheduleService, { listSchedules: () => ({ schedules: hist.schedules }) });
      router.service(UserService, { listUsers: () => ({ users: fx.users(now) }) });
      router.service(JobService, { listJobs: () => ({ jobs: hist.jobs.slice(0, 5) }) });
    });
    vi.useFakeTimers({ shouldAdvanceTime: true });
    renderApp("/jobs", transport);
    const table = await screen.findByRole("table", { name: "Jobs" });
    await waitFor(() => expect(table.querySelectorAll("tbody td time").length).toBeGreaterThan(0));
    const before = cells(table);
    const link = table.querySelector<HTMLAnchorElement>("tbody td a");
    expect(link).not.toBeNull();
    link?.focus();

    // Past the Duration column's 5 s tick and the list's 5 s poll.
    await act(async () => {
      await vi.advanceTimersByTimeAsync(11_000);
    });
    expect(before.length).toBeGreaterThan(0);
    expect(remounted(table, before)).toBe(0);
    expect(document.activeElement).toBe(link);
  });

  it("on the schedules page when a schedule is paused", async () => {
    let schedules = fx.history(now).schedules;
    const transport = mockTransport((router) => {
      signedIn(router, now);
      router.service(WorkerAdminService, { listWorkers: () => ({ workers: fx.workers(now) }) });
      router.service(JobService, { listJobs: () => ({ jobs: [] }) });
      router.service(DashboardService, {});
      router.service(ScheduleService, {
        listSchedules: () => ({ schedules }),
        setScheduleEnabled: (req) => {
          schedules = schedules.map((s) =>
            s.id === req.scheduleId ? { ...s, enabled: req.enabled } : s,
          );
          return { schedule: schedules.find((s) => s.id === req.scheduleId) };
        },
      });
    });
    renderApp("/schedules", transport);
    const table = await screen.findByRole("table", { name: "Schedules" });
    await waitFor(() =>
      expect(table.querySelectorAll("tbody [role=switch]").length).toBeGreaterThan(1),
    );
    const before = cells(table);
    const toggle = table.querySelector<HTMLButtonElement>("tbody [role=switch]")!;
    const wasOn = toggle.getAttribute("aria-checked");

    const user = userEvent.setup();
    toggle.focus();
    await user.keyboard(" ");
    await screen.findByText(/^(Paused|Resumed) Nightly/);
    await waitFor(() => expect(toggle.getAttribute("aria-checked")).not.toBe(wasOn));
    // Only the toggled schedule's next run is another element (a paused
    // schedule shows why it is paused instead of a time).
    expect(remounted(table, before)).toBeLessThanOrEqual(1);
    expect(document.contains(toggle)).toBe(true);
    expect(document.activeElement).toBe(toggle);
  });

  it("in the delivery log while a retry is sent", async () => {
    const deliveries = fx.deliveries(now, fx.history(now));
    expect(deliveries.some((d) => d.status === DeliveryStatus.DEAD)).toBe(true);
    let release: (() => void) | undefined;
    const transport = mockTransport((router) => {
      signedIn(router, now);
      router.service(ChannelService, { listChannels: () => ({ channels: fx.channels }) });
      router.service(ContactService, {
        listContacts: () => ({ contacts: fx.contacts(now) }),
        listDeliveries: () => ({ deliveries }),
        retryDelivery: async () => {
          await new Promise<void>((resolve) => (release = resolve));
          return {};
        },
      });
    });
    renderApp("/notifications?tab=deliveries", transport);
    const table = await screen.findByRole("table", { name: "Deliveries" });
    const retry = await waitFor(() => {
      const b = [...table.querySelectorAll<HTMLButtonElement>("tbody button")].find((x) =>
        /Retry/.test(x.textContent ?? ""),
      );
      expect(b).toBeDefined();
      return b!;
    });
    const before = cells(table);

    const user = userEvent.setup();
    retry.focus();
    await user.keyboard("{Enter}");
    await waitFor(() => expect(release).toBeDefined());
    // The request is in flight: the mutation's state has changed.
    expect(retry).toBeDisabled();
    expect(remounted(table, before)).toBe(0);
    expect(document.contains(retry)).toBe(true);
    release?.();
    await screen.findByText("Sending it again");
  });
});
