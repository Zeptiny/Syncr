import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { toast } from "sonner";

import { JobService, JobStatus, type Job } from "@/gen/syncr/v1/api_pb";
import { invalidateService } from "@/lib/invalidate";

import { JobEndWatch } from "./job-end-watch";
import { invalidatePlaces } from "./refresh";
import type { StartedJob } from "./use-started-jobs";

/**
 * StartedJobs follows the transfers a page started. When one ends it says
 * so, with a link to the job, and refreshes the listings it changed (none
 * for a preview).
 */
export function StartedJobs({
  jobs,
  onDone,
}: {
  jobs: StartedJob[];
  onDone: (jobId: string) => void;
}) {
  const queryClient = useQueryClient();
  const navigate = useNavigate();
  const ended = async (s: StartedJob, job: Job) => {
    onDone(s.jobId);
    const view = {
      label: "View job",
      onClick: () => void navigate({ to: "/jobs/$jobId", params: { jobId: s.jobId } }),
    };
    const name = s.dryRun ? "preview" : s.op;
    switch (job.status) {
      case JobStatus.SUCCEEDED:
        if (s.dryRun)
          toast.success("Preview finished", {
            description: "The job's page lists what would be copied.",
            action: view,
          });
        else toast.success(`${s.op === "move" ? "Moved" : "Copied"} ${s.what}`, { action: view });
        break;
      case JobStatus.CANCELLED:
        toast.info(`The ${name} was cancelled`, { action: view });
        break;
      default:
        toast.error(`The ${name} failed`, { description: job.error?.message, action: view });
    }
    await Promise.all([
      invalidateService(queryClient, JobService),
      s.dryRun ? undefined : invalidatePlaces(queryClient, s.refresh),
    ]);
  };
  return (
    <>
      {jobs.map((s) => (
        <JobEndWatch key={s.jobId} jobId={s.jobId} onEnd={(job) => void ended(s, job)} />
      ))}
    </>
  );
}
