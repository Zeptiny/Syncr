import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { TriangleAlert } from "lucide-react";
import { useState } from "react";

import { TypedConfirmDialog } from "@/components/app/typed-confirm-dialog";
import { WorkerPicker } from "@/components/app/worker-picker";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { Checkbox } from "@/components/ui/checkbox";
import { Label } from "@/components/ui/label";
import { JobService, type Worker } from "@/gen/syncr/v1/api_pb";
import {
  ResticService,
  type DeleteRepositoryResponse,
  type Repository,
} from "@/gen/syncr/v1/restic_pb";
import { ScheduleService, type Schedule } from "@/gen/syncr/v1/schedules_pb";
import { fieldViolations } from "@/lib/errors";
import { invalidateService } from "@/lib/invalidate";

import { canDelete, isLocalRepository, locationText, toDeleteRequest } from "./model";

/** DeleteRepositoryDialogProps configures DeleteRepositoryDialog. */
export interface DeleteRepositoryDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  repository: Repository;
  /** schedules are the ones using the repository. */
  schedules: Schedule[];
  workers: Worker[];
  /** activeJobs is how many jobs on the repository are queued or running. */
  activeJobs?: number;
  /** initialDeleteData opens with "also delete the data" ticked ("Try again"). */
  initialDeleteData?: boolean;
  /** onDone is told what happened: forgotten, or a deletion job was started. */
  onDone?: (res: DeleteRepositoryResponse, deleteData: boolean) => void;
}

function ScheduleList({ schedules }: { schedules: Schedule[] }) {
  return (
    <ul className="flex list-disc flex-col gap-0.5 pl-5 text-[13px]">
      {schedules.map((s) => (
        <li key={s.id}>{s.name}</li>
      ))}
    </ul>
  );
}

/**
 * DeleteRepositoryDialog removes a repository from Syncr. By default it
 * only forgets it and the backups stay where they are. Ticking "Also
 * delete the backup data" is the destructive path: it warns, lists what
 * pauses, asks which worker deletes, and unlocks its button only once the
 * repository's name is typed exactly.
 */
export function DeleteRepositoryDialog(props: DeleteRepositoryDialogProps) {
  // Mounted with each opening, so the choice never carries over.
  return props.open ? <Body {...props} /> : null;
}

function Body({
  open,
  onOpenChange,
  repository: repo,
  schedules,
  workers,
  activeJobs = 0,
  initialDeleteData = false,
  onDone,
}: DeleteRepositoryDialogProps) {
  const queryClient = useQueryClient();
  const del = useMutation(ResticService.method.deleteRepository);
  const [deleteData, setDeleteData] = useState(initialDeleteData);
  const [workerId, setWorkerId] = useState("");
  const [nameError, setNameError] = useState<string | undefined>();
  const [workerError, setWorkerError] = useState<string | undefined>();
  const local = isLocalRepository(repo);
  const where = locationText(repo);
  // The typed name is checked by TypedConfirmDialog; here only the worker.
  const ready = canDelete(repo, { deleteData, typedName: repo.name, workerId });

  const confirm = async (typed: string) => {
    setNameError(undefined);
    setWorkerError(undefined);
    try {
      const res = await del.mutateAsync(
        toDeleteRequest(repo, { deleteData, typedName: typed, workerId }),
      );
      await Promise.all([
        invalidateService(queryClient, ResticService),
        invalidateService(queryClient, ScheduleService),
        invalidateService(queryClient, JobService),
      ]);
      onDone?.(res, deleteData);
    } catch (err) {
      const violations = fieldViolations(err);
      const name = violations.find((v) => v.field === "confirm_name");
      const worker = violations.find((v) => v.field === "worker_id");
      if (name) setNameError(sentence(name.message));
      if (worker) setWorkerError(sentence(worker.message));
      if (name || worker) throw new Error("Check the highlighted fields.", { cause: err });
      throw err;
    }
  };

  return (
    <TypedConfirmDialog
      open={open}
      onOpenChange={onOpenChange}
      title={deleteData ? `Delete ${repo.name} and its data?` : `Forget ${repo.name}?`}
      description={
        deleteData
          ? `Syncr deletes the repository's files in ${where}, then forgets the repository.`
          : `Syncr forgets this repository. The backups stay in ${where}.`
      }
      name={repo.name}
      requireName={deleteData}
      nameError={nameError}
      disabled={!ready}
      confirmLabel={deleteData ? "Delete repository and data" : "Forget repository"}
      onConfirm={confirm}
    >
      <div className="flex flex-col gap-4">
        {!deleteData && schedules.length > 0 && (
          <div className="flex flex-col gap-1.5 text-[13px]">
            <span>
              {schedules.length === 1 ? "This schedule uses" : "These schedules use"} it and will be
              paused:
            </span>
            <ScheduleList schedules={schedules} />
          </div>
        )}
        <Label
          htmlFor="delete-repo-data"
          className="flex cursor-pointer items-start gap-2.5 rounded-md border px-3 py-2.5 text-[13px] leading-5 font-normal"
        >
          <Checkbox
            id="delete-repo-data"
            className="mt-0.5"
            checked={deleteData}
            onCheckedChange={(c) => setDeleteData(c === true)}
          />
          <span>
            Also delete the backup data in <span className="font-mono text-[12.5px]">{where}</span>
          </span>
        </Label>
        {deleteData && (
          <>
            <Alert variant="destructive">
              <TriangleAlert aria-hidden />
              <AlertTitle>The backups will be gone</AlertTitle>
              <AlertDescription>
                <p>
                  Deletes restic's config, keys, data, index, snapshots and locks there. Other files
                  are kept. This cannot be undone.
                </p>
                <p>
                  A versioned bucket keeps older versions of the deleted files until its own rules
                  remove them.
                </p>
              </AlertDescription>
            </Alert>
            {schedules.length > 0 && (
              <div className="flex flex-col gap-1.5 text-[13px]">
                <span>
                  {schedules.length === 1 ? "This schedule is" : "These schedules are"} paused at
                  once:
                </span>
                <ScheduleList schedules={schedules} />
              </div>
            )}
            {activeJobs > 0 && (
              <p className="text-[13px]">
                {activeJobs} queued or running {activeJobs === 1 ? "job" : "jobs"} on this
                repository {activeJobs === 1 ? "finishes" : "finish"} first.
              </p>
            )}
            <div className="flex flex-col gap-1.5">
              <Label htmlFor="delete-repo-worker" className="text-[13px]">
                Run on
              </Label>
              <WorkerPicker
                id="delete-repo-worker"
                workers={workers}
                value={workerId}
                onChange={(v) => {
                  setWorkerId(v);
                  setWorkerError(undefined);
                }}
                autoLabel={local ? undefined : "Automatic"}
                invalid={!!workerError}
              />
              <p className={workerError ? "text-xs text-danger" : "text-xs text-muted-foreground"}>
                {workerError ??
                  (local
                    ? "This repository is on a worker's disk: choose that worker."
                    : "The worker that deletes the files. Automatic picks an online one.")}
              </p>
            </div>
          </>
        )}
      </div>
    </TypedConfirmDialog>
  );
}

function sentence(s: string): string {
  return s ? s.charAt(0).toUpperCase() + s.slice(1) + (s.endsWith(".") ? "" : ".") : s;
}
