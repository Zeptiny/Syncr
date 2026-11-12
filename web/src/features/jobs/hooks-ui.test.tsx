import { create } from "@bufbuild/protobuf";
import { durationFromMs } from "@bufbuild/protobuf/wkt";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it } from "vitest";

import { WorkerSchema, type RunJobRequest } from "@/gen/syncr/v1/api_pb";
import type { CreateScheduleRequest } from "@/gen/syncr/v1/schedules_pb";
import * as hooksMock from "@/mock/hooks";
import * as m5 from "@/mock/m5";
import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { renderApp } from "@/test/render";

import { WorkerHooksPanel } from "../workers/worker-detail-page";
import { HookRunsPanel } from "./hook-runs";
import { emptyHooks, type HooksState, type HooksTarget } from "./hooks";
import { HooksPanel } from "./hooks-editor";

const now = Date.now();

function worker(name: string, hooks: [string, string?][] | undefined, features = ["job.hooks"]) {
  return create(WorkerSchema, {
    id: name,
    name,
    info:
      hooks === undefined
        ? undefined
        : {
            workerVersion: "v2.1.0",
            features,
            hooks: hooks.map(([n, description = ""]) => ({
              name: n,
              description,
              timeout: durationFromMs(120_000),
            })),
          },
  });
}

const nas = worker("nas-01", [
  ["dump-db", "Dump the database"],
  ["start-app", "Start the app again"],
  ["stop-app", "Stop the app"],
]);

/** Editor is HooksPanel with its own state, showing what it holds. */
function Editor({ initial = emptyHooks(), target }: { initial?: HooksState; target: HooksTarget }) {
  const [value, setValue] = useState(initial);
  return (
    <>
      <HooksPanel idPrefix="t" value={value} onChange={setValue} target={target} />
      <output data-testid="value">{JSON.stringify(value)}</output>
    </>
  );
}

const held = () => JSON.parse(screen.getByTestId("value").textContent ?? "{}") as HooksState;

