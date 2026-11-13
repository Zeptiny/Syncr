import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { TriangleAlert, Undo2 } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { FieldShell } from "@/components/app/descriptor-form";
import { OverwriteChoice } from "@/components/app/overwrite-choice";
import { PathBrowserDialog } from "@/components/app/path-browser";
import { PathInput } from "@/components/app/path-input";
import { WorkerPicker } from "@/components/app/worker-picker";
import { Button } from "@/components/ui/button";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetFooter,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { JobService, type Worker } from "@/gen/syncr/v1/api_pb";
import type { ResticRestoreOverwrite, ResticSnapshot } from "@/gen/syncr/v1/link_pb";
import { ResticService, type Repository } from "@/gen/syncr/v1/restic_pb";
import { errorMessage, fieldViolations } from "@/lib/errors";
import { formatDateTime, tsDate } from "@/lib/format";
import { invalidateService } from "@/lib/invalidate";
import { isWithinRoots, treeToLocal } from "@/lib/restic-paths";
import { isOnline } from "@/lib/workers";

import { hasHooks, hooksError } from "../jobs/hooks";
import { HooksFields, hooksEmptyText } from "../jobs/hooks-editor";
import { PatternList } from "../jobs/pattern-list";
import { Toggle } from "../jobs/transfer-editor";
import {
  defaultRestoreTarget,
  emptyRestore,
  cannotRestoreInPlace,
  hostWorker,
  outdatedHost,
  loadRestoreTarget,
  originalParent,
  pickRestoreWorker,
  restoreSummary,
  shortId,
  toRestoreRequest,
  saveRestoreTarget,
  validateRestore,
  whatLabel,
  type RestoreErrors,
  type RestoreWhat,
} from "./model";

/** RestoreSheetProps configures RestoreSheet. */
export interface RestoreSheetProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  repository: Repository;
  /** snapshot is the one restored from; only its ID is required. */
  snapshot: Pick<ResticSnapshot, "id"> & Partial<Pick<ResticSnapshot, "host" | "time">>;
  /**
   * what is restored: chosen items, one folder's contents, everything, or
   * chosen items back to their original location.
   */
  what: RestoreWhat;
  workers: Worker[];
  /** now dates the proposed target folder (tests pin it). */
  now?: Date;
}

// TOO_OLD says a worker lacks restic.restore_subfolder_paths (before 2.1).
const TOO_OLD =
  "This worker is too old to restore items back in place: upgrade it, or choose another.";

/**
 * RestoreSheet starts a restore: what, onto which worker, into which
 * folder, and what happens to files already there. Existing files are kept
 * unless the user chooses otherwise. The worker and folder last used for
 * the repository are proposed first; a restore to the original location
 * proposes the snapshot's own machine and the items' original folder.
 */
export function RestoreSheet(props: RestoreSheetProps) {
  return (
    <Sheet open={props.open} onOpenChange={props.onOpenChange}>
      <SheetContent side="right" className="w-full gap-0 overflow-y-auto sm:max-w-[520px]">
        {props.open && <Body {...props} />}
      </SheetContent>
    </Sheet>
  );
}

