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
  /** repository limits the list to one repository's jobs. */
  repository?: string;
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
    repository: str(s.repository),
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

/** The kinds a form can create; a restore or a data deletion is never among them. */
const formKinds = [
  "rclone_transfer",
  "restic_backup",
  "restic_forget",
  "restic_check",
  "restic_prune",
] as const;

/** FormKind is a job kind the Run job page and the schedule form offer. */
export type FormKind = (typeof formKinds)[number];

/** formKind parses a kind from the URL; anything else, a restore included, is dropped. */
export function formKind(v: unknown): FormKind | undefined {
  return formKinds.find((k) => k === v);
}

/** RunJobSearch is the URL state of "Run a job". */
export interface RunJobSearch {
  /** from is an earlier job to run again with changes. */
  from?: string;
  kind?: FormKind;
  /** repository preselects a repository for the restic kinds. */
  repository?: string;
}

/** validateRunJobSearch parses /jobs/new's search params. */
export function validateRunJobSearch(s: Record<string, unknown>): RunJobSearch {
  const str = (v: unknown) => (typeof v === "string" && v ? v : undefined);
  return { from: str(s.from), kind: formKind(s.kind), repository: str(s.repository) };
}
