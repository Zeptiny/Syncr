import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { Eye, FilePen, TriangleAlert } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { ChoiceCards } from "@/components/app/choice-cards";
import { ConfirmDialog } from "@/components/app/confirm-dialog";
import { FieldShell } from "@/components/app/descriptor-form";
import { EntryIcon } from "@/components/app/entry-icon";
import { sizeText } from "@/components/app/file-browser/helpers";
import type { KnownSize } from "@/components/app/file-browser/selection";
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
import { JobService, WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import { RemoteService } from "@/gen/syncr/v1/remotes_pb";
import { useBackendMap } from "@/hooks/use-backends";
import { errorMessage, fieldViolations } from "@/lib/errors";
import { invalidateService } from "@/lib/invalidate";
import { pathCrumbs } from "@/lib/paths";
import { isOnline } from "@/lib/workers";

import { DestinationPicker } from "./destination-picker";
import { makeJobDraft } from "./job-drafts";
import {
  conflictOptions,
  moveText,
  endpointText,
  itemsText,
  placeKind,
  planTransfer,
  transferDefinition,
  transferRequest,
  transferSummary,
  workerRule,
  type Conflict,
  type Endpoint,
  type TransferErrors,
  type TransferOp,
  type TransferPlan,
  type TransferSource,
} from "./transfer";
import type { StartedJob } from "./use-started-jobs";

/** TransferSheetProps configures TransferSheet. */
export interface TransferSheetProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** source is what is copied: the selection's uncovered items and their common parent. */
  source: TransferSource;
  /** size is what the items are known to weigh (SelectionContext.size). */
  size?: KnownSize;
  /** listedBy is the worker that listed the source; it runs a copy between remotes first. */
  listedBy?: string;
  /** op is the operation the sheet opens with. */
  op?: TransferOp;
  /** onStarted hands a started job to the page, which follows it (StartedJobs). */
  onStarted: (job: StartedJob) => void;
}

/**
 * TransferSheet copies or moves a selection into another folder, a
 * worker's or a remote's, as a transfer job with a file manager's
 * semantics: selected folders arrive as folders. Existing files are kept
 * unless the user chooses otherwise; Preview runs it as a dry run; a move
 * asks first.
 */