function Body({
  onOpenChange,
  repository: repo,
  snapshot,
  what,
  workers,
  now = new Date(),
}: RestoreSheetProps) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const run = useMutation(JobService.method.runJob);
  // The worker and the target start as proposals; once the user chooses,
  // their choice stays. The proposals are made once (the target once per
  // worker), so a background refetch of the workers cannot move the
  // restore to another machine or folder while the sheet is open.
  const [chosenWorker, setChosenWorker] = useState<string | undefined>();
  const [typedTarget, setTypedTarget] = useState<string | undefined>();
  const [proposedWorker, setProposedWorker] = useState<string | undefined>();
  const [proposedTarget, setProposedTarget] = useState<{ workerId: string; target: string }>();
  const [date] = useState(now);
  const [rest, setRest] = useState(emptyRestore);
  const [errors, setErrors] = useState<RestoreErrors>({});
  const [browsing, setBrowsing] = useState(false);
  const [remembered] = useState(() =>
    what.mode === "original" ? undefined : loadRestoreTarget(repo.id),
  );
  const toOriginal = what.mode === "original";
  const parent = toOriginal ? originalParent(what.paths) : undefined;

  const usable = (id: string | undefined) => {
    const w = workers.find((x) => x.id === id);
    return w && pickRestoreWorker([w], "") ? w : undefined;
  };
  const proposal =
    proposedWorker ??
    (toOriginal
      ? hostWorker(workers, snapshot.host ?? "")?.id
      : (usable(remembered?.workerId)?.id ?? pickRestoreWorker(workers, snapshot.host ?? "")?.id));
  if (proposedWorker === undefined && proposal !== undefined) setProposedWorker(proposal);
  const workerId = chosenWorker ?? proposal ?? "";
  const worker = workers.find((w) => w.id === workerId);
  const os = worker?.info?.os;
  const roots = worker?.info?.localRoots ?? [];
  // Back in place needs a worker of 2.1 (restic.restore_subfolder_paths).
  const tooOld = toOriginal && !!worker && cannotRestoreInPlace(worker) !== undefined;
  const oldHost = toOriginal && !proposal ? outdatedHost(workers, snapshot.host ?? "") : undefined;
  const defaultTarget =
    proposedTarget?.workerId === workerId
      ? proposedTarget.target
      : parent !== undefined
        ? (treeToLocal(parent, os) ?? "")
        : remembered?.workerId === workerId
          ? remembered.target
          : defaultRestoreTarget(roots, snapshot.id, date, os);
  if (worker && proposedTarget?.workerId !== workerId) {
    setProposedTarget({ workerId, target: defaultTarget });
  }
  const target = typedTarget ?? defaultTarget;
  const state = { ...rest, workerId, target };
  // The hooks are the chosen worker's: another worker offers others.
  const hooksTarget = { workers: worker?.info ? [worker] : [] };
  const showHooks = hasHooks(rest.hooks) || hooksEmptyText(rest.hooks, hooksTarget) === undefined;
  const original = what.mode === "folder" ? treeToLocal(what.folder, os) : undefined;
  const outside = target.trim() !== "" && roots.length > 0 && !isWithinRoots(target, roots, os);
  // Back to the original location only where the worker may write, and
  // only by a worker that can.
  const blocked = toOriginal && (outside || tooOld);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    const local = validateRestore(what, state);
    if (tooOld && !local.worker) local.worker = TOO_OLD;
    else if (blocked && !local.target)
      local.target = "The original location is outside the worker's allowed folders.";
    setErrors(local);
    if (Object.keys(local).length) return;
    try {
      const res = await run.mutateAsync(toRestoreRequest(repo.id, snapshot, what, state));
      if (!toOriginal) saveRestoreTarget(repo.id, { workerId, target: target.trim() });
      await Promise.all([
        invalidateService(queryClient, JobService),
        invalidateService(queryClient, ResticService),
      ]);
      toast.success("Restore queued");
      onOpenChange(false);
      if (res.job) await navigate({ to: "/jobs/$jobId", params: { jobId: res.job.id } });
    } catch (err) {
      const server: RestoreErrors = {};
      const other: string[] = [];
      for (const v of fieldViolations(err)) {
        const hooks = hooksError(v);
        if (hooks !== undefined) server.hooks = server.hooks ? `${server.hooks} ${hooks}` : hooks;
        else if (v.field.includes("worker")) server.worker = v.message;
        else if (v.field.endsWith(".target")) server.target = v.message;
        else other.push(`${v.field}: ${v.message}`);
      }
      if (other.length) server.form = other.join(" ");
      else if (!Object.keys(server).length) server.form = errorMessage(err);
      setErrors(server);
    }
  };

  return (
    <form onSubmit={(e) => void submit(e)} noValidate className="flex min-h-full flex-col">
      <SheetHeader className="border-b">
        <SheetTitle>Restore from snapshot {shortId(snapshot.id)}</SheetTitle>
        <SheetDescription>
          {repo.name}
          {snapshot.time ? `, taken ${formatDateTime(tsDate(snapshot.time))}` : ""}
          {snapshot.host ? ` on ${snapshot.host}` : ""}
        </SheetDescription>
      </SheetHeader>

      <div className="flex flex-1 flex-col gap-6 px-4 py-5">
        <section className="flex flex-col gap-1.5" aria-labelledby="restore-what">
          <h3 id="restore-what" className="text-[13px] font-medium">
            What
          </h3>
          <div className="rounded-md border bg-muted/40 px-3 py-2.5 text-[13px]">
            <div className="font-medium first-letter:uppercase">{whatLabel(what)}</div>
            {(what.mode === "items" || what.mode === "original") && (
              <ul className="mt-1.5 flex max-h-28 flex-col gap-0.5 overflow-y-auto font-mono text-[12px] text-muted-foreground">
                {what.paths.map((p) => (
                  <li key={p} className="truncate" title={p}>
                    {p}
                  </li>
                ))}
              </ul>
            )}
            <p className="mt-1.5 text-xs leading-5 text-muted-foreground">
              {what.mode === "folder"
                ? "The folder's contents go directly into the target."
                : what.mode === "original"
                  ? `Back where they were: the items go into the target as they were in ${parent ?? "/"}.`
                  : "Files keep their original path under the target, such as target/srv/photos/…"}
            </p>
          </div>
          {errors.what && <p className="text-xs text-danger">{errors.what}</p>}
        </section>

        <FieldShell
          id="restore-worker"
          label="Worker"
          error={errors.worker}
          help={
            tooOld
              ? TOO_OLD
              : worker && !isOnline(worker)
                ? `${worker.name} is offline: the restore waits until it is back.`
                : oldHost && !worker
                  ? `${oldHost.name} took this snapshot, but it is too old to restore items back in place: upgrade it, or choose where the files go back.`
                  : toOriginal && !proposal
                    ? `No online worker is ${snapshot.host || "the machine this snapshot was taken on"}: choose where the files go back.`
                    : "The machine that receives the files."
          }
        >
          <WorkerPicker
            id="restore-worker"
            workers={workers}
            value={workerId}
            onChange={setChosenWorker}
            invalid={!!errors.worker || tooOld}
            unavailable={toOriginal ? cannotRestoreInPlace : undefined}
            className="bg-card"
          />
        </FieldShell>

        <FieldShell
          id="restore-target"
          label="Target folder"
          error={errors.target}
          help="A folder on the worker, inside one of its allowed folders. It is created if missing."
        >
          <PathInput
            id="restore-target"
            value={target}
            onChange={setTypedTarget}
            placeholder={roots[0] ? `${roots[0]}/restore` : "/srv/restore"}
            invalid={!!errors.target}
            onBrowse={worker && isOnline(worker) ? () => setBrowsing(true) : undefined}
          />
          {outside && (
            <p className="inline-flex w-fit items-center gap-1.5 rounded-md bg-warning-soft px-2 py-1 text-xs text-warning">
              <TriangleAlert className="size-3.5" aria-hidden />
              Not inside {worker?.name ?? "the worker"}'s allowed folders ({roots.join(", ")}). The
              worker will refuse it.
            </p>
          )}
          {original !== undefined && (
            <div>
              <Button
                type="button"
                variant="outline"
                size="sm"
                onClick={() => setTypedTarget(original)}
              >
                <Undo2 aria-hidden />
                Restore to the original location
              </Button>
              <span className="ml-2 font-mono text-xs text-muted-foreground">{original}</span>
            </div>
          )}
        </FieldShell>

        <OverwriteChoice
          value={rest.overwrite}
          onChange={(overwrite: ResticRestoreOverwrite) => setRest((p) => ({ ...p, overwrite }))}
          className="grid-cols-1"
        />

        <Toggle
          id="restore-verify"
          label="Verify after restoring"
          help="Reads the restored files back and compares them with the snapshot."
          checked={rest.verify}
          onChange={(verify) => setRest((p) => ({ ...p, verify }))}
        />

        <FieldShell
          id="restore-excludes"
          label="Exclude"
          optional
          help="Patterns of files to leave out, such as *.tmp."
        >
          <PatternList
            id="restore-excludes"
            value={rest.excludes}
            onChange={(excludes) => setRest((p) => ({ ...p, excludes }))}
          />
        </FieldShell>

        {showHooks && (
          <section className="flex flex-col gap-3" aria-labelledby="restore-hooks">
            <div>
              <h3 id="restore-hooks" className="text-[13px] font-medium">
                Hooks <span className="font-normal text-muted-foreground">(optional)</span>
              </h3>
              <p className="mt-0.5 text-xs leading-5 text-muted-foreground">
                Commands {worker?.name ?? "the worker"}'s operator prepared, run around the restore:
                stop what uses the files first, load a restored dump afterwards.
              </p>
            </div>
            <HooksFields
              idPrefix="restore"
              value={rest.hooks}
              onChange={(hooks) => setRest((p) => ({ ...p, hooks }))}
              target={hooksTarget}
              error={errors.hooks}
              className="lg:grid-cols-1"
            />
          </section>
        )}

        {errors.form && (
          <p role="alert" className="rounded-md bg-danger-soft px-3 py-2 text-[13px] text-danger">
            {errors.form}
          </p>
        )}
      </div>

      <SheetFooter className="sticky bottom-0 border-t bg-background">
        <p className="text-[13px] text-muted-foreground" data-testid="restore-summary">
          {restoreSummary(what, state, worker?.name)}
        </p>
        <div className="flex justify-end gap-2">
          <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
            Cancel
          </Button>
          <Button type="submit" disabled={run.isPending || repo.deleting || blocked}>
            {run.isPending ? "Queuing…" : "Start restore"}
          </Button>
        </div>
      </SheetFooter>

      {worker && (
        <PathBrowserDialog
          open={browsing}
          onOpenChange={setBrowsing}
          source={{ kind: "worker", workerId: worker.id }}
          title={`Folders on ${worker.name}`}
          mode="dir"
          onSelect={([path]) => path !== undefined && setTypedTarget(path)}
        />
      )}
    </form>
  );
}
