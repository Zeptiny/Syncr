import type { JobSample } from "@/hooks/use-watch-job";
import { rateBetween } from "@/lib/format";

/** PhaseChange is where a new phase starts: the first sample of it and when it began (seconds). */
export interface PhaseChange {
  index: number;
  at: number;
  /** exact: the time is known (from the job's result), not guessed between two samples. */
  exact?: boolean;
}

/**
 * phaseChanges finds where a job's phases change. A backup of a remote
 * mirrors, then backs up, and the counters restart at the change. Live
 * samples name their phase; stored ones don't, so known holds the times
 * the job's result gives (the mirror's end) and, without them, a drop of
 * the cumulative bytes marks a change (within a phase they only grow).
 */
export function phaseChanges(samples: JobSample[], known: number[] = []): PhaseChange[] {
  const out = new Map<number, PhaseChange>();
  for (let i = 1; i < samples.length; i++) {
    const a = samples[i - 1];
    const b = samples[i];
    const guess = { index: i, at: (a.t + b.t) / 2 };
    const c = known.find((k) => a.t < k && k <= b.t);
    // Strictly before the next sample, so the time axis keeps increasing.
    const exact =
      c === undefined
        ? undefined
        : { index: i, at: Math.min(c, b.t - (b.t - a.t) / 1000), exact: true };
    if (exact) out.set(i, exact);
    else if (a.phase && b.phase && a.phase !== b.phase) out.set(i, guess);
    else if (known.length === 0 && b.bytes < a.bytes) out.set(i, guess);
  }
  return [...out.values()];
}

/** ChartData is uPlot's [time, throughput, bytes]. */
export type ChartData = [number[], (number | null)[], (number | null)[]];

/**
 * chartData turns cumulative samples into [time, throughput, bytes]. At a
 * phase change both series break (an empty point where the phase changed,
 * where the chart draws its marker), so neither line runs across the
 * restart and no rate is taken across it; after a change at a known time,
 * the first rate counts from that time, when the counters were zero.
 */
export function chartData(samples: JobSample[], changes: PhaseChange[]): ChartData {
  const t: number[] = [];
  const rate: (number | null)[] = [];
  const bytes: (number | null)[] = [];
  samples.forEach((s, i) => {
    const change = changes.find((c) => c.index === i);
    if (change) {
      t.push(change.at);
      rate.push(null);
      bytes.push(null);
    }
    t.push(s.t);
    if (change?.exact) rate.push(rateBetween({ t: change.at, bytes: 0 }, s));
    else rate.push(i === 0 || change ? null : rateBetween(samples[i - 1], s));
    bytes.push(s.bytes);
  });
  return [t, rate, bytes];
}

/**
 * averageRate is a finished job's average throughput in its last phase:
 * from the job's start (or the first sample) or, after a phase change,
 * from the change, when the counters started again.
 */
export function averageRate(
  samples: JobSample[],
  changes: PhaseChange[],
  startedAt?: number,
): number {
  const last = samples.at(-1);
  if (!last) return 0;
  const change = changes.at(-1);
  if (change) return rateBetween({ t: change.at, bytes: 0 }, last);
  if (startedAt !== undefined && startedAt < samples[0].t) {
    return rateBetween({ t: startedAt, bytes: 0 }, last);
  }
  return rateBetween(samples[0], last);
}
