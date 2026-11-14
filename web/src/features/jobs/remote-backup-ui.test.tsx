import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { screen, waitFor, within } from "@testing-library/react";
import userEvent, { type UserEvent } from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";

import {
  JobProgressSchema,
  JobService,
  WorkerAdminService,
  type RunJobRequest,
} from "@/gen/syncr/v1/api_pb";
import { ResticRemoteSourceMethod } from "@/gen/syncr/v1/link_pb";
import type { CreateScheduleRequest } from "@/gen/syncr/v1/schedules_pb";
import * as m5 from "@/mock/m5";
import * as rb from "@/mock/remote-backup";
import { formatBytes } from "@/lib/format";
import { signedIn } from "@/test/app-api";
import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { mockTransport, renderApp } from "@/test/render";

// uPlot needs a canvas, which jsdom lacks (the job page's throughput
// chart would draw after a test ended); these tests read the page around it.
vi.mock("uplot", () => ({
  default: class {
    setData() {}
    setSize() {}
    destroy() {}
  },
}));

const now = Date.now();
const MIRROR = ResticRemoteSourceMethod.MIRROR;
const MOUNT = ResticRemoteSourceMethod.MOUNT;

/** choose picks an option of a labelled select. */
async function choose(user: UserEvent, label: RegExp, option: RegExp) {
  await user.click(screen.getByRole("combobox", { name: label }));
  await user.click(await screen.findByRole("option", { name: option }));
}

/** openEditor opens Run a job on a backup of a remote, with the remote-backup demo. */
async function openEditor() {
  const user = userEvent.setup();
  const api = m5Api(now, "admin", { remoteBackup: true });
  const app = renderApp(`/jobs/new?kind=restic_backup`, api.transport);
  await user.click(await screen.findByRole("radio", { name: /^A folder on a remote/ }));
  return { user, api, ...app };
}

const sent = (api: ReturnType<typeof m5Api>) => {
  const op = api.of<RunJobRequest>("RunJob").at(-1)?.definition?.operation;
  return op?.case === "resticBackup" ? op.value : undefined;
};

