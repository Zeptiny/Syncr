import { useEffect, useRef } from "react";

import type { Job } from "@/gen/syncr/v1/api_pb";
import { useWatchJob } from "@/hooks/use-watch-job";
import { isTerminal } from "@/lib/status";

/**
 * JobEndWatch follows a job live (WatchJob) and calls onEnd once, with the
 * job, when it reaches a final status. It renders nothing: a page keeps one
 * per job it started, to refresh what the job changed.
 */
export function JobEndWatch({ jobId, onEnd }: { jobId: string; onEnd: (job: Job) => void }) {
  const watch = useWatchJob(jobId, { maxPoints: 1 });
  const done = useRef(false);
  const end = useRef(onEnd);
  useEffect(() => {
    end.current = onEnd;
  });
  const job = watch.job;
  useEffect(() => {
    if (done.current || !job || !isTerminal(job.status)) return;
    done.current = true;
    end.current(job);
  }, [job]);
  return null;
}
