import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { RefreshCw, ShieldAlert } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { ConfirmDialog } from "@/components/app/confirm-dialog";
import { Button } from "@/components/ui/button";
import { JobService, type Job } from "@/gen/syncr/v1/api_pb";
import { ScheduleService } from "@/gen/syncr/v1/schedules_pb";
import { invalidateService } from "@/lib/invalidate";

import {
  bisyncOf,
  recoveryFor,
  resyncModeLabel,
  withOneShot,
  type OneShot,
  type Recovery,
} from "./bisync-definition";

/** recoveryHint says what a failed two-way sync means and what to do about it. */
export function recoveryHint(r: Recovery): string {
  switch (r) {
    case "resync":
      return "rclone needs a resync before this pair can sync again: its filters changed, or a run could not finish and rclone no longer trusts its record of the last sync. A resync merges both folders and deletes nothing.";
    case "force":
      return "Nothing was changed: the run would have deleted more files than the delete limit allows, or every file looked changed. If you moved or deleted those files on purpose, run it once past the check. If not, put the files back or fix the path; the next run checks again.";
    case "hint":
      return "Nothing was changed. Check that the drive is mounted and the path is right, and with Check access on, that both folders hold their RCLONE_TEST file. No override skips this check: the next run goes ahead once both folders are there.";
  }
}

/** BisyncRecoveryProps configures BisyncRecovery. */
export interface BisyncRecoveryProps {
  /** job is a failed two-way sync; for any other job nothing renders. */
  job: Job;
  /** canRun allows the action: only the job's owner may run it again. */
  canRun: boolean;
  /** workerName names the job's worker, where the new run goes. */
  workerName?: string;
}

/**
 * BisyncRecovery explains a two-way sync's failure and offers its way out:
 * Resync now after RESYNC_REQUIRED, Run once anyway after SAFETY_ABORT,
 * and only a hint after SIDE_UNAVAILABLE. Both actions ask first, then
 * run the job's definition again on the same worker with resync or force
 * set (RunJob) and open the new job. The job page and the schedule page
 * share it.
 */
export function BisyncRecovery({ job, canRun, workerName }: BisyncRecoveryProps) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const run = useMutation(JobService.method.runJob);
  const [asking, setAsking] = useState<OneShot | undefined>();
  const b = bisyncOf(job.definition);
  const recovery = b ? recoveryFor(job.error?.code) : undefined;
  if (!recovery) return null;
  const worker = workerName ?? "the same worker";

  const start = async (what: OneShot) => {
    const res = await run.mutateAsync({
      workerId: job.workerId,
      definition: withOneShot(job.definition, what),
    });
    await Promise.all([
      invalidateService(queryClient, JobService),
      invalidateService(queryClient, ScheduleService),
    ]);
    toast.success(what === "resync" ? "Resync queued" : "Run past the safety check queued");
    if (res.job) await navigate({ to: "/jobs/$jobId", params: { jobId: res.job.id } });
  };

  return (
    <div className="mt-2 flex flex-col items-start gap-3">
      <p className="text-[13px] leading-5 text-foreground">{recoveryHint(recovery)}</p>
      {canRun && recovery === "resync" && (
        <Button size="sm" onClick={() => setAsking("resync")}>
          <RefreshCw aria-hidden />
          Resync now
        </Button>
      )}
      {canRun && recovery === "force" && (
        <Button
          size="sm"
          variant="outline"
          className="bg-card text-danger hover:text-danger"
          onClick={() => setAsking("force")}
        >
          <ShieldAlert aria-hidden />
          Run once anyway
        </Button>
      )}

      <ConfirmDialog
        open={asking === "resync"}
        onOpenChange={(o) => !o && setAsking(undefined)}
        title="Resync this pair?"
        description={`It runs once on ${worker} and merges Folder A and Folder B as they are now: each gets the files it lacks, and nothing is deleted. Where a file differs on the two sides, ${resyncModeLabel(b?.options?.resyncMode)} is kept and the other is overwritten.`}
        confirmLabel="Resync now"
        onConfirm={() => start("resync")}
      >
        <p className="text-[13px] leading-5 text-muted-foreground">
          Files deleted on one side since the last sync come back from the other. The new filters,
          if they changed, apply from now on.
        </p>
      </ConfirmDialog>
      <ConfirmDialog
        open={asking === "force"}
        onOpenChange={(o) => !o && setAsking(undefined)}
        destructive
        title="Run once past the safety check?"
        description={`It runs once on ${worker} and applies the changes the check stopped, deletions included, on both folders.`}
        confirmLabel="Run once anyway"
        onConfirm={() => start("force")}
      >
        <div className="flex flex-col gap-1.5">
          <span className="text-xs text-muted-foreground">The worker reported</span>
          <p className="rounded-md bg-muted px-3 py-2 font-mono text-[12.5px] leading-5 break-words">
            {job.error?.message}
          </p>
          <p className="text-[13px] leading-5 text-muted-foreground">
            Only this run skips the check; later runs check again.
          </p>
        </div>
      </ConfirmDialog>
    </div>
  );
}
