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

import { appendSamples, backoffDelay, reconnectable, useWatchJob } from "./use-watch-job";

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
) {
  let calls = 0;
  const transport = mockTransport(({ service }) => {
    service(JobService, {
      listJobSamples: () => ({ samples: [progress(0, 0), progress(5, 500)] }),
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
  return { hook, calls: () => calls, queryClient };
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

  it("appends only newer samples and caps the history", () => {
    const a = [{ t: 1, bytes: 1, files: 0, errors: 0 }];
    const next = appendSamples(a, [a[0], { t: 2, bytes: 2, files: 0, errors: 0 }], 10);
    expect(next.map((s) => s.t)).toEqual([1, 2]);
    expect(
      appendSamples(next, [{ t: 3, bytes: 3, files: 0, errors: 0 }], 2).map((s) => s.t),
    ).toEqual([2, 3]);
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