// These tests drive whole pages through the router, which is slow under a
// parallel run.
describe("backups of a remote in the forms", { timeout: 30_000 }, () => {
  it("switches the source without losing what was typed on either side", async () => {
    const user = userEvent.setup();
    const api = m5Api(now, "admin", { remoteBackup: true });
    renderApp(`/jobs/new?kind=restic_backup`, api.transport);
    await user.type(await screen.findByLabelText("Path to back up 1"), "/srv/photos");
    expect(screen.getByLabelText(/^Exclude/)).toBeInTheDocument();

    await user.click(screen.getByRole("radio", { name: /^A folder on a remote/ }));
    expect(screen.queryByLabelText("Path to back up 1")).toBeNull();
    // Restic excludes, one file system and VSS are not offered for a remote.
    expect(screen.queryByRole("button", { name: "Add an exclude" })).toBeNull();
    expect(screen.queryByRole("switch", { name: "Stay on one file system" })).toBeNull();
    expect(screen.queryByRole("switch", { name: /VSS/ })).toBeNull();
    expect(screen.getByText(/A mirror is still brought up to date for real/)).toBeVisible();
    await user.type(screen.getByLabelText(/^Folder/), "Photos");

    await user.click(screen.getByRole("radio", { name: /^Folders on a worker/ }));
    expect(screen.getByLabelText("Path to back up 1")).toHaveValue("/srv/photos");
    await user.click(screen.getByRole("radio", { name: /^A folder on a remote/ }));
    expect(screen.getByLabelText(/^Folder/)).toHaveValue("Photos");
  });

  it("offers a Local directory remote disabled, with the controller's reason", async () => {
    const { user } = await openEditor();
    await user.click(screen.getByRole("combobox", { name: /^Remote/ }));
    const media = await screen.findByRole("option", { name: /^Media disk/ });
    expect(media).toHaveAttribute("aria-disabled", "true");
    expect(media).toHaveTextContent(
      "this remote is a folder on a worker; back up that folder instead",
    );
    expect(screen.getByRole("option", { name: /^Nextcloud/ })).not.toHaveAttribute("aria-disabled");
  });

  it("runs a mirror: only the folder of the remote is sent", async () => {
    const { user, api } = await openEditor();
    await choose(user, /^Worker/, /^nas-01/);
    await choose(user, /^Repository/, /^Nextcloud photos/);
    await choose(user, /^Remote/, /^Nextcloud/);
    await user.type(screen.getByLabelText(/^Folder/), "Photos");
    await user.click(screen.getByRole("button", { name: "Add filter" }));
    await user.type(screen.getByLabelText("Filter 1 pattern"), "*.tmp");
    await user.click(screen.getByRole("switch", { name: "Delete the mirror after each run" }));
    await user.click(screen.getByRole("button", { name: "Advanced" }));
    await user.type(screen.getByLabelText("Parallel downloads"), "8");
    await user.click(screen.getByRole("switch", { name: "Back up an empty folder" }));
    await user.click(screen.getByRole("button", { name: "Run job" }));

    await waitFor(() => expect(api.of("RunJob")).toHaveLength(1));
    const b = sent(api);
    expect(b?.paths).toEqual([]);
    expect(b?.excludes).toEqual([]);
    expect(b?.remoteSource).toMatchObject({
      remoteId: fx.ids.nextcloud,
      path: "Photos",
      method: MIRROR,
      discardMirror: true,
      allowEmpty: true,
      transfers: 8,
      filters: [{ pattern: "*.tmp" }],
    });
    expect(
      await screen.findByRole("heading", {
        name: "Back up Nextcloud/Photos (mirror) → Nextcloud photos",
      }),
    ).toBeInTheDocument();
  });

  it("runs a mount without the mirror's options", async () => {
    const { user, api } = await openEditor();
    await choose(user, /^Worker/, /^nas-01/);
    await choose(user, /^Repository/, /^Nextcloud photos/);
    await choose(user, /^Remote/, /^Storage box/);
    await user.type(screen.getByLabelText(/^Folder/), "Scans");
    await user.click(screen.getByRole("switch", { name: "Delete the mirror after each run" }));
    await user.click(screen.getByRole("radio", { name: /^Mount/ }));
    await user.click(screen.getByRole("button", { name: "Run job" }));
    await waitFor(() => expect(api.of("RunJob")).toHaveLength(1));
    expect(sent(api)?.remoteSource).toMatchObject({
      remoteId: fx.ids.storageBox,
      path: "Scans",
      method: MOUNT,
      discardMirror: false,
      fastList: false,
    });
    expect(sent(api)?.remoteSource?.transfers).toBeUndefined();
  });

  it("disables a method the worker cannot use, with its reason", async () => {
    const { user } = await openEditor();
    // Without a worker, both are offered.
    expect(screen.getByRole("radio", { name: /^Mount/ })).toBeEnabled();
    await choose(user, /^Worker/, /^backup-host/);
    const mount = screen.getByRole("radio", { name: /^Mount/ });
    expect(mount).toBeDisabled();
    expect(mount).toHaveTextContent("backup-host cannot mount a remote: /dev/fuse is missing.");
    expect(screen.getByRole("radio", { name: /^Mirror/ })).toBeEnabled();

    // Chosen on nas-01, then the worker changes: the choice stays, with a warning.
    await choose(user, /^Worker/, /^nas-01/);
    await user.click(screen.getByRole("radio", { name: /^Mount/ }));
    await user.click(screen.getByRole("combobox", { name: /^Worker/ }));
    // The worker list marks who cannot mount, and the outdated worker.
    expect(await screen.findByRole("option", { name: /^backup-host/ })).toHaveTextContent(
      "can't mount: /dev/fuse is missing",
    );
    expect(screen.getByRole("option", { name: /^backup-host/ })).toHaveAttribute(
      "aria-disabled",
      "true",
    );
    expect(screen.getByRole("option", { name: /^nas-01/ })).not.toHaveAttribute("aria-disabled");
  });

  it("puts the controller's refusals on their fields", async () => {
    const { user, api } = await openEditor();
    await choose(user, /^Worker/, /^nas-01/);
    await choose(user, /^Repository/, /^Nextcloud photos/);
    await choose(user, /^Remote/, /^MinIO archive/);
    await user.type(screen.getByLabelText(/^Folder/), "restic");
    // The browser already sees the overlap.
    expect(screen.getByText(/This folder holds repository Nextcloud photos/)).toBeVisible();
    await user.type(screen.getByLabelText(/^Tags/), "syncr-remote:x,");
    await user.click(screen.getByRole("button", { name: "Run job" }));
    // A reserved tag is refused before sending.
    expect(await screen.findByText(/Tags cannot start with “syncr-remote:”/)).toBeVisible();
    expect(api.of("RunJob")).toHaveLength(0);

    await user.click(screen.getByRole("button", { name: /Remove tag syncr-remote:x/ }));
    await user.click(screen.getByRole("button", { name: "Run job" }));
    await waitFor(() => expect(api.of("RunJob")).toHaveLength(1));
    expect(
      await screen.findByText(
        'holds repository "Nextcloud photos" (at "restic/nextcloud-photos"): a backup cannot read its own repository; choose a folder beside it',
      ),
    ).toBeVisible();
    // The API's message replaces the browser's warning.
    expect(screen.queryByText(/This folder holds repository Nextcloud photos/)).toBeNull();
  });

  it("refuses a schedule on a label, as two-way syncs do", async () => {
    const user = userEvent.setup();
    const api = m5Api(now, "admin", { remoteBackup: true });
    renderApp(`/schedules/new?kind=restic_backup`, api.transport);
    const label = await screen.findByRole("radio", { name: /Any worker with a label/ });
    expect(label).toBeEnabled();
    await user.click(label);
    await user.click(screen.getByRole("radio", { name: /^A folder on a remote/ }));
    // Switching to a remote moves the target to one worker and disables labels.
    expect(screen.getByRole("radio", { name: /Any worker with a label/ })).toBeDisabled();
    expect(screen.getByRole("radio", { name: /A specific worker/ })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    expect(
      screen.getByText(
        "A backup of a remote keeps its mirror or mount folder on one worker, so its schedule runs on a specific worker, not on any worker with a label.",
      ),
    ).toBeVisible();
    // Back to folders: labels are fine again.
    await user.click(screen.getByRole("radio", { name: /^Folders on a worker/ }));
    expect(screen.getByRole("radio", { name: /Any worker with a label/ })).toBeEnabled();
    expect(api.of<CreateScheduleRequest>("CreateSchedule")).toHaveLength(0);
  });
});

describe("backups of a remote on the job page", { timeout: 30_000 }, () => {
  it("shows the live phase with its own counters", async () => {
    const job = rb.runningJob(now);
    const progress = (phase: string, bytes: number, total: number, t: number) =>
      create(JobProgressSchema, {
        time: timestampFromMs(now + t),
        phase,
        stats: { bytes: BigInt(bytes), totalBytes: BigInt(total), files: 3n, totalFiles: 9n },
      });
    // The stored counters are the mirror's, larger than the backup's new ones.
    const transport = mockTransport((router) => {
      signedIn(router, now);
      router.service(WorkerAdminService, { listWorkers: () => ({ workers: m5.workers(now) }) });
      router.service(JobService, {
        getJob: () => ({ job }),
        listJobSamples: () => ({ samples: [] }),
        async *watchJob(_req, ctx) {
          yield { update: { case: "job" as const, value: job } };
          yield { update: { case: "progress" as const, value: progress("mirror", 600, 620, 0) } };
          yield {
            update: { case: "progress" as const, value: progress("backup", 50, 2000, 1000) },
          };
          await new Promise<void>((resolve) =>
            ctx.signal.addEventListener("abort", () => resolve(), { once: true }),
          );
        },
      });
    });
    renderApp(`/jobs/${job.id}`, transport);
    const status = await screen.findByText("Backing up…");
    expect(status.closest("[role=status]")).toHaveTextContent("Backing up…step 2 of 2");
    // The backup's counters, not the larger ones of the mirror before it.
    const card = screen.getByText("Processed").parentElement!;
    expect(card).toHaveTextContent(formatBytes(50));
    expect(card).toHaveTextContent(`of ${formatBytes(2000)}`);
    expect(screen.queryByText("Mirrored")).toBeNull();
  });

  it("says when an allowed empty folder made no snapshot", async () => {
    const api = m5Api(now, "admin", { remoteBackup: true });
    renderApp(`/jobs/${rb.ids.emptyJob}`, api.transport);
    expect(
      await screen.findByText("No snapshot: the remote folder was empty", {
        selector: "div",
      }),
    ).toBeVisible();
    // The controller's summary rows.
    expect(screen.getByText(/^0 bytes, 0 files, 3 deleted in 2s$/)).toBeVisible();
    expect(screen.getAllByText("Mirror").length).toBeGreaterThan(0);
    expect(screen.queryByText("None (dry run)")).toBeNull();
  });

  it("explains a method the worker could not use", async () => {
    const api = m5Api(now, "admin", { remoteBackup: true });
    renderApp(`/jobs/${rb.ids.refusedJob}`, api.transport);
    expect(await screen.findByText("The worker cannot mount remotes")).toBeVisible();
    expect(screen.getByText(/Choose the mirror, or a worker that can mount/)).toBeVisible();
    expect(screen.getByText(rb.messages.cannotMount)).toBeVisible();
  });
});

describe("snapshots of a remote", { timeout: 30_000 }, () => {
  it("are listed as From Nextcloud with a badge, not their internal folder", async () => {
    const api = m5Api(now, "admin", { remoteBackup: true });
    renderApp(`/repositories/${rb.ids.repository}`, api.transport);
    const table = await screen.findByRole("table", { name: "Snapshots" });
    await waitFor(() => expect(within(table).getAllByText("From Nextcloud")).toHaveLength(12));
    expect(within(table).queryByText(rb.MIRROR_PATH)).toBeNull();
    expect(within(table).queryByText(/^syncr-remote:/)).toBeNull();
    expect(within(table).getAllByText("phone")).toHaveLength(12);
  });

  it("are shown as From a remote when the remote is not the user's", async () => {
    const api = m5Api(now, "admin", { remoteBackup: true });
    api.repo(rb.ids.repository).snapshots.forEach((s) => {
      s.tags = ["syncr-remote:someone-elses"];
    });
    renderApp(`/repositories/${rb.ids.repository}`, api.transport);
    const table = await screen.findByRole("table", { name: "Snapshots" });
    await waitFor(() => expect(within(table).getAllByText("From a remote")).toHaveLength(12));
  });

  it("are never restored to an original location", async () => {
    const user = userEvent.setup();
    const api = m5Api(now, "admin", { remoteBackup: true });
    const snap = api.repo(rb.ids.repository).snapshots[0];
    renderApp(`/repositories/${rb.ids.repository}/snapshots/${snap.id}`, api.transport);
    expect(
      await screen.findByText(
        /This snapshot is of a remote: restore it to a worker folder, then copy it to the remote/,
      ),
    ).toBeVisible();
    expect(screen.getAllByText("From Nextcloud").length).toBeGreaterThan(0);
    const grid = await screen.findByRole("grid");
    // The row menu has no original location.
    await user.click(await within(grid).findByRole("button", { name: "Actions for README.txt" }));
    const menu = await screen.findByRole("menu");
    expect(
      within(menu)
        .getAllByRole("menuitem")
        .map((m) => m.textContent),
    ).toEqual(["Restore…", "Show versions", "Copy path"]);
    await user.keyboard("{Escape}");
    // Nor the tray.
    await user.click(within(grid).getByRole("checkbox", { name: "Select README.txt" }));
    const tray = screen.getByRole("region", { name: "Selection" });
    expect(
      within(tray).queryByRole("button", { name: "Restore to original location…" }),
    ).toBeNull();
    // Nor the restore sheet of a folder.
    await user.click(within(grid).getByRole("button", { name: "2026" }));
    await user.click(await screen.findByRole("button", { name: "Restore this folder…" }));
    const sheet = await screen.findByRole("dialog", { name: /^Restore from snapshot/ });
    expect(
      within(sheet).queryByRole("button", { name: "Restore to the original location" }),
    ).toBeNull();
    expect(within(sheet).getByText(/then copy it to the remote/)).toBeVisible();
  });
});

describe("workers and the remote browser", { timeout: 30_000 }, () => {
  it("lists what each worker can do for remote backups", async () => {
    const api = m5Api(now, "admin", { remoteBackup: true });
    renderApp("/workers", api.transport);
    const table = await screen.findByRole("table", { name: "Workers" });
    const row = (name: RegExp) => within(table).getByRole("row", { name });
    await waitFor(() => expect(row(/^backup-host/)).toHaveTextContent("/dev/fuse is missing"));
    expect(row(/^backup-host/)).toHaveTextContent("Mirror: yes. Mount: no, /dev/fuse is missing.");
    expect(row(/^nas-01/)).toHaveTextContent("Mirror: yes. Mount: yes.");
    expect(row(/^office-pc/)).toHaveTextContent("WinFsp is not installed");
    // Workers that report nothing show nothing.
    expect(row(/^old-nas/)).not.toHaveTextContent(/Mirror/);
  });

  it("backs up a remote's open folder from the browser", async () => {
    const user = userEvent.setup();
    const api = m5Api(now, "admin", { remoteBackup: true });
    const { router } = renderApp(`/remotes/${fx.ids.nextcloud}/files?path=Photos`, api.transport);
    await screen.findByRole("grid");
    await user.click(screen.getByRole("button", { name: /New job/ }));
    const menu = await screen.findByRole("menu");
    expect(within(menu).getByText("For Photos")).toBeInTheDocument();
    await user.click(within(menu).getByRole("menuitem", { name: "Back up this folder…" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/jobs/new"));
    expect(await screen.findByRole("radio", { name: /^A folder on a remote/ })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    expect(screen.getByLabelText(/^Folder/)).toHaveValue("Photos");
    expect(screen.getByRole("combobox", { name: /^Remote/ })).toHaveTextContent("Nextcloud");
    expect(screen.getByRole("radio", { name: /^Mirror/ })).toHaveAttribute("aria-checked", "true");
  });
});
