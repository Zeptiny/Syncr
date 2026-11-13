import { StartedJobs } from "./started-jobs";
import { TransferSheet } from "./transfer-sheet";
import type { TransferActions } from "./use-transfer-actions";

/**
 * TransferActionsHost renders a page's transfer sheet and follows the jobs
 * it started (see useTransferActions).
 */
export function TransferActionsHost({
  actions,
  listedBy,
}: {
  actions: TransferActions;
  /** listedBy is the worker that listed the source, proposed for copies between remotes. */
  listedBy?: string;
}) {
  const { request, close, started } = actions;
  return (
    <>
      {request && (
        <TransferSheet
          open
          onOpenChange={(o) => !o && close()}
          source={request.source}
          size={request.size}
          listedBy={listedBy}
          onStarted={(job) => {
            started.add(job);
            if (job.op === "move" && !job.dryRun) request.clear?.();
          }}
        />
      )}
      <StartedJobs jobs={started.jobs} onDone={started.done} />
    </>
  );
}
