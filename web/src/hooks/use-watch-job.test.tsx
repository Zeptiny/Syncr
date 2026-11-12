import { create } from "@bufbuild/protobuf";
import { timestampFromMs } from "@bufbuild/protobuf/wkt";
import { Code, ConnectError } from "@connectrpc/connect";
import { TransportProvider } from "@connectrpc/connect-query";
import { QueryClientProvider, type QueryClient } from "@tanstack/react-query";
import { act, renderHook, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

import type { MessageInitShape } from "@bufbuild/protobuf";

import {
  JobProgressSchema,
  JobSchema,
  JobService,
  JobStatus,
  type WatchJobResponseSchema,
} from "@/gen/syncr/v1/api_pb";
import { createQueryClient } from "@/lib/query-client";
import { mockTransport, testQueryClient } from "@/test/render";

import {
  appendSamples,
  backoffDelay,
  HISTORY_POINTS,
  reconnectable,
  thinSamples,
  useWatchJob,
} from "./use-watch-job";

type WatchJobResponseInit = MessageInitShape<typeof WatchJobResponseSchema>;

const t0 = Date.parse("2026-03-12T12:00:00Z");
const job = (status: JobStatus) => create(JobSchema, { id: "j1", status, kind: "rclone_transfer" });
const progress = (sec: number, bytes: number) =>
  create(JobProgressSchema, {
    time: timestampFromMs(t0 + sec * 1000),
    stats: { bytes: BigInt(bytes) },
  });

function setup(
  watch: (call: number) => AsyncIterable<unknown>,
  queryClient: QueryClient = testQueryClient(),
  total = 2,
) {
  let calls = 0;
  const sampleRequests: number[] = [];
  const transport = mockTransport(({ service }) => {
    service(JobService, {
      listJobSamples: (req) => {
        sampleRequests.push(req.maxPoints);
        return { samples: [progress(0, 0), progress(5, 500)], total };
      },
      watchJob: () => watch(++calls) as AsyncIterable<WatchJobResponseInit>,
    });
  });
  const wrapper = ({ children }: { children: React.ReactNode }) => (
    <TransportProvider transport={transport}>
      <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>
    </TransportProvider>
  );
  const hook = renderHook(() => useWatchJob("j1", { minBackoffMs: 5, maxBackoffMs: 20 }), {
    wrapper,
  });
  return { hook, calls: () => calls, queryClient, sampleRequests };
}

describe("backoffDelay", () => {
  it("doubles from 1 s and caps at 30 s", () => {
    expect([0, 1, 2, 3, 4, 5, 6].map((n) => backoffDelay(n))).toEqual([
      1000, 2000, 4000, 8000, 16000, 30000, 30000,
    ]);
  });

  it("reconnects only on transient errors", () => {
    expect(reconnectable(new ConnectError("x", Code.Unavailable))).toBe(true);
    expect(reconnectable(new ConnectError("x", Code.DeadlineExceeded))).toBe(true);
    expect(reconnectable(new ConnectError("x", Code.PermissionDenied))).toBe(false);
    expect(reconnectable(new ConnectError("x", Code.NotFound))).toBe(false);
  });

  it("appends only newer samples and thins past the cap, keeping the start", () => {
    const at = (t: number) => ({ t, bytes: t, files: 0, errors: 0 });
    const a = [at(1)];
    const next = appendSamples(a, [a[0], at(2)], 10);
    expect(next.map((s) => s.t)).toEqual([1, 2]);
    // Past the cap the history is thinned, never cut at its start.
    expect(appendSamples(next, [at(3)], 2).map((s) => s.t)).toEqual([1, 3]);
    const long = Array.from({ length: 5000 }, (_, i) => at(i));
    const thinned = appendSamples(long, [at(5000)], 5000);
    expect(thinned).toHaveLength(2500);
    expect(thinned[0].t).toBe(0);
    expect(thinned.at(-1)?.t).toBe(5000);
  });

  it("thins like the controller: evenly, first and last kept, exactly m", () => {
    const xs = Array.from({ length: 10 }, (_, i) => i);
    expect(thinSamples(xs, 4)).toEqual([0, 3, 6, 9]);
    expect(thinSamples(xs, 2)).toEqual([0, 9]);
    expect(thinSamples(xs, 10)).toBe(xs);
    expect(thinSamples(xs, 0)).toBe(xs);
    for (const m of [2, 3, 7, 999]) {
      const n = 1000 + m;
      const got = thinSamples(
        Array.from({ length: n }, (_, i) => i),
        m,
      );
      expect(got).toHaveLength(m);
      expect(got[0]).toBe(0);
      expect(got.at(-1)).toBe(n - 1);
    }
  });
});

describe("useWatchJob", () => {
  it("reconnects after UNAVAILABLE and stops once the job is terminal", async () => {
    const { hook, calls, queryClient } = setup((n) => {
      if (n === 1) {
        return (async function* () {
          await Promise.resolve();
          yield* [];
          throw new ConnectError("controller shutting down", Code.Unavailable);
        })();
      }
      if (n === 2) {
        return (async function* () {
          await Promise.resolve();
          yield { update: { case: "job", value: job(JobStatus.RUNNING) } };
          yield { update: { case: "progress", value: progress(10, 900) } };
          throw new ConnectError("watch limit", Code.DeadlineExceeded);
        })();
      }
      return (async function* () {
        await Promise.resolve();
        yield { update: { case: "job", value: job(JobStatus.RUNNING) } };
        yield { update: { case: "progress", value: progress(15, 1400) } };
        yield { update: { case: "job", value: job(JobStatus.SUCCEEDED) } };
      })();
    });

    await waitFor(() => expect(hook.result.current.state).toBe("ended"));
    expect(calls()).toBe(3);
    expect(hook.result.current.error).toBeUndefined();
    expect(hook.result.current.job?.status).toBe(JobStatus.SUCCEEDED);
    expect(hook.result.current.samples.map((s) => s.bytes)).toEqual([0, 500, 900, 1400]);
    // Job messages land in the GetJob cache.
    const cached = queryClient.getQueriesData({});
    expect(
      JSON.stringify(cached, (_k, v: unknown) => (typeof v === "bigint" ? String(v) : v)),
    ).toContain('"status":4');

    // Nothing reconnects after the end.
    await new Promise((r) => setTimeout(r, 60));
    expect(calls()).toBe(3);
  });

  it("asks for a bounded history and says when the controller thinned it", async () => {
    const ended = () =>
      (async function* () {
        await Promise.resolve();
        yield { update: { case: "job", value: job(JobStatus.SUCCEEDED) } };
      })();
    const full = setup(ended);
    await waitFor(() => expect(full.hook.result.current.history).toBe("loaded"));
    expect(full.sampleRequests).toEqual([HISTORY_POINTS]);
    expect(full.hook.result.current.total).toBe(2);
    expect(full.hook.result.current.thinned).toBe(false);

    const thinned = setup(ended, testQueryClient(), 7200);
    await waitFor(() => expect(thinned.hook.result.current.history).toBe("loaded"));
    expect(thinned.hook.result.current.total).toBe(7200);
    expect(thinned.hook.result.current.thinned).toBe(true);
  });

  it("gives up on a permanent error", async () => {
    const { hook, calls } = setup(() =>
      (async function* () {
        await Promise.resolve();
        yield* [];
        throw new ConnectError("not yours", Code.NotFound);
      })(),
    );
    await waitFor(() => expect(hook.result.current.state).toBe("failed"));
    expect(calls()).toBe(1);
    expect(hook.result.current.error).toBeInstanceOf(ConnectError);
  });

  it("does not report a failed stream as ended, and can retry it", async () => {
    const { hook, calls } = setup((n) =>
      (async function* () {
        await Promise.resolve();
        if (n === 1) throw new ConnectError("not allowed", Code.PermissionDenied);
        yield { update: { case: "job", value: job(JobStatus.SUCCEEDED) } };
      })(),
    );
    await waitFor(() => expect(hook.result.current.state).toBe("failed"));
    expect(hook.result.current.state).not.toBe("ended");

    act(() => hook.result.current.retry());
    await waitFor(() => expect(hook.result.current.state).toBe("ended"));
    expect(calls()).toBe(2);
    expect(hook.result.current.error).toBeUndefined();
  });

  it("sends UNAUTHENTICATED through the session-expiry path", async () => {
    const expired = vi.fn();
    const client = createQueryClient({ onUnauthenticated: expired });
    const { hook, calls } = setup(
      () =>
        (async function* () {
          await Promise.resolve();
          yield* [];
          throw new ConnectError("session expired", Code.Unauthenticated);
        })(),
      client,
    );
    await waitFor(() => expect(hook.result.current.state).toBe("failed"));
    expect(expired).toHaveBeenCalledTimes(1);
    expect(expired).toHaveBeenCalledWith(client);
    expect(calls()).toBe(1);
  });

  it("reconnects when the stream closes before the job ends", async () => {
    const { hook, calls } = setup((n) =>
      (async function* () {
        await Promise.resolve();
        yield { update: { case: "job", value: job(n < 3 ? JobStatus.RUNNING : JobStatus.FAILED) } };
      })(),
    );
    await waitFor(() => expect(hook.result.current.state).toBe("ended"));
    expect(calls()).toBe(3);
  });
});
