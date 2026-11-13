import { screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import type { RunJobRequest } from "@/gen/syncr/v1/api_pb";
import type { CreateScheduleRequest } from "@/gen/syncr/v1/schedules_pb";
import * as m5 from "@/mock/m5";
import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { renderApp } from "@/test/render";

const now = Date.now();
const kinds = ["Transfer", "Two-way sync", "Backup", "Retention", "Check", "Prune"];

function kindLabels(): (string | undefined)[] {
  const group = screen.getByRole("radiogroup", { name: "Kind of job" });
  return within(group)
    .getAllByRole("radio")
    .map((r) => kinds.find((k) => r.textContent?.startsWith(k)));
}

// These tests drive whole pages through the router, which is slow under a
// parallel run.
describe("restic jobs in the forms and the jobs pages", { timeout: 30_000 }, () => {
  it("offers six kinds to Run a job: never a restore or a data deletion", async () => {
    const api = m5Api(now);
    const { router } = renderApp("/jobs/new", api.transport);
    await screen.findByRole("radiogroup", { name: "Kind of job" });
    expect(kindLabels()).toEqual(kinds);
    const group = screen.getByRole("radiogroup", { name: "Kind of job" });
    expect(group).not.toHaveTextContent(/Restore|Delete data/);
    expect(within(group).getByRole("radio", { name: /^Transfer/ })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    // Picking a kind shows its editor and is kept in the URL.
    await userEvent.setup().click(within(group).getByRole("radio", { name: /^Check/ }));
    expect(await screen.findByRole("radiogroup", { name: "Read data" })).toBeInTheDocument();
    await waitFor(() => expect(router.state.location.search).toEqual({ kind: "restic_check" }));
  });

  it.each(["restic_delete_data", "restic_restore"])(
    "ignores ?kind=%s and stays on Transfer",
    async (kind) => {
      const api = m5Api(now);
      renderApp(`/jobs/new?kind=${kind}&repository=${m5.ids.photosVault}`, api.transport);
      const group = await screen.findByRole("radiogroup", { name: "Kind of job" });
      expect(within(group).getByRole("radio", { name: /^Transfer/ })).toHaveAttribute(
        "aria-checked",
        "true",
      );
      expect(kindLabels()).toEqual(kinds);
    },
  );

  it("runs a backup: repository preselected, paths, VSS only for a Windows worker", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    const { router } = renderApp(
      `/jobs/new?kind=restic_backup&repository=${m5.ids.photosVault}`,
      api.transport,
    );
    await waitFor(() =>
      expect(screen.getByRole("combobox", { name: /^Repository/ })).toHaveTextContent(
        "Photos vault",
      ),
    );
    const vss = screen.getByRole("switch", { name: "Use a volume snapshot (VSS)" });
    expect(vss).toBeDisabled();

    // Nothing filled in: local validation, no request.
    await user.click(screen.getByRole("button", { name: "Run job" }));
    expect(await screen.findByText("Add at least one folder or file to back up.")).toBeVisible();
    expect(screen.getByText("Choose the worker that runs the job.")).toBeVisible();
    expect(api.of("RunJob")).toHaveLength(0);

    await user.click(screen.getByRole("combobox", { name: /^Worker/ }));
    await user.click(await screen.findByRole("option", { name: /office-pc/ }));
    expect(vss).toBeEnabled();
    expect(screen.getByText(/Needs the worker to run as a system service/)).toBeVisible();
    await user.click(vss);
    await user.click(screen.getByRole("combobox", { name: /^Worker/ }));
    await user.click(await screen.findByRole("option", { name: /nas-01/ }));
    // Back on Linux the switch is off again and VSS is not sent.
    expect(vss).toBeDisabled();
    expect(vss).not.toBeChecked();

    await user.type(screen.getByLabelText("Path to back up 1"), "/srv/photos");
    await user.click(screen.getByRole("button", { name: "Add a path" }));
    await user.type(screen.getByLabelText("Path to back up 2"), "/srv/docs");
    await user.type(screen.getByLabelText(/^Tags/), "photos,");
    await user.click(screen.getByRole("switch", { name: "Skip cache folders" }));
    await user.click(screen.getByRole("button", { name: "Run job" }));
    await waitFor(() => expect(api.of("RunJob")).toHaveLength(1));
    const req = api.of<RunJobRequest>("RunJob")[0];
    expect(req.workerId).toBe(fx.ids.nas);
    expect(req.definition?.operation).toMatchObject({
      case: "resticBackup",
      value: {
        repositoryId: m5.ids.photosVault,
        paths: ["/srv/photos", "/srv/docs"],
        tags: ["photos"],
        excludeCaches: true,
        useFsSnapshot: false,
        dryRun: false,
      },
    });
    await waitFor(() => expect(router.state.location.pathname).toMatch(/^\/jobs\/job-/));
  });

  it("browses the worker's folders for a backup path", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    renderApp(`/jobs/new?kind=restic_backup`, api.transport);
    await user.click(await screen.findByRole("combobox", { name: /^Worker/ }));
    await user.click(await screen.findByRole("option", { name: /nas-01/ }));
    await user.click(screen.getByRole("button", { name: /^Browse/ }));
    const dialog = await screen.findByRole("dialog", { name: "Folders on nas-01" });
    await user.click(await within(dialog).findByRole("button", { name: "/srv" }));
    await user.click(await within(dialog).findByRole("checkbox", { name: "Select docs" }));
    await user.click(within(dialog).getByRole("checkbox", { name: "Select photos" }));
    await user.click(within(dialog).getByRole("button", { name: "Select 2 items" }));
    expect(screen.getByLabelText("Path to back up 1")).toHaveValue("/srv/docs");
    expect(screen.getByLabelText("Path to back up 2")).toHaveValue("/srv/photos");
  });

  it("schedules retention with presets; a schedule offers no restore", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    renderApp(`/schedules/new?kind=restic_forget&repository=${m5.ids.photosVault}`, api.transport);
    await screen.findByRole("radiogroup", { name: "Kind of job" });
    expect(kindLabels()).toEqual(kinds);
    expect(screen.getByRole("radiogroup", { name: "Kind of job" })).not.toHaveTextContent(
      /Restore|Delete data/,
    );
    expect(screen.getByRole("radio", { name: /^Retention/ })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    expect(screen.getByRole("button", { name: "7 daily, 4 weekly, 12 monthly" })).toHaveAttribute(
      "aria-pressed",
      "true",
    );
    expect(screen.getByRole("switch", { name: "Prune afterwards" })).toBeChecked();
    await user.click(screen.getByRole("button", { name: "Last 30" }));
    expect(screen.getByText(/Keeps the last 30 snapshots\./)).toBeVisible();

    await user.type(screen.getByLabelText("Name"), "Photos retention");
    await user.click(screen.getByRole("combobox", { name: "Worker" }));
    await user.click(await screen.findByRole("option", { name: /nas-01/ }));
    await user.click(screen.getByRole("button", { name: "Create schedule" }));
    await waitFor(() => expect(api.of("CreateSchedule")).toHaveLength(1));
    expect(
      api.of<CreateScheduleRequest>("CreateSchedule")[0].input?.definition?.operation,
    ).toMatchObject({
      case: "resticForget",
      value: {
        repositoryId: m5.ids.photosVault,
        policy: { keepLast: 30, keepDaily: 0, keepWeekly: 0, keepMonthly: 0 },
        prune: true,
        dryRun: false,
      },
    });
  });

  it("shows a backup's summary with a link to its snapshot, and no Edit for a deletion", async () => {
    const api = m5Api(now);
    const snap = api.repo(m5.ids.photosVault).snapshots[0];
    const view = renderApp(`/jobs/${m5.ids.lastBackup}`, api.transport);
    expect(
      await screen.findByRole("heading", { name: "Back up /srv/photos → Photos vault" }),
    ).toBeInTheDocument();
    const link = await screen.findByRole("link", {
      name: `Browse snapshot ${snap.id.slice(0, 8)}`,
    });
    expect(link).toHaveAttribute(
      "href",
      `/repositories/${m5.ids.photosVault}/snapshots/${snap.id}`,
    );
    expect(screen.getByText("Backup summary")).toBeInTheDocument();
    expect(
      screen.getByRole("img", { name: /^Files: .* new, .* changed, .* unmodified$/ }),
    ).toBeVisible();
    expect(screen.getByText("Summary")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /Edit and run/ })).toBeInTheDocument();
    view.unmount();

    renderApp(`/jobs/${m5.ids.retiredDelete}`, api.transport);
    expect(
      await screen.findByRole("heading", { name: "Delete the data of Retired NAS" }),
    ).toBeInTheDocument();
    expect(await screen.findByText("Deletion result")).toBeInTheDocument();
    expect(screen.getByText("The repository's folder was removed.")).toBeInTheDocument();
    expect(screen.getAllByText("48,213").length).toBeGreaterThan(0);
    for (const name of ["Edit and run", "Schedule", "Run again"])
      expect(screen.queryByRole("link", { name })).toBeNull();
    expect(screen.queryByRole("button", { name: /Run again/ })).toBeNull();
  });

  it("marks a queued job that waits for its repository, linking to the job ahead", async () => {
    const api = m5Api(now);
    renderApp(`/jobs?repository=${m5.ids.nasDocs}`, api.transport);
    const table = await screen.findByRole("table", { name: "Jobs" });
    const row = await within(table).findByRole("row", {
      name: "Back up /srv/backup/db → NAS documents",
    });
    expect(within(row).getByRole("link", { name: /Waiting for repository/ })).toHaveAttribute(
      "href",
      `/jobs/${m5.ids.runningBackup}`,
    );
    const running = within(table).getByRole("row", { name: "Back up /srv/docs → NAS documents" });
    expect(within(running).queryByText(/Waiting for repository/)).toBeNull();
    expect(within(running).getByText("Backup")).toBeInTheDocument();
  });
});
