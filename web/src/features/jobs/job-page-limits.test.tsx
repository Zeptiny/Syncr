import { create } from "@bufbuild/protobuf";
import { createConnectQueryKey } from "@connectrpc/connect-query";
import { act, renderHook, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";

import { SearchInput } from "@/components/app/filter-bar";
import {
  JobSchema,
  JobService,
  JobStatus,
  WorkerAdminService,
  type Job,
  type JobProgress,
} from "@/gen/syncr/v1/api_pb";
import { ResticService } from "@/gen/syncr/v1/restic_pb";
import { useNow } from "@/hooks/use-now";
import * as m5 from "@/mock/m5";
import * as m6 from "@/mock/m6";
import { signedIn } from "@/test/app-api";
import * as fx from "@/test/fixtures";
import { m5Api } from "@/test/m5-api";
import { mockTransport, renderApp, renderWithProviders } from "@/test/render";

// uPlot needs a canvas, which jsdom lacks; the note under the chart is
// what these tests read.
vi.mock("uplot", () => ({
  default: class {
    setData() {}
    setSize() {}
    destroy() {}
  },
}));

const now = Date.now();

/** jobApi serves one finished job and its stored samples, like the controller. */
function jobApi(job: Job, samples: JobProgress[]) {
  const maxPoints: number[] = [];
  const transport = mockTransport((router) => {
    signedIn(router, now);
    router.service(WorkerAdminService, { listWorkers: () => ({ workers: fx.workers(now) }) });
    router.service(JobService, {
      getJob: () => ({ job }),
      listJobSamples: (req) => {
        maxPoints.push(req.maxPoints);
        return m6.listSamples(samples, req.maxPoints);
      },
      async *watchJob() {
        await Promise.resolve();
        yield { update: { case: "job" as const, value: job } };
      },
    });
  });
  return { transport, maxPoints };
}

describe("job page: time limits and progress history", { timeout: 30_000 }, () => {
  it("reads a failure by time limit as such, with the limit and a thinned chart", async () => {
    const { job, samples } = m6.timeLimitJob(now);
    const { transport, maxPoints } = jobApi(job, samples);
    renderApp(`/jobs/${job.id}`, transport);

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("Time limit reached");
    expect(alert).toHaveTextContent("It ran longer than its time limit of 6h");
    expect(alert).toHaveTextContent(m6.TIME_LIMIT_MESSAGE);
    expect(screen.getByText("Time limit 6h")).toBeVisible();
    // The Details list names the effective limit.
    const details = screen.getByRole("heading", { name: "Details" }).closest("section")!;
    expect(within(details).getByText("Time limit")).toBeVisible();
    expect(within(details).getByText("6h")).toBeVisible();

    await waitFor(() =>
      expect(screen.getByText(/^Thinned: 1,000 of 4,320 progress samples shown/)).toBeVisible(),
    );
    expect(maxPoints).toEqual([1000]);
  });

  it("says a finished job's history is gone when it has no samples", async () => {
    const job = fx.jobs(now).find((j) => j.status === JobStatus.SUCCEEDED)!;
    const { transport } = jobApi(job, []);
    renderApp(`/jobs/${job.id}`, transport);
    expect(
      await screen.findByText("No progress history (it is kept for a limited time)"),
    ).toBeVisible();
    expect(screen.getByText("None")).toBeVisible(); // no time limit
  });

  it("refreshes the repository queries when a restic job runs again", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    const done = api.jobs.find(
      (j) => j.kind === "restic_check" && j.status === JobStatus.SUCCEEDED,
    )!;
    const { queryClient } = renderApp(`/jobs/${done.id}`, api.transport);
    const spy = vi.spyOn(queryClient, "invalidateQueries");
    await user.click(await screen.findByRole("button", { name: "Run again" }));
    await waitFor(() => expect(api.of("RunJob")).toHaveLength(1));
    const restic = createConnectQueryKey({ schema: ResticService, cardinality: undefined });
    await waitFor(() =>
      expect(spy).toHaveBeenCalledWith(expect.objectContaining({ queryKey: restic })),
    );
  });
});

describe("useNow", () => {
  afterEach(() => vi.useRealTimers());

  it("stops ticking while inactive", () => {
    vi.useFakeTimers();
    let renders = 0;
    const { result, rerender } = renderHook(
      ({ active }) => {
        renders++;
        return useNow(1000, active);
      },
      { initialProps: { active: false } },
    );
    const first = result.current;
    act(() => void vi.advanceTimersByTime(5000));
    expect(result.current).toBe(first);
    expect(renders).toBe(1);
    rerender({ active: true });
    act(() => void vi.advanceTimersByTime(1000));
    expect(result.current).not.toBe(first);
  });
});

describe("SearchInput", () => {
  it("keeps a keystroke typed while its value is being committed", async () => {
    const user = userEvent.setup();
    const sent: string[] = [];
    let release: (() => void) | undefined;
    // The parent takes its time to store what was reported (a navigation).
    function Parent() {
      const [value, setValue] = useState("");
      return (
        <SearchInput
          value={value}
          delayMs={20}
          placeholder="Search jobs"
          onChange={(v) => {
            sent.push(v);
            release = () => setValue(v);
          }}
        />
      );
    }
    renderWithProviders(<Parent />);
    const box = screen.getByRole("searchbox", { name: "Search jobs" });
    await user.type(box, "ab");
    await waitFor(() => expect(sent).toEqual(["ab"]));
    await user.type(box, "c");
    act(() => release?.());
    expect(box).toHaveValue("abc");
    await waitFor(() => expect(sent).toEqual(["ab", "abc"]));
    act(() => release?.());
    expect(box).toHaveValue("abc");
  });

  it("takes a value changed elsewhere, such as cleared filters", async () => {
    function Parent() {
      const [value, setValue] = useState("photos");
      return (
        <>
          <SearchInput value={value} onChange={setValue} placeholder="Search jobs" />
          <button onClick={() => setValue("")}>Clear</button>
        </>
      );
    }
    const user = userEvent.setup();
    renderWithProviders(<Parent />);
    const box = screen.getByRole("searchbox", { name: "Search jobs" });
    expect(box).toHaveValue("photos");
    await user.click(screen.getByRole("button", { name: "Clear" }));
    expect(box).toHaveValue("");
  });
});

describe("remote delete dialog", { timeout: 30_000 }, () => {
  it("disables Delete while repositories are on the remote", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    renderApp(`/remotes/${fx.ids.b2}`, api.transport);
    await user.click(await screen.findByRole("button", { name: "Delete remote" }));
    const dialog = await screen.findByRole("alertdialog");
    await waitFor(() => expect(dialog).toHaveTextContent("Photos vault"));
    expect(within(dialog).getByRole("button", { name: "Delete remote" })).toBeDisabled();
    expect(api.data.repositories.some((r) => r.repo.id === m5.ids.photosVault)).toBe(true);
  });

  it("enables it for a remote nothing depends on", async () => {
    const user = userEvent.setup();
    const api = m5Api(now);
    renderApp(`/remotes/${fx.ids.nextcloud}`, api.transport);
    await user.click(await screen.findByRole("button", { name: "Delete remote" }));
    const dialog = await screen.findByRole("alertdialog");
    await waitFor(() =>
      expect(within(dialog).getByRole("button", { name: "Delete remote" })).toBeEnabled(),
    );
  });
});

// The created job of the m6 fixture must stay a valid Job message.
it("builds the time-limit job as a proper message", () => {
  const { job } = m6.timeLimitJob(now);
  expect(create(JobSchema, job).timeLimit?.seconds).toBe(21600n);
});
