import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useTransport } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";

import {
  GetJobResponseSchema,
  JobService,
  type Job,
  type JobProgress,
} from "@/gen/syncr/v1/api_pb";
import { toNumber, tsDate } from "@/lib/format";
import { isTerminal } from "@/lib/status";

/** JobSample is one cumulative progress sample; t is in epoch seconds. */
export interface JobSample {
  t: number;
  bytes: number;
  files: number;
  errors: number;
}

/** WatchState is the state of the live stream. */
export type WatchState = "connecting" | "live" | "reconnecting" | "ended";

/** WatchOptions tunes the reconnect backoff (tests shorten it). */
export interface WatchOptions {
  minBackoffMs?: number;
  maxBackoffMs?: number;
  /** maxSamples caps the kept history; the oldest samples are dropped. */
  maxSamples?: number;
}

/** WatchResult is what useWatchJob returns. */
export interface WatchResult {
  /** job is the latest job message from the stream, if any yet. */
  job: Job | undefined;
  /** samples is the chart history followed by live samples, oldest first. */
  samples: JobSample[];
  state: WatchState;
  /** attempt counts consecutive failed connections (0 while live). */
  attempt: number;
  /** error is set when the stream stopped for a non-transient reason. */
  error: unknown;
}

/**
 * backoffDelay is the wait before reconnect attempt n (0-based): it doubles
 * from min to max (1 s to 30 s by default).
 */
export function backoffDelay(attempt: number, min = 1000, max = 30_000): number {
  return Math.min(max, min * 2 ** Math.max(0, attempt));
}

/** reconnectable reports whether a stream error is worth a reconnect. */
export function reconnectable(err: unknown): boolean {
  if (!(err instanceof ConnectError)) return true;
  return (
    err.code === Code.DeadlineExceeded || err.code === Code.Unavailable || err.code === Code.Unknown
  );
}

/** toSample converts a progress message; undefined if it has no time. */
export function toSample(p: JobProgress): JobSample | undefined {
  const at = tsDate(p.time);
  if (!at) return undefined;
  return {
    t: at.getTime() / 1000,
    bytes: toNumber(p.stats?.bytes),
    files: toNumber(p.stats?.files),
    errors: toNumber(p.stats?.errors),
  };
}

/** appendSamples adds samples newer than the last one, keeping at most max. */
export function appendSamples(prev: JobSample[], add: JobSample[], max: number): JobSample[] {
  let last = prev.length ? prev[prev.length - 1].t : -Infinity;
  const fresh: JobSample[] = [];
  for (const s of add) {
    if (s.t > last) {
      fresh.push(s);
      last = s.t;
    }
  }
  if (!fresh.length) return prev;
  const next = prev.concat(fresh);
  return next.length > max ? next.slice(next.length - max) : next;
}

function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const id = setTimeout(resolve, ms);
    signal.addEventListener(
      "abort",
      () => {
        clearTimeout(id);
        resolve();
      },
      { once: true },
    );
  });
}

/**
 * useWatchJob follows a job live: it loads the stored samples
 * (ListJobSamples), then streams WatchJob. Job messages also update the
 * GetJob query cache, so views reading the job re-render. On
 * DEADLINE_EXCEEDED, UNAVAILABLE or a dropped connection it reconnects with
 * backoff (1 s doubling to 30 s); it stops once the job is terminal.
 */
export function useWatchJob(jobId: string, opts: WatchOptions = {}): WatchResult {
  const transport = useTransport();
  const queryClient = useQueryClient();
  const { minBackoffMs = 1000, maxBackoffMs = 30_000, maxSamples = 5000 } = opts;
  const [result, setResult] = useState<WatchResult & { jobId: string }>({
    jobId,
    job: undefined,
    samples: [],
    state: "connecting",
    attempt: 0,
    error: undefined,
  });

  useEffect(() => {
    const ctrl = new AbortController();
    const { signal } = ctrl;
    const client = createClient(JobService, transport);
    const jobKey = createConnectQueryKey({
      schema: JobService.method.getJob,
      input: { jobId },
      transport,
      cardinality: "finite",
    });
    const update = (patch: Partial<WatchResult>) =>
      setResult((r) => (signal.aborted ? r : { ...r, ...patch }));
    const addSamples = (add: JobSample[]) =>
      setResult((r) =>
        signal.aborted ? r : { ...r, samples: appendSamples(r.samples, add, maxSamples) },
      );

    async function loadHistory() {
      try {
        const res = await client.listJobSamples({ jobId }, { signal });
        const history = res.samples.map(toSample).filter((s): s is JobSample => !!s);
        // History is older than anything the stream delivers; merge by time.
        setResult((r) =>
          signal.aborted
            ? r
            : {
                ...r,
                samples: appendSamples(
                  history,
                  r.samples.filter((s) => !history.length || s.t > history[history.length - 1].t),
                  maxSamples,
                ),
              },
        );
      } catch {
        // The chart starts empty; the live stream still fills it.
      }
    }

    async function watch() {
      let attempt = 0;
      while (!signal.aborted) {
        update({ state: attempt === 0 ? "connecting" : "reconnecting", attempt });
        let terminal = false;
        let connected = false;
        try {
          for await (const msg of client.watchJob({ jobId }, { signal })) {
            if (!connected) {
              connected = true;
              attempt = 0;
              update({ state: "live", attempt: 0, error: undefined });
            }
            if (msg.update.case === "job") {
              const job = msg.update.value;
              terminal = isTerminal(job.status);
              update({ job });
              queryClient.setQueryData(jobKey, create(GetJobResponseSchema, { job }));
            } else if (msg.update.case === "progress") {
              const s = toSample(msg.update.value);
              if (s) addSamples([s]);
            }
          }
          if (terminal) {
            update({ state: "ended" });
            return;
          }
          // The stream ended without a terminal job: reconnect.
        } catch (err) {
          if (signal.aborted) return;
          if (terminal) {
            update({ state: "ended" });
            return;
          }
          if (!reconnectable(err)) {
            update({ state: "ended", error: err });
            return;
          }
        }
        const delay = backoffDelay(attempt, minBackoffMs, maxBackoffMs);
        attempt++;
        update({ state: "reconnecting", attempt });
        await sleep(delay, signal);
      }
    }

    void loadHistory();
    void watch();
    return () => ctrl.abort();
  }, [jobId, transport, queryClient, minBackoffMs, maxBackoffMs, maxSamples]);

  // A new jobId starts from a clean slate until its effect reports.
  if (result.jobId !== jobId) {
    const fresh = {
      jobId,
      job: undefined,
      samples: [],
      state: "connecting",
      attempt: 0,
      error: undefined,
    } as const;
    setResult({ ...fresh, samples: [] });
    return { ...fresh, samples: [] };
  }
  return result;
}