describe("HooksPanel", () => {
  it("asks for a place to run before anything can be chosen", () => {
    render(<Editor target={{ workers: [] }} />);
    expect(
      screen.getByText("Choose where the job runs to see the hooks its worker offers."),
    ).toBeVisible();
    expect(screen.queryByRole("combobox")).toBeNull();
  });

  it("says where hooks come from when the worker defines none", () => {
    render(<Editor target={{ workers: [worker("office-pc", [])] }} />);
    expect(
      screen.getByText(/office-pc defines no hooks\. A worker's operator adds them/),
    ).toBeVisible();
    expect(screen.queryByRole("combobox")).toBeNull();
  });

  it("says a worker from before hooks must be upgraded", () => {
    render(<Editor target={{ workers: [worker("old-nas", [], ["probe"])] }} />);
    expect(screen.getByText("old-nas must be upgraded before it can run hooks.")).toBeVisible();
  });

  it("adds hooks to a phase in the order chosen, and removes them", async () => {
    const user = userEvent.setup();
    render(<Editor target={{ workers: [nas] }} />);
    // Four phases, each with its own list.
    expect(screen.getAllByRole("combobox")).toHaveLength(4);
    expect(screen.getByText(/If one fails, the job does not run and fails\./)).toBeVisible();

    await user.click(screen.getByRole("combobox", { name: "Add a hook: before the job" }));
    const options = await screen.findAllByRole("option");
    expect(options.map((o) => o.textContent)).toEqual([
      "dump-dbDump the database · up to 2m",
      "start-appStart the app again · up to 2m",
      "stop-appStop the app · up to 2m",
    ]);
    await user.click(screen.getByRole("option", { name: /stop-app/ }));
    await user.click(screen.getByRole("combobox", { name: "Add a hook: before the job" }));
    // What the phase already runs is not offered again.
    expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toHaveLength(2);
    await user.click(screen.getByRole("option", { name: /dump-db/ }));
    await user.click(screen.getByRole("combobox", { name: "Add a hook: always, at the end" }));
    await user.click(await screen.findByRole("option", { name: /start-app/ }));

    expect(held()).toEqual({
      before: ["stop-app", "dump-db"],
      onSuccess: [],
      onFailure: [],
      after: ["start-app"],
    });
    const before = screen.getByRole("list", { name: "Hooks: before the job" });
    expect(
      within(before)
        .getAllByRole("listitem")
        .map((li) => li.textContent),
    ).toEqual(["1stop-app", "2dump-db"]);

    await user.click(screen.getByRole("button", { name: "Remove hook stop-app: before the job" }));
    expect(held().before).toEqual(["dump-db"]);
    expect(held().after).toEqual(["start-app"]);
  });

  it("keeps, and marks, a hook the worker does not define", () => {
    render(
      <Editor
        initial={{ ...emptyHooks(), before: ["stop-app", "gone"], after: ["start-app"] }}
        target={{ workers: [nas] }}
      />,
    );
    expect(screen.getByRole("alert")).toHaveTextContent(
      "nas-01 does not define the hook gone. Remove it, or choose another place to run.",
    );
    expect(held().before).toEqual(["stop-app", "gone"]);
    expect(screen.getByText("gone").closest("li")).toHaveAttribute(
      "title",
      "nas-01 does not define this hook",
    );
    expect(screen.getByText("stop-app").closest("li")).not.toHaveAttribute("title");
  });

  it("keeps the hooks of a stored job visible while its worker is unknown", () => {
    render(<Editor initial={{ ...emptyHooks(), before: ["stop-app"] }} target={{ workers: [] }} />);
    expect(screen.getByText("stop-app")).toBeVisible();
    // Nothing is known, so nothing is called missing.
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("offers a label's hooks with how many of its workers define each", async () => {
    const user = userEvent.setup();
    const db = worker("db-01", [["dump-db", "Dump the database"]]);
    render(<Editor target={{ workers: [nas, db], label: "app" }} />);
    expect(
      screen.getByText(
        "A run goes to a worker with the label that defines every hook chosen here.",
      ),
    ).toBeVisible();
    await user.click(screen.getByRole("combobox", { name: "Add a hook: before the job" }));
    expect((await screen.findAllByRole("option")).map((o) => o.textContent)).toEqual([
      "dump-dbDump the database · up to 2m · on 2 of 2 workers",
      "start-appStart the app again · up to 2m · on 1 of 2 workers",
      "stop-appStop the app · up to 2m · on 1 of 2 workers",
    ]);
    await user.click(screen.getByRole("option", { name: /stop-app/ }));
    expect(
      screen.getByText(
        "1 of 2 workers labelled app defines all of these hooks; a run goes to one of them.",
      ),
    ).toBeVisible();
  });

  it("shows the server's error", () => {
    render(
      <HooksPanel
        idPrefix="t"
        value={{ ...emptyHooks(), before: ["stop-app"] }}
        onChange={() => {}}
        target={{ workers: [nas] }}
        error={'worker "nas-01" defines no hook named "stop-app"'}
      />,
    );
    expect(screen.getByRole("alert")).toHaveTextContent('defines no hook named "stop-app"');
  });
});

describe("HookRunsPanel", () => {
  const [ok, failed] = hooksMock.jobs(now);

  it("lists what each hook did, by phase", () => {
    render(<HookRunsPanel job={ok} />);
    const before = screen.getByRole("region", { name: "Hooks: before the job" });
    expect(within(before).getAllByRole("listitem")).toHaveLength(2);
    expect(within(before).getByText("stop-paperless")).toBeVisible();
    expect(within(before).getAllByText("Succeeded")).toHaveLength(2);
    expect(within(before).getByText("3.2s")).toBeVisible();
    expect(within(before).getByText("41s")).toBeVisible();
    expect(screen.getByRole("region", { name: "Hooks: after success" })).toHaveTextContent(
      "ping-healthcheck",
    );
    // What a hook that succeeded printed stays folded until asked for.
    expect(screen.queryByText(/wrote \/srv\/paperless\/db\/paperless\.dump/)).toBeNull();
  });

  it("opens the output of a hook that did not succeed", async () => {
    const user = userEvent.setup();
    render(<HookRunsPanel job={failed} />);
    const after = screen.getByRole("region", { name: "Hooks: always, at the end" });
    expect(within(after).getByText("Timed out")).toBeVisible();
    expect(within(after).getByText("timed out after 2m0s")).toBeVisible();
    expect(within(after).getByText(/port is already allocated/)).toBeVisible();

    // The others open on request, saying when only the end was kept.
    await user.click(screen.getByRole("button", { name: /dump-paperless-db/ }));
    expect(screen.getByText(/wrote \/srv\/paperless\/db\/paperless\.dump/)).toBeVisible();
    expect(screen.getByText("Only the end of what it printed is kept.")).toBeVisible();
  });

  it("says which hooks did not run", () => {
    render(
      <HookRunsPanel
        job={{ ...failed, hookRuns: failed.hookRuns.filter((r) => r.name !== "ping-healthcheck") }}
      />,
    );
    expect(screen.getByRole("region", { name: "Hooks: after success" })).toHaveTextContent(
      "ping-healthcheckDid not run",
    );
  });

  it("renders nothing for a job without hooks", () => {
    const { container } = render(
      <HookRunsPanel job={{ ...ok, hookRuns: [], definition: undefined }} />,
    );
    expect(container).toBeEmptyDOMElement();
  });
});

describe("WorkerHooksPanel", () => {
  it("lists the worker's hooks with what they do", () => {
    render(<WorkerHooksPanel worker={nas} />);
    const rows = screen.getAllByRole("listitem");
    expect(rows.map((r) => r.textContent)).toEqual([
      "dump-dbDump the databasestopped after 2m",
      "start-appStart the app againstopped after 2m",
      "stop-appStop the appstopped after 2m",
    ]);
  });

  it("says how to offer some, or that the worker is too old", () => {
    const { rerender } = render(<WorkerHooksPanel worker={worker("office-pc", [])} />);
    expect(
      screen.getByText(/add them to the worker's configuration file under “hooks”/),
    ).toBeVisible();
    rerender(<WorkerHooksPanel worker={worker("old-nas", [], ["probe"])} />);
    expect(
      screen.getByText("This worker's version cannot run hooks. Upgrade it first."),
    ).toBeVisible();
  });

  it("shows nothing for a worker that has not connected yet", () => {
    const { container } = render(<WorkerHooksPanel worker={worker("laptop", undefined)} />);
    expect(container).toBeEmptyDOMElement();
  });
});

describe("hooks in the forms", { timeout: 30_000 }, () => {
  it("runs a backup with the worker's hooks", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    renderApp(`/jobs/new?kind=restic_check&repository=${m5.ids.photosVault}`, api.transport);
    expect(
      await screen.findByText("Choose where the job runs to see the hooks its worker offers."),
    ).toBeVisible();
    await user.click(screen.getByRole("combobox", { name: /^Worker/ }));
    await user.click(await screen.findByRole("option", { name: /nas-01/ }));

    await user.click(await screen.findByRole("combobox", { name: "Add a hook: before the job" }));
    await user.click(await screen.findByRole("option", { name: /stop-paperless/ }));
    await user.click(screen.getByRole("combobox", { name: "Add a hook: always, at the end" }));
    await user.click(await screen.findByRole("option", { name: /start-paperless/ }));
    await user.click(screen.getByRole("button", { name: "Run job" }));

    await waitFor(() => expect(api.of("RunJob")).toHaveLength(1));
    const req = api.of<RunJobRequest>("RunJob")[0];
    expect(req.definition?.hooks).toMatchObject({
      before: ["stop-paperless"],
      onSuccess: [],
      onFailure: [],
      after: ["start-paperless"],
    });
  });

  it("sends no hooks when none is chosen", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    renderApp(`/jobs/new?kind=restic_check&repository=${m5.ids.photosVault}`, api.transport);
    await user.click(await screen.findByRole("combobox", { name: /^Worker/ }));
    await user.click(await screen.findByRole("option", { name: /nas-01/ }));
    await user.click(screen.getByRole("button", { name: "Run job" }));
    await waitFor(() => expect(api.of("RunJob")).toHaveLength(1));
    expect(api.of<RunJobRequest>("RunJob")[0].definition?.hooks).toBeUndefined();
  });

  it("stores the hooks with a schedule's definition", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    renderApp(`/schedules/new?kind=restic_prune&repository=${m5.ids.photosVault}`, api.transport);
    await user.type(await screen.findByLabelText("Name"), "Monthly prune");
    await user.click(screen.getByRole("combobox", { name: /^Worker/ }));
    await user.click(await screen.findByRole("option", { name: /nas-01/ }));
    await user.click(await screen.findByRole("combobox", { name: "Add a hook: after success" }));
    await user.click(await screen.findByRole("option", { name: /ping-healthcheck/ }));
    await user.click(screen.getByRole("button", { name: "Create schedule" }));
    await waitFor(() => expect(api.of("CreateSchedule")).toHaveLength(1));
    const input = api.of<CreateScheduleRequest>("CreateSchedule")[0].input;
    expect(input?.definition?.hooks?.onSuccess).toEqual(["ping-healthcheck"]);
    expect(input?.target?.target).toEqual({ case: "workerId", value: fx.ids.nas });
  });

  it("shows a job's hooks on its page, with why it failed", async () => {
    const api = m5Api(now);
    renderApp(`/jobs/${hooksMock.ids.hookFailedBackup}`, api.transport);
    expect(await screen.findByText("A hook failed")).toBeVisible();
    expect(
      screen.getByText(/The job itself finished: what it did is kept\. A hook that ran after it/),
    ).toBeVisible();
    expect(screen.getByRole("region", { name: "Hooks: always, at the end" })).toHaveTextContent(
      "start-paperless",
    );
    // The backup was written: its snapshot is still shown.
    expect(screen.getAllByText("7c1e02d9").length).toBeGreaterThan(0);
    expect(
      screen.getByText(
        "Before: stop-paperless, dump-paperless-db · After success: ping-healthcheck · Always: start-paperless",
      ),
    ).toBeVisible();
  });
});
