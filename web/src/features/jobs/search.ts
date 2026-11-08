// The jobs list's URL state lives apart from the page, so the route can
// validate it without pulling the page into the main chunk.

/** JobsSearch is the jobs list's URL state. */
export interface JobsSearch {
  /** status is a comma-separated list of JobStatus numbers. */
  status?: string;
  worker?: string;
  remote?: string;
  schedule?: string;
  kind?: string;
  q?: string;
  period?: "24h" | "7d" | "30d" | "custom";
  from?: string;
  to?: string;
  all?: boolean;
}

/** validateJobsSearch parses the URL search params of the jobs list. */
export function validateJobsSearch(s: Record<string, unknown>): JobsSearch {
  const str = (v: unknown) => (typeof v === "string" && v ? v : undefined);
  const date = (v: unknown) =>
    typeof v === "string" && /^\d{4}-\d{2}-\d{2}$/.test(v) ? v : undefined;
  const period = str(s.period);
  return {
    status: str(s.status),
    worker: str(s.worker),
    remote: str(s.remote),
    schedule: str(s.schedule),
    kind: str(s.kind),
    q: str(s.q),
    period:
      period === "24h" || period === "7d" || period === "30d" || period === "custom"
        ? period
        : undefined,
    from: date(s.from),
    to: date(s.to),
    all: s.all === true || s.all === "true" ? true : undefined,
  };
}