export function TransferSheet(props: TransferSheetProps) {
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
  source,
  size,
  listedBy,
  op: initialOp,
  onStarted,
}: TransferSheetProps) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const backends = useBackendMap();
  const workersQ = useQuery(WorkerAdminService.method.listWorkers, {});
  const remotesQ = useQuery(RemoteService.method.listRemotes, {});
  const run = useMutation(JobService.method.runJob);
  const workers = workersQ.data?.workers ?? [];
  const remotes = remotesQ.data?.remotes ?? [];

  const [op, setOp] = useState<TransferOp>(initialOp ?? "copy");
  const [destination, setDestination] = useState<Endpoint>();
  const [chosenWorker, setChosenWorker] = useState("");
  const [conflict, setConflict] = useState<Conflict>("keep");
  const [errors, setErrors] = useState<TransferErrors>({});
  const [confirming, setConfirming] = useState<TransferPlan>();

  const n = source.items.length;
  const kind = placeKind(source.place);
  const from: Endpoint | undefined =
    source.parent !== undefined ? { place: source.place, path: source.parent } : undefined;
  const fromText = from ? endpointText(from, workers, remotes) : "the source";
  const rule = workerRule(source.place, destination?.place, workers, listedBy);
  const workerId =
    rule.kind === "pinned"
      ? rule.workerId
      : rule.kind === "choose"
        ? chosenWorker || rule.proposed || ""
        : "";
  const jobWorker = workers.find((w) => w.id === workerId);
  const form = { destination, workerId, conflict };

  const plan = (): TransferPlan | undefined => {
    const r = planTransfer(source, form, workers, listedBy, remotes);
    setErrors(r.errors);
    return r.plan;
  };

  const start = async (p: TransferPlan, dryRun: boolean) => {
    setErrors({});
    try {
      const res = await run.mutateAsync(transferRequest(p, op, conflict, dryRun));
      await invalidateService(queryClient, JobService);
      const job = res.job;
      const what = `${itemsText(n)} into ${endpointText(p.destination, workers, remotes)}`;
      const verb = dryRun ? "Preview" : op === "move" ? "Move" : "Copy";
      if (job) {
        toast.success(`${verb} queued`, {
          description: dryRun
            ? "Nothing is changed: the job's page lists what would be copied."
            : `${itemsText(n)} into ${endpointText(p.destination, workers, remotes)}.`,
          action: {
            label: "View job",
            onClick: () => void navigate({ to: "/jobs/$jobId", params: { jobId: job.id } }),
          },
        });
        onStarted({
          jobId: job.id,
          op,
          conflict,
          dryRun,
          what,
          refresh: op === "move" ? [p.destination.place, p.source.place] : [p.destination.place],
        });
      }
      onOpenChange(false);
    } catch (err) {
      const server: TransferErrors = {};
      const other: string[] = [];
      for (const v of fieldViolations(err)) {
        if (v.field.includes("worker")) server.worker = v.message;
        else if (v.field.includes("destination")) server.destination = v.message;
        else if (v.field.includes("source") || v.field.includes("filters")) server.what = v.message;
        else other.push(`${v.field}: ${v.message}`);
      }
      if (other.length) server.form = other.join(" ");
      else if (!Object.keys(server).length) server.form = errorMessage(err);
      setErrors(server);
      throw err;
    }
  };

  const submit = (e: React.FormEvent) => {
    e.preventDefault();
    const p = plan();
    if (!p) return;
    if (op === "move") setConfirming(p);
    else void start(p, false).catch(() => {});
  };
  const preview = () => {
    const p = plan();
    if (p) void start(p, true).catch(() => {});
  };
  const openEditor = () => {
    const p = plan();
    if (!p) return;
    const jobDraft = makeJobDraft(transferDefinition(p, op, conflict, false), p.workerId);
    onOpenChange(false);
    void navigate({ to: "/jobs/new", state: (prev) => ({ ...prev, jobDraft }) });
  };

  return (
    <form onSubmit={submit} noValidate className="flex min-h-full flex-col">
      <SheetHeader className="border-b">
        <SheetTitle>
          {op === "move" ? "Move" : "Copy"} {itemsText(n)}
        </SheetTitle>
        <SheetDescription>From {fromText}</SheetDescription>
      </SheetHeader>

      <div className="flex flex-1 flex-col gap-6 px-4 py-5">
        <section className="flex flex-col gap-1.5" aria-labelledby="transfer-what">
          <h3 id="transfer-what" className="text-[13px] font-medium">
            What
          </h3>
          <div className="rounded-md border bg-muted/40 px-3 py-2.5 text-[13px]">
            <div className="flex flex-wrap items-baseline justify-between gap-x-3">
              <span className="font-medium">{itemsText(n)}</span>
              {size && <span className="text-xs text-muted-foreground">{sizeText(size)}</span>}
            </div>
            <ul
              className="mt-1.5 flex max-h-32 flex-col gap-0.5 overflow-y-auto text-[12px]"
              aria-label="Items"
            >
              {source.items.map((i) => (
                <li key={i.path} className="flex min-w-0 items-center gap-1.5" title={i.path}>
                  <EntryIcon type={i.entry.type} className="size-3.5" />
                  <span className="truncate font-mono">
                    {pathCrumbs(i.path, kind).at(-1)?.label ?? i.path}
                  </span>
                </li>
              ))}
            </ul>
            <p className="mt-1.5 text-xs leading-5 text-muted-foreground">
              Selected folders arrive as folders, with everything in them.
            </p>
          </div>
          {errors.what && <p className="text-xs text-danger">{errors.what}</p>}
        </section>

        <ChoiceCards
          label="Copy or move"
          labelId="transfer-op"
          value={op}
          onChange={setOp}
          className="grid-cols-2"
          options={[
            { value: "copy", label: "Copy", help: "The items stay where they are." },
            { value: "move", label: "Move", help: "The items are deleted here once copied." },
          ]}
        />

        <DestinationPicker
          id="transfer-dest"
          value={destination}
          onChange={setDestination}
          workers={workers}
          remotes={remotes}
          backends={backends}
          jobWorker={jobWorker}
          error={errors.destination}
        />

        {rule.kind === "refused" ? (
          <p
            role="alert"
            className="flex gap-2 rounded-md bg-danger-soft px-3 py-2 text-[13px] text-danger"
          >
            <TriangleAlert className="mt-0.5 size-4 shrink-0" aria-hidden />
            {rule.message}
          </p>
        ) : rule.kind === "pinned" ? (
          <div className="flex flex-col gap-1.5">
            <span className="text-[13px] font-medium">Worker</span>
            <p className="text-[13px]" data-testid="transfer-worker">
              {jobWorker?.name ?? "The worker"} runs the job: the files are on its disk.
            </p>
            {jobWorker && !isOnline(jobWorker) && (
              <p className="text-xs text-muted-foreground">
                {jobWorker.name} is offline: the job waits until it is back.
              </p>
            )}
            {errors.worker && <p className="text-xs text-danger">{errors.worker}</p>}
          </div>
        ) : (
          <FieldShell
            id="transfer-worker"
            label="Worker"
            error={errors.worker}
            help="Copies between remotes go through a worker. The one that listed these files is proposed."
          >
            <WorkerPicker
              id="transfer-worker"
              workers={rule.workers}
              value={workerId}
              onChange={setChosenWorker}
              invalid={!!errors.worker}
              className="bg-card"
            />
          </FieldShell>
        )}

        <ChoiceCards
          label="If a file is already there"
          labelId="transfer-conflict"
          value={conflict}
          onChange={setConflict}
          options={conflictOptions}
          className="grid-cols-1"
        />

        {errors.form && (
          <p role="alert" className="rounded-md bg-danger-soft px-3 py-2 text-[13px] text-danger">
            {errors.form}
          </p>
        )}
      </div>

      <SheetFooter className="sticky bottom-0 border-t bg-background">
        <p className="text-[13px] text-muted-foreground" data-testid="transfer-summary">
          {transferSummary(op, n, from, destination, conflict, workers, remotes)}
        </p>
        <div className="flex flex-wrap items-center justify-end gap-2">
          <Button
            type="button"
            variant="ghost"
            className="basis-full justify-start px-2 sm:mr-auto sm:basis-auto"
            onClick={openEditor}
          >
            <FilePen aria-hidden />
            Open in the job editor
          </Button>
          <Button type="button" variant="outline" disabled={run.isPending} onClick={preview}>
            <Eye aria-hidden />
            Preview
          </Button>
          <Button type="submit" disabled={run.isPending || rule.kind === "refused"}>
            {run.isPending ? "Queuing…" : op === "move" ? "Move…" : "Copy"}
          </Button>
        </div>
      </SheetFooter>

      <ConfirmDialog
        open={!!confirming}
        onOpenChange={(o) => !o && setConfirming(undefined)}
        title={`Move ${itemsText(n)}?`}
        description={moveText(conflict, itemsText(n), fromText)}
        confirmLabel="Move"
        destructive
        onConfirm={() => (confirming ? start(confirming, false) : undefined)}
      />
    </form>
  );
}
