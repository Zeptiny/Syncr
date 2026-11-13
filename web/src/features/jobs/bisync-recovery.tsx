import { Code, ConnectError } from "@connectrpc/connect";
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { RefreshCw, ShieldAlert } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { ConfirmDialog } from "@/components/app/confirm-dialog";
import { Button } from "@/components/ui/button";
import { JobService, type Job, type Worker } from "@/gen/syncr/v1/api_pb";
import { ScheduleService, type Schedule } from "@/gen/syncr/v1/schedules_pb";
import { invalidateService } from "@/lib/invalidate";

import {
  bisyncOf,
  planRecovery,
  recoveryFor,
  resyncModeLabel,
  scheduleWorker,
  withOneShot,
  type OneShot,
  type Recovery,
  type RecoveryBlock,
  type RecoveryPlan,
} from "./bisync-definition";

/** recoveryHint says what a failed two-way sync means and what to do about it. */
export function recoveryHint(r: Recovery): string {
  switch (r) {
    case "resync":
      return "rclone needs a resync before this pair can sync again: its filters changed, a folder was empty on the pair's first run on this worker, or a run failed before the pair had a state to go back to. Fix what the message names first, if anything: a resync stops at the same error. A resync merges both folders and deletes nothing.";
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
  /**
   * schedule is the page's schedule on the schedule page, where the action
   * runs its current settings on the worker it targets. Without it (the
   * job page) the action runs the job's own settings on its worker, and the
   * job's schedule, if any, is looked up.
   */
  schedule?: Schedule;
  /** workers name the worker the action runs on. */
  workers?: Worker[];
}

/**
 * BisyncRecovery explains a two-way sync's failure and offers its way out:
 * Resync now after RESYNC_REQUIRED, Run once anyway after SAFETY_ABORT,
 * and only a hint after SIDE_UNAVAILABLE. The job page and the schedule
 * page share it. planRecovery decides what an action runs, and whether
 * it is offered at all: never on a run that a newer run of the pair, or a
 * change of its schedule, superseded. Both actions ask first, then run
 * that definition once with resync or force set (RunJob) and open the new
 * job.
 */
export function BisyncRecovery({ job, canRun, schedule, workers }: BisyncRecoveryProps) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const run = useMutation(JobService.method.runJob);
  const [asking, setAsking] = useState<OneShot | undefined>();
  const from = schedule ? "schedule" : "job";
  const recovery = bisyncOf(job.definition) ? recoveryFor(job.error?.code) : undefined;
  const acts = canRun && (recovery === "resync" || recovery === "force");
  // The job's own schedule, on the job page: its settings decide whether
  // the job is still current.
  const own = useQuery(
    ScheduleService.method.getSchedule,
    { scheduleId: job.scheduleId },
    { enabled: acts && !schedule && !!job.scheduleId, retry: false },
  );
  const ownSchedule = schedule ? undefined : own.data?.schedule;
  // A deleted schedule leaves the job on its own; any other error, or none
  // yet, leaves it undecided.
  const ownGone = !!own.error && ConnectError.from(own.error).code === Code.NotFound;
  const ownPending = !schedule && !!job.scheduleId && !ownSchedule && !ownGone;
  const workerId = schedule ? scheduleWorker(schedule) : job.workerId;
  const newer = useQuery(
    JobService.method.listJobs,
    { workerId, kinds: ["rclone_bisync"], createdAfter: job.createdAt, pageSize: 100 },
    { enabled: acts && !!workerId },
  );
  if (!recovery) return null;
  const plan: RecoveryPlan = (acts &&
    planRecovery({
      job,
      from,
      schedule: schedule ?? ownSchedule,
      newer:
        newer.data && !ownPending
          ? { jobs: newer.data.jobs, complete: !newer.data.nextPageToken }
          : undefined,
    })) || { recovery };
  const target = plan.run;
  const def = target?.definition;
  const b = bisyncOf(def);
  const dryRun = !!b?.options?.dryRun;
  const workerName = workers?.find((w) => w.id === target?.workerId)?.name ?? "the same worker";
  const settings =
    target?.from === "schedule" ? "the schedule's current settings" : "this job's settings";

  const start = async (what: OneShot) => {
    if (!target) return;
    const res = await run.mutateAsync({
      workerId: target.workerId,
      definition: withOneShot(target.definition, what),
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
      {plan.block && <Blocked block={plan.block} />}
      {target && recovery === "resync" && (
        <Button size="sm" onClick={() => setAsking("resync")}>
          <RefreshCw aria-hidden />
          Resync now
        </Button>
      )}
      {target && recovery === "force" && (
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
        description={`It runs once on ${workerName} with ${settings} and merges Folder A and Folder B as they are now: each gets the files it lacks, and nothing is deleted. Where a file differs on the two sides, ${resyncModeLabel(b?.options?.resyncMode)} is kept and the other is overwritten.`}
        confirmLabel="Resync now"
        onConfirm={() => start("resync")}
      >
        <div className="flex flex-col gap-2 text-[13px] leading-5">
          <p className="font-medium text-foreground">
            If a folder is on a drive or a mount point, make sure it is mounted first. A resync
            fills an empty folder from the other side; once the drive is back, its files would look
            deleted, and the next run would delete them on the other side.
          </p>
          <p className="text-muted-foreground">
            Files deleted on one side since the last sync come back from the other. The new filters,
            if they changed, apply from now on.
            {dryRun &&
              " The settings say dry run, but this resync is a real one: a dry run cannot rebuild the pair's state."}
          </p>
        </div>
      </ConfirmDialog>
      <ConfirmDialog
        open={asking === "force"}
        onOpenChange={(o) => !o && setAsking(undefined)}
        destructive
        title="Run once past the safety check?"
        description={
          dryRun
            ? `It runs once on ${workerName} with ${settings}, a dry run: it reports what it would change past the delete limit and the “every file changed” check, and changes nothing.`
            : `It runs once on ${workerName} with ${settings}, without the delete limit and the “every file changed” check, and applies the changes it finds, deletions included, on both folders.`
        }
        confirmLabel="Run once anyway"
        onConfirm={() => start("force")}
      >
        <div className="flex flex-col gap-1.5">
          <span className="text-xs text-muted-foreground">The stopped run reported</span>
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

/** Blocked says why a failed run offers no action, and where to act instead. */
function Blocked({ block }: { block: RecoveryBlock }) {
  const linkClass = "font-medium text-primary hover:underline";
  switch (block.why) {
    case "pending":
      return null;
    case "newer":
      return (
        <p className="text-[13px] leading-5 text-muted-foreground">
          A newer run of this pair exists on this worker, so its result is the one to act on
          {block.jobId ? (
            <>
              {": "}
              <Link to="/jobs/$jobId" params={{ jobId: block.jobId }} className={linkClass}>
                open it
              </Link>
              .
            </>
          ) : (
            "."
          )}
        </p>
      );
    case "scheduleChanged":
      return (
        <p className="text-[13px] leading-5 text-muted-foreground">
          Its schedule has other settings or another worker since this run. Its next run uses them;{" "}
          <Link
            to="/schedules/$scheduleId"
            params={{ scheduleId: block.scheduleId }}
            className={linkClass}
          >
            the schedule's page
          </Link>{" "}
          offers what that run needs.
        </p>
      );
    case "settingsChanged":
      return (
        <p className="text-[13px] leading-5 text-muted-foreground">
          The schedule's settings changed after this run, so these counts are not those of the
          current settings. The next run checks again with them; Run now starts it at once.
        </p>
      );
  }
}
