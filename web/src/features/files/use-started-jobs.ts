import { useCallback, useState } from "react";

import type { TransferOp, Place } from "./transfer";

/** StartedJob is a job a file browser page started and follows until it ends. */
export interface StartedJob {
  jobId: string;
  op: TransferOp;
  dryRun: boolean;
  /** what says what moves where, for the end toast: "3 items into B2 offsite: inbox". */
  what: string;
  /** refresh are the places whose listings the job changes. */
  refresh: Place[];
}

/** useStartedJobs keeps the jobs a page started until each one ends (see StartedJobs). */
export function useStartedJobs() {
  const [jobs, setJobs] = useState<StartedJob[]>([]);
  const add = useCallback((j: StartedJob) => setJobs((p) => [...p, j]), []);
  const done = useCallback(
    (jobId: string) => setJobs((p) => p.filter((j) => j.jobId !== jobId)),
    [],
  );
  return { jobs, add, done };
}
