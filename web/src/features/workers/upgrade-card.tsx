import { useQuery } from "@connectrpc/connect-query";
import { TriangleAlert } from "lucide-react";

import { CopyCommand } from "@/components/app/copy-command";
import { Panel } from "@/components/app/panel";
import { Skeleton } from "@/components/ui/skeleton";
import { WorkerAdminService, type Worker } from "@/gen/syncr/v1/api_pb";
import { errorMessage } from "@/lib/errors";

import { platformLabel } from "./workers-page";

/**
 * upgradeLimits are what the upgrade command cannot do (design §7.8,
 * "Limits"), shown next to the command.
 */
export const upgradeLimits = [
  "After the restart, the command checks that the service stays up, not that the worker reaches the controller: check that it shows as online here.",
  "A worker in a container is upgraded by pulling the new image. The command refuses when it cannot replace its own program file, and says why.",
  "The download is checked against the release's checksums. That catches a damaged download, not a tampered release: the trust is the same as for the install command.",
];

/**
 * UpgradeWorkerCard shows an administrator the commands that upgrade an
 * outdated worker in place (`syncr-worker upgrade`), for its operating
 * system, system service and per-user, with the server's notes and the
 * command's limits. Running jobs are interrupted by the restart.
 */
export function UpgradeWorkerCard({ worker }: { worker: Worker }) {
  const q = useQuery(WorkerAdminService.method.getWorkerUpgradeCommands, { workerId: worker.id });
  const commands = q.data?.commands ?? [];
  return (
    <Panel
      title="Upgrade this worker"
      description={`Run one of these commands on ${worker.info?.hostname || worker.name}. The worker downloads the controller's release, checks it, swaps its program and restarts; if the new version does not stay up, the old one is put back.`}
      aria-label="Upgrade this worker"
    >
      <div className="flex flex-col gap-4">
        <div
          role="note"
          className="flex gap-2.5 rounded-lg bg-warning-soft px-3.5 py-3 text-[13px] leading-5"
        >
          <TriangleAlert className="mt-0.5 size-4 shrink-0 text-warning" aria-hidden />
          <div>
            <strong className="font-semibold">Running jobs on this worker are interrupted</strong>{" "}
            by the restart, as with any worker restart. Upgrade it when it is idle.
          </div>
        </div>
        {q.isPending ? (
          <Skeleton className="h-32 w-full" />
        ) : q.error ? (
          <p role="alert" className="text-[13px] text-danger">
            {errorMessage(q.error)}
          </p>
        ) : commands.length ? (
          <CopyCommand commands={commands} scopeLabel="Upgrade for" />
        ) : (
          <p className="text-[13px] text-muted-foreground">
            {worker.info?.os
              ? `There is no upgrade command for ${platformLabel(worker)}: no release is built for it. Install the new version by hand.`
              : "The worker has not reported its operating system yet, so there is no command to show."}
          </p>
        )}
        <ul className="flex list-disc flex-col gap-1 pl-5 text-xs leading-5 text-muted-foreground">
          {upgradeLimits.map((l) => (
            <li key={l}>{l}</li>
          ))}
        </ul>
      </div>
    </Panel>
  );
}
