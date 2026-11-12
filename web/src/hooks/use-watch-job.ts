import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, createClient } from "@connectrpc/connect";
import { createConnectQueryKey, useTransport } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useState } from "react";

import {
  GetJobResponseSchema,
  JobService,
  type Job,
  type JobProgress,
} from "@/gen/syncr/v1/api_pb";
import { toNumber, tsDate } from "@/lib/format";
import { reportUnauthenticated } from "@/lib/query-client";
import { isTerminal } from "@/lib/status";

/** JobSample is one cumulative progress sample; t is in epoch seconds. */
export interface JobSample {
  t: number;
  bytes: number;
  files: number;
  errors: number;
}

/**
 * WatchState is the state of the live stream. "ended" is a clean end: the job
 * reached a terminal status. "failed" is a stop on an error that is not
 * retried automatically (see WatchResult.error and WatchResult.retry).
 */
export type WatchState = "connecting" | "live" | "reconnecting" | "ended" | "failed";

/** WatchOptions tunes the reconnect backoff (tests shorten it). */
export interface WatchOptions {
  minBackoffMs?: number;
  maxBackoffMs?: number;
  /**
   * maxSamples caps the kept samples: past it, the history is thinned
   * evenly (first and last kept), so the chart keeps the job's start.
   */
  maxSamples?: number;
  /** maxPoints is how many stored samples to ask for (ListJobSamples.max_points). */
  maxPoints?: number;
}

/** HISTORY_POINTS is how many stored samples the chart asks for. */
export const HISTORY_POINTS = 1000;

/** WatchResult is what useWatchJob returns. */
export interface WatchResult {
  /** job is the latest job message from the stream, if any yet. */
  job: Job | undefined;
  /** samples is the chart history followed by live samples, oldest first. */
  samples: JobSample[];
  /**
   * history is "loading" until the stored samples have been read, then
   * "loaded" (or "failed": the chart then has only live samples).
   */
  history: "loading" | "loaded" | "failed";
  /** total is how many samples the controller stores for the job. */
  total: number;
  /** thinned is true when samples leaves some out: the stored ones, or older live ones. */
  thinned: boolean;
  state: WatchState;
  /** attempt counts consecutive failed connections (0 while live). */
  attempt: number;
  /** error is set when the stream stopped for a non-transient reason. */
  error: unknown;
  /** retry starts the stream again after it failed. */
  retry: () => void;
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

/**
 * thinSamples keeps m of the samples, evenly spread, always with the first
 * and the last: the rule the controller thins stored samples with (the
 * sample at rank r stays if r*(m-1)/(n-1) moves past (r-1)*(m-1)/(n-1),
 * in integer division).
 */
export function thinSamples<T>(samples: T[], m: number): T[] {
  const n = samples.length;
  if (m <= 0 || n <= m) return samples;
  if (m === 1) return [samples[n - 1]];
  const out: T[] = [];
  for (let r = 0; r < n; r++) {
    if (r === 0 || Math.floor((r * (m - 1)) / (n - 1)) > Math.floor(((r - 1) * (m - 1)) / (n - 1)))
      out.push(samples[r]);
  }
  return out;
}

/**
 * appendSamples adds samples newer than the last one. Past max it thins the
 * whole history to half of max (first and last kept) instead of dropping
 * the oldest, so a long job's chart still starts at its start.
 */
export function appendSamples(prev: JobSample[], add: JobSample[], max: number): JobSample[] {
  return mergeSamples(prev, add, max).samples;
}

/** mergeSamples is appendSamples that also tells whether it thinned. */
function mergeSamples(
  prev: JobSample[],
  add: JobSample[],
  max: number,
): { samples: JobSample[]; thinned: boolean } {
  let last = prev.length ? prev[prev.length - 1].t : -Infinity;
  const fresh: JobSample[] = [];
  for (const s of add) {
    if (s.t > last) {
      fresh.push(s);
      last = s.t;
    }
  }
  if (!fresh.length) return { samples: prev, thinned: false };
  const next = prev.concat(fresh);
  if (next.length <= max) return { samples: next, thinned: false };
  return { samples: thinSamples(next, Math.max(2, Math.floor(max / 2))), thinned: true };
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
 * backoff (1 s doubling to 30 s); it stops once the job is terminal. Any
 * other error ends in state "failed" with the error (retry restarts the
 * stream), and UNAUTHENTICATED also goes through the session-expiry path.
 */
export function useWatchJob(jobId: string, opts: WatchOptions = {}): WatchResult {
  const transport = useTransport();
  const queryClient = useQueryClient();
  const {
    minBackoffMs = 1000,
    maxBackoffMs = 30_000,
    maxSamples = 5000,
    maxPoints = HISTORY_POINTS,
  } = opts;
  const [nonce, setNonce] = useState(0);
  const retry = useCallback(() => setNonce((n) => n + 1), []);
  const [result, setResult] = useState<Omit<WatchResult, "retry"> & { jobId: string }>(() =>
    freshResult(jobId),
  );

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
    const update = (patch: Partial<Omit<WatchResult, "retry">>) =>
      setResult((r) => (signal.aborted ? r : { ...r, ...patch }));
    const addSamples = (add: JobSample[]) =>
      setResult((r) => {
        if (signal.aborted) return r;
        const m = mergeSamples(r.samples, add, maxSamples);
        return m.samples === r.samples
          ? r
          : { ...r, samples: m.samples, thinned: r.thinned || m.thinned };
      });

    async function loadHistory() {
      try {
        const res = await client.listJobSamples({ jobId, maxPoints }, { signal });
        const history = res.samples.map(toSample).filter((s): s is JobSample => !!s);
        // History is older than anything the stream delivers; merge by time.
        setResult((r) => {
          if (signal.aborted) return r;
          const live = r.samples.filter(
            (s) => !history.length || s.t > history[history.length - 1].t,
          );
          const m = mergeSamples(history, live, maxSamples);
          return {
            ...r,
            samples: m.samples,
            history: "loaded",
            total: res.total,
            thinned: r.thinned || m.thinned || res.total > res.samples.length,
          };
        });
      } catch {
        // The chart starts empty; the live stream still fills it.
        setResult((r) => (signal.aborted ? r : { ...r, history: "failed" }));
      }
    }

    async function watch() {
      let attempt = 0;
      while (!signal.aborted) {
        update(
          attempt === 0
            ? { state: "connecting", attempt, error: undefined }
            : { state: "reconnecting", attempt },
        );
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
            update({ state: "failed", error: err });
            reportUnauthenticated(queryClient, err);
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
  }, [jobId, transport, queryClient, minBackoffMs, maxBackoffMs, maxSamples, maxPoints, nonce]);

  // A new jobId starts from a clean slate until its effect reports.
  if (result.jobId !== jobId) {
    const fresh = freshResult(jobId);
    setResult(fresh);
    return { ...fresh, retry };
  }
  return { ...result, retry };
}

function freshResult(jobId: string): Omit<WatchResult, "retry"> & { jobId: string } {
  return {
    jobId,
    job: undefined,
    samples: [],
    history: "loading",
    total: 0,
    thinned: false,
    state: "connecting",
    attempt: 0,
    error: undefined,
  };
}
