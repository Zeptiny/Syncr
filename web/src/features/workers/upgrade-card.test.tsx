import { create } from "@bufbuild/protobuf";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import { InstallPlatform, WorkerSchema, WorkerStatus } from "@/gen/syncr/v1/api_pb";
import * as m5 from "@/mock/m5";
import * as m6 from "@/mock/m6";
import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { renderApp } from "@/test/render";
import { isOutdated, outdatedHelp } from "@/lib/workers";

const now = Date.now();

describe("outdated workers", () => {
  it("follows the controller's flag, and still knows a worker without probes", () => {
    const base = { status: WorkerStatus.ONLINE, info: { features: ["probe", "restic.v2"] } };
    expect(isOutdated(create(WorkerSchema, base))).toBe(false);
    expect(isOutdated(create(WorkerSchema, { ...base, outdated: true }))).toBe(true);
    expect(isOutdated(create(WorkerSchema, { ...base, info: { features: [] } }))).toBe(true);
    // A worker that never connected has reported nothing.
    expect(isOutdated(create(WorkerSchema, { status: WorkerStatus.PENDING }))).toBe(false);
    // A worker newer than the controller is not flagged by it: its upgrade
    // command would install the controller's older version.
    const newer = create(WorkerSchema, {
      ...base,
      info: { ...base.info, workerVersion: "v2.1.0" },
    });
    expect(isOutdated(newer)).toBe(false);
    expect(outdatedHelp(create(WorkerSchema, { ...base, outdated: true }))).toContain(
      "an older release than the controller",
    );
  });

  it("renders the controller's commands per OS, none for a platform without a build", () => {
    const workers = m5.workers(now);
    const office = workers.find((w) => w.id === fx.ids.office)!;
    const win = m6.upgradeCommands(office);
    expect(win.map((c) => c.platform)).toEqual([InstallPlatform.WINDOWS, InstallPlatform.WINDOWS]);
    expect(win[1].command).toContain("upgrade --user --version 'v2.0.0'");
    const arm = create(WorkerSchema, { info: { os: "windows", arch: "arm64" } });
    expect(m6.upgradeCommands(arm)).toEqual([]);
    expect(m6.upgradeCommands(create(WorkerSchema, {}))).toEqual([]);
  });
});

describe("worker pages", { timeout: 30_000 }, () => {
  it("badges outdated workers in the list", async () => {
    const api = m5Api(now);
    renderApp("/workers", api.transport);
    const old = await screen.findByRole("row", { name: /old-nas/ });
    expect(within(old).getByText("Outdated")).toBeVisible();
    expect(
      within(screen.getByRole("row", { name: /backup-host/ })).getByText("Outdated"),
    ).toBeVisible();
    expect(within(screen.getByRole("row", { name: /nas-01/ })).queryByText("Outdated")).toBeNull();
  });

  it("shows an administrator the upgrade commands of an outdated worker", async () => {
    const user = userEvent.setup();
    const writeText = vi.fn().mockResolvedValue(undefined);
    Object.defineProperty(navigator, "clipboard", { value: { writeText }, configurable: true });
    const api = m5Api(now);
    renderApp(`/workers/${fx.ids.backupHost}`, api.transport);

    const card = await screen.findByRole("region", { name: "Upgrade this worker" });
    expect(screen.getAllByText("Outdated")[0]).toBeVisible();
    expect(card).toHaveTextContent("Running jobs on this worker are interrupted");
    expect(card).toHaveTextContent("not that the worker reaches the controller");
    await waitFor(() =>
      expect(card).toHaveTextContent("sudo /usr/local/bin/syncr-worker upgrade --version 'v2.0.0'"),
    );
    // One OS: no platform tabs; the server's note is shown with the command.
    expect(within(card).queryByRole("tab")).toBeNull();
    expect(card).toHaveTextContent("Installs v2.0.0, the controller's version.");
    expect(api.of("GetWorkerUpgradeCommands")).toEqual([
      expect.objectContaining({ workerId: fx.ids.backupHost }),
    ]);

    await user.click(within(card).getByRole("radio", { name: "Current user" }));
    expect(card).toHaveTextContent(
      `"$HOME/.local/bin/syncr-worker" upgrade --user --version 'v2.0.0'`,
    );
    await user.click(within(card).getByRole("button", { name: "Copy command" }));
    expect(writeText).toHaveBeenCalledWith(
      `"$HOME/.local/bin/syncr-worker" upgrade --user --version 'v2.0.0'`,
    );
  });

  it("shows no upgrade card for a current worker", async () => {
    const api = m5Api(now);
    renderApp(`/workers/${fx.ids.nas}`, api.transport);
    await screen.findByRole("heading", { name: "nas-01" });
    expect(screen.queryByRole("region", { name: "Upgrade this worker" })).toBeNull();
    expect(screen.queryByText("Outdated")).toBeNull();
    expect(api.of("GetWorkerUpgradeCommands")).toHaveLength(0);
  });

  it("shows a user the badge but not the commands", async () => {
    const api = m5Api(now, "alice");
    renderApp(`/workers/${m5.ids.oldNas}`, api.transport);
    expect(await screen.findByText("Outdated")).toBeVisible();
    expect(screen.getByText(/cannot browse files or work with restic/)).toBeVisible();
    expect(screen.queryByRole("region", { name: "Upgrade this worker" })).toBeNull();
    expect(api.of("GetWorkerUpgradeCommands")).toHaveLength(0);
  });
});
