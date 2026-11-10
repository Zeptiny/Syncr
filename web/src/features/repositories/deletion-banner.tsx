import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { LoaderCircle, TriangleAlert } from "lucide-react";
import { useState } from "react";

import { ConfirmDialog } from "@/components/app/confirm-dialog";
import { StatusBadge } from "@/components/app/status-badge";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Button } from "@/components/ui/button";
import { JobService, JobStatus, type Worker } from "@/gen/syncr/v1/api_pb";
import { ResticService, type Repository } from "@/gen/syncr/v1/restic_pb";
import type { Schedule } from "@/gen/syncr/v1/schedules_pb";
import { useWatchJob } from "@/hooks/use-watch-job";
import { invalidateService } from "@/lib/invalidate";
import { isTerminal } from "@/lib/status";

import { deletionFailed } from "./model";

/** DeletionBannerProps configures DeletionBanner. */
export interface DeletionBannerProps {
  repository: Repository;
  workers: Worker[];
  /** waitingFor is how many other jobs on the repository run before the deletion. */
  waitingFor?: number;
  /** pausedSchedules are the repository's schedules the deletion paused. */
  pausedSchedules?: Schedule[];
  /** onTryAgain opens the delete dialog with the data option ticked. */
  onTryAgain: () => void;
  /** onForget opens the delete dialog for a plain forget. */
  onForget: () => void;
}

/**
 * DeletionBanner shows on a repository whose data is being deleted, or
 * whose last deletion failed. While deleting it follows the deletion job
 * live and offers to cancel it; after a failure it offers to try again or
 * to forget the repository only.
 */
export function DeletionBanner(props: DeletionBannerProps) {
  const repo = props.repository;
  if (repo.deleting) return <Deleting {...props} />;
  if (deletionFailed(repo)) return <Failed {...props} />;
  return null;
}

function Deleting({ repository: repo, workers, waitingFor = 0 }: DeletionBannerProps) {
  const queryClient = useQueryClient();
  const watch = useWatchJob(repo.deletionJobId);
  const cancel = useMutation(JobService.method.cancelJob);
  const [cancelling, setCancelling] = useState(false);
  const job = watch.job;
  const worker = workers.find((w) => w.id === job?.workerId)?.name;
  const waiting = job?.status === JobStatus.QUEUED && (!!job.waitingForJobId || waitingFor > 0);
  const ended = !!job && isTerminal(job.status);
  return (
    <>
      <Alert variant="destructive" data-testid="deletion-banner">
        <LoaderCircle className="animate-spin motion-reduce:animate-none" aria-hidden />
        <AlertTitle>
          Deleting the backup data{worker ? ` on ${worker}` : ""}
          {waiting &&
            (waitingFor > 0
              ? ` — waiting for ${waitingFor} ${waitingFor === 1 ? "job" : "jobs"} on this repository first`
              : " — waiting for another job on this repository first")}
        </AlertTitle>
        <AlertDescription>
          <p className="text-foreground/80">
            New jobs and schedule changes are refused until the deletion ends. The repository is
            forgotten when it succeeds.
          </p>
          <div className="mt-2 flex flex-wrap items-center gap-2">
            {job && <StatusBadge job={job.status} cancelRequested={job.cancelRequested} />}
            {repo.deletionJobId && (
              <Button variant="outline" size="sm" asChild>
                <Link to="/jobs/$jobId" params={{ jobId: repo.deletionJobId }}>
                  View job
                </Link>
              </Button>
            )}
            <Button
              variant="outline"
              size="sm"
              className="text-danger hover:text-danger"
              disabled={!repo.deletionJobId || ended || job?.cancelRequested}
              onClick={() => setCancelling(true)}
            >
              {job?.cancelRequested ? "Cancelling…" : "Cancel deletion"}
            </Button>
          </div>
        </AlertDescription>
      </Alert>
      <ConfirmDialog
        open={cancelling}
        onOpenChange={setCancelling}
        destructive
        title="Cancel the deletion?"
        description={
          job?.status === JobStatus.QUEUED
            ? "The deletion has not started, so nothing was removed. The repository stays, and its schedules stay paused until you resume them."
            : "The worker is asked to stop. Some of the repository's files may already be gone; you can try again or forget the repository afterwards."
        }
        confirmLabel="Cancel deletion"
        onConfirm={async () => {
          await cancel.mutateAsync({ jobId: repo.deletionJobId });
          await Promise.all([
            invalidateService(queryClient, JobService),
            invalidateService(queryClient, ResticService),
          ]);
        }}
      />
    </>
  );
}

function Failed({
  repository: repo,
  pausedSchedules = [],
  onTryAgain,
  onForget,
}: DeletionBannerProps) {
  const cancelled = /cancelled/i.test(repo.deletionError);
  return (
    <Alert variant="destructive" data-testid="deletion-failed">
      <TriangleAlert aria-hidden />
      <AlertTitle>
        {cancelled ? "The deletion was cancelled" : "Deleting the data failed"}
      </AlertTitle>
      <AlertDescription>
        <p>
          {cancelled
            ? "Some repository files may already be gone."
            : `Deleting the data failed: ${repo.deletionError.replace(/^deleting the data failed:\s*/i, "")}`}
        </p>
        {pausedSchedules.length > 0 && (
          <p className="text-foreground/80">
            {pausedSchedules.length === 1 ? "Its schedule remains" : "Its schedules remain"} paused:{" "}
            {pausedSchedules.map((s, i) => (
              <span key={s.id}>
                {i > 0 && ", "}
                <Link
                  to="/schedules/$scheduleId"
                  params={{ scheduleId: s.id }}
                  className="font-medium underline underline-offset-2"
                >
                  {s.name}
                </Link>
              </span>
            ))}
            . Resume {pausedSchedules.length === 1 ? "it" : "them"} if you keep the repository.
          </p>
        )}
        <div className="mt-2 flex flex-wrap items-center gap-2">
          <Button size="sm" variant="destructive" onClick={onTryAgain}>
            Try again
          </Button>
          <Button size="sm" variant="outline" onClick={onForget}>
            Forget only
          </Button>
          {repo.deletionJobId && (
            <Button size="sm" variant="ghost" asChild>
              <Link to="/jobs/$jobId" params={{ jobId: repo.deletionJobId }}>
                View the failed job
              </Link>
            </Button>
          )}
        </div>
      </AlertDescription>
    </Alert>
  );
}
