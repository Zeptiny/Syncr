import { create, toJson } from "@bufbuild/protobuf";
import { act, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";

import type { SelectedItem } from "@/components/app/file-browser/selection";
import { JobDefinitionSchema } from "@/gen/syncr/v1/api_pb";
import { ProbeEntrySchema, ProbeEntryType } from "@/gen/syncr/v1/link_pb";
import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { renderApp } from "@/test/render";

import {
  backupDraft,
  bisyncDraft,
  draftFolder,
  draftRunJob,
  draftSchedule,
  makeJobDraft,
  readJobDraft,
  transferDraft,
  type DraftTarget,
  type JobDraft,
} from "./job-drafts";

const now = Date.UTC(2026, 10, 10, 12);

const item = (path: string, dir = false): SelectedItem => ({
  path,
  entry: create(ProbeEntrySchema, {
    path,
    type: dir ? ProbeEntryType.DIR : ProbeEntryType.FILE,
  }),
});

const transfer = makeJobDraft(
  {
    operation: {
      case: "rcloneTransfer",
      value: {
        source: { remoteId: "", path: "/srv/docs" },
        destination: { remoteId: fx.ids.b2, path: "inbox" },
        options: {
          ignoreExisting: true,
          filters: [
            { action: 1, pattern: "/notes.md" },
            { action: 2, pattern: "**" },
          ],
        },
      },
    },
  },
  fx.ids.nas,
);
const bisync = makeJobDraft(
  { operation: { case: "rcloneBisync", value: { path1: { remoteId: "", path: "/srv/shared" } } } },
  fx.ids.nas,
);
const backup = makeJobDraft(
  { operation: { case: "resticBackup", value: { paths: ["/srv/docs", "/srv/photos"] } } },
  fx.ids.nas,
);

describe("job drafts", () => {
  it("round-trips through history state", () => {
    expect(transfer.kind).toBe("rclone_transfer");
    // What the history keeps is plain data.
    const stored = structuredClone({ jobDraft: transfer }) as unknown;
    const d = readJobDraft(stored);
    expect(d?.kind).toBe("rclone_transfer");
    expect(d?.workerId).toBe(fx.ids.nas);
    const s = d && draftRunJob(d);
    expect(s?.definition.kind === "rclone_transfer" && s.definition.transfer).toMatchObject({
      source: { remoteId: "", path: "/srv/docs" },
      destination: { remoteId: fx.ids.b2, path: "inbox" },
      ignoreExisting: true,
      filters: [
        { action: 1, pattern: "/notes.md" },
        { action: 2, pattern: "**" },
      ],
    });
    const sched = d && draftSchedule(d, "Europe/Berlin");
    expect(sched?.target).toEqual({ kind: "worker", workerId: fx.ids.nas, label: "" });
    expect(sched?.timezone).toBe("Europe/Berlin");
  });

  it("ignores missing or invalid drafts", () => {
    expect(readJobDraft(undefined)).toBeUndefined();
    expect(readJobDraft({})).toBeUndefined();
    expect(readJobDraft({ jobDraft: "x" })).toBeUndefined();
    expect(readJobDraft({ jobDraft: { ...transfer, kind: "restic_restore" } })).toBeUndefined();
    expect(readJobDraft({ jobDraft: { ...transfer, kind: "rclone_bisync" } })).toBeUndefined();
    expect(readJobDraft({ jobDraft: { ...transfer, workerId: 3 } })).toBeUndefined();
    expect(readJobDraft({ jobDraft: { ...transfer, definition: { nope: 1 } } })).toBeUndefined();
    const restore = toJson(
      JobDefinitionSchema,
      create(JobDefinitionSchema, {
        operation: { case: "resticRestore", value: { target: "/srv" } },
      }),
    );
    expect(
      readJobDraft({ jobDraft: { kind: "rclone_transfer", definition: restore, workerId: "" } }),
    ).toBeUndefined();
    expect(() =>
      makeJobDraft({ operation: { case: "resticRestore", value: { target: "/srv" } } }),
    ).toThrow();
  });

  describe("from the file browser", () => {
    const nas = { kind: "worker" as const, workerId: fx.ids.nas };
    const target = (t: Partial<DraftTarget>): DraftTarget => ({
      place: nas,
      items: [],
      parent: undefined,
      folder: "/srv/docs",
      workerId: fx.ids.nas,
      ...t,
    });
    const op = (d: JobDraft | undefined) =>
      d && readJobDraft({ jobDraft: d })?.definition.operation;

    it("names one folder: a selected one or the open one", () => {
      expect(draftFolder(target({}))).toBe("/srv/docs");
      expect(draftFolder(target({ folder: "" }))).toBeUndefined();
      expect(draftFolder(target({ items: [item("/srv/docs/taxes", true)] }))).toBe(
        "/srv/docs/taxes",
      );
      expect(draftFolder(target({ items: [item("/srv/docs/notes.md")] }))).toBeUndefined();
      expect(
        draftFolder({
          ...target({ folder: "" }),
          place: { kind: "remote", remoteId: fx.ids.b2 },
        }),
      ).toBe("");
    });

    it("backs up the selection or the open folder", () => {
      expect(op(backupDraft(target({})))).toMatchObject({
        case: "resticBackup",
        value: { paths: ["/srv/docs"] },
      });
      const items = [item("/srv/docs/notes.md"), item("/srv/photos", true)];
      expect(op(backupDraft(target({ items, parent: "/srv" })))).toMatchObject({
        value: { paths: ["/srv/docs/notes.md", "/srv/photos"] },
      });
      expect(backupDraft(target({ folder: "" }))).toBeUndefined();
    });

    it("copies the selection with filters, or the open folder", () => {
      expect(op(transferDraft(target({})))).toMatchObject({
        case: "rcloneTransfer",
        value: { source: { path: "/srv/docs" }, options: { filters: [] } },
      });
      const items = [item("/srv/docs/notes.md"), item("/srv/docs/taxes", true)];
      expect(op(transferDraft(target({ items, parent: "/srv/docs" })))).toMatchObject({
        value: {
          source: { remoteId: "", path: "/srv/docs" },
          options: {
            filters: [{ pattern: "/notes.md" }, { pattern: "/taxes/**" }, { pattern: "**" }],
          },
        },
      });
    });

    it("syncs one folder both ways", () => {
      const remote = target({
        place: { kind: "remote", remoteId: fx.ids.b2 },
        folder: "photos",
        workerId: "",
      });
      expect(op(bisyncDraft(remote))).toMatchObject({
        case: "rcloneBisync",
        value: { path1: { remoteId: fx.ids.b2, path: "photos" } },
      });
      expect(bisyncDraft(target({ items: [item("/a"), item("/b")] }))).toBeUndefined();
    });
  });
});

describe("drafts in the forms", { timeout: 30_000 }, () => {
  const open = async (to: "/jobs/new" | "/schedules/new", jobDraft: JobDraft) => {
    const api = m5Api(now);
    const view = renderApp("/", api.transport);
    await act(() => view.router.navigate({ to, state: (prev) => ({ ...prev, jobDraft }) }));
    return { api, ...view };
  };

  it("fills Run a job with a transfer", async () => {
    await open("/jobs/new", transfer);
    const from = await screen.findByRole("group", { name: "From" });
    await waitFor(() => expect(within(from).getByLabelText("Path")).toHaveValue("/srv/docs"));
    expect(within(screen.getByRole("group", { name: "To" })).getByLabelText("Path")).toHaveValue(
      "inbox",
    );
    expect(screen.getByText("2 filters")).toBeInTheDocument();
    expect(await screen.findByRole("combobox", { name: /^Worker/ })).toHaveTextContent("nas-01");
  });

  it("fills Run a job with a two-way sync", async () => {
    await open("/jobs/new", bisync);
    const a = await screen.findByRole("group", { name: "Folder A" });
    expect(within(a).getByLabelText("Path")).toHaveValue("/srv/shared");
    expect(screen.getByRole("radio", { name: /^Two-way sync/ })).toHaveAttribute(
      "aria-checked",
      "true",
    );
  });

  it("fills Run a job with a backup", async () => {
    await open("/jobs/new", backup);
    expect(await screen.findByLabelText("Path to back up 1")).toHaveValue("/srv/docs");
    expect(screen.getByLabelText("Path to back up 2")).toHaveValue("/srv/photos");
  });

  it("fills a new schedule, on the draft's worker", async () => {
    await open("/schedules/new", backup);
    expect(await screen.findByLabelText("Path to back up 1")).toHaveValue("/srv/docs");
    expect(screen.getByRole("heading", { name: "New schedule" })).toBeInTheDocument();
    expect(await screen.findByRole("combobox", { name: /^Worker/ })).toHaveTextContent("nas-01");
    await open("/schedules/new", bisync);
    expect(
      within(await screen.findByRole("group", { name: "Folder A" })).getByLabelText("Path"),
    ).toHaveValue("/srv/shared");
  });

  it("opens the empty form without a draft, or with a broken one", async () => {
    await open("/jobs/new", { ...transfer, kind: "restic_check" });
    const from = await screen.findByRole("group", { name: "From" });
    expect(within(from).getByLabelText("Path")).toHaveValue("");
  });

  it("starts from the worker browser's New job menu", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    const { router } = renderApp(
      `/workers/${fx.ids.nas}/files?path=${encodeURIComponent("/srv/docs")}`,
      api.transport,
    );
    const grid = await screen.findByRole("grid", { name: "Contents of /srv/docs" });
    await user.click(await within(grid).findByRole("checkbox", { name: "Select notes.md" }));
    await user.click(within(grid).getByRole("checkbox", { name: "Select taxes" }));
    await user.click(screen.getByRole("button", { name: /New job/ }));
    const menu = await screen.findByRole("menu");
    expect(within(menu).getByText("For the 2 items selected")).toBeInTheDocument();
    // Two items name no single folder to keep in step.
    expect(within(menu).getByRole("menuitem", { name: "Two-way sync…" })).toHaveAttribute(
      "aria-disabled",
      "true",
    );
    await user.click(within(menu).getByRole("menuitem", { name: "Back up with restic…" }));
    await waitFor(() => expect(router.state.location.pathname).toBe("/jobs/new"));
    expect(await screen.findByLabelText("Path to back up 1")).toHaveValue("/srv/docs/notes.md");
    expect(screen.getByLabelText("Path to back up 2")).toHaveValue("/srv/docs/taxes");
  });

  it("offers a new repository in a remote's folder", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    const { router } = renderApp(`/remotes/${fx.ids.minio}/files?path=archive`, api.transport);
    await screen.findByRole("grid", { name: "Contents of archive" });
    await user.click(screen.getByRole("button", { name: /New job/ }));
    await user.click(await screen.findByRole("menuitem", { name: "New restic repository here" }));
    await waitFor(() =>
      expect(router.state.location.search).toEqual({ remote: fx.ids.minio, path: "archive" }),
    );
    expect(await screen.findByLabelText(/^Path/)).toHaveValue("archive");
  });
});
