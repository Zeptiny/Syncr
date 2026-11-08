import type { MessageInitShape } from "@bufbuild/protobuf";
import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { CalendarClock } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { FieldShell } from "@/components/app/descriptor-form";
import { PageHeader } from "@/components/app/page-header";
import { WorkerPicker } from "@/components/app/worker-picker";
import { Button } from "@/components/ui/button";
import {
  JobService,
  WorkerAdminService,
  type Job,
  type RunJobRequestSchema,
} from "@/gen/syncr/v1/api_pb";
import { RemoteService } from "@/gen/syncr/v1/remotes_pb";
import { useBackendMap } from "@/hooks/use-backends";
import { errorMessage, fieldViolations } from "@/lib/errors";
import { invalidateService } from "@/lib/invalidate";

import {
  definitionError,
  definitionState,
  emptyDefinition,
  toDefinition,
  validateDefinition,
  type DefinitionErrors,
  type DefinitionState,
} from "./definition";
import { JobDefinitionEditor } from "./job-definition-editor";

/** RunJobState is the run-job form: a worker and the job definition. */
export interface RunJobState {
  workerId: string;
  definition: DefinitionState;
}

type Errors = DefinitionErrors & { worker?: string };

/** emptyRunJob is a blank form. */
export function emptyRunJob(): RunJobState {
  return { workerId: "", definition: emptyDefinition() };
}

/** fromJob prefills the form from an earlier job ("Edit and run"). */
export function fromJob(job: Job): RunJobState {
  return { workerId: job.workerId, definition: definitionState(job.definition) };
}

/** validateRunJob checks what the browser can check before sending. */
export function validateRunJob(s: RunJobState): Errors {
  const e: Errors = validateDefinition(s.definition);
  if (!s.workerId) e.worker = "Choose the worker that runs the job.";
  return e;
}

/** toRunJobRequest builds the RunJob request from the form. */
export function toRunJobRequest(s: RunJobState): MessageInitShape<typeof RunJobRequestSchema> {
  return { workerId: s.workerId, definition: toDefinition(s.definition) };
}

/** serverErrors places API violations on the form's sections. */
function serverErrors(err: unknown): { errors: Errors; rest: string[] } {
  const errors: Errors = {};
  const rest: string[] = [];
  for (const v of fieldViolations(err)) {
    if (v.field.includes("worker")) errors.worker = v.message;
    else if (!definitionError(v, errors)) rest.push(`${v.field}: ${v.message}`);
  }
  return { errors, rest };
}

/** RunJobPage queues a transfer on a worker. */
export function RunJobPage({ from }: { from?: Job }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const backends = useBackendMap();
  const workers = useQuery(WorkerAdminService.method.listWorkers, {});
  const remotes = useQuery(RemoteService.method.listRemotes, {});
  const run = useMutation(JobService.method.runJob);
  const [s, setS] = useState<RunJobState>(() => (from ? fromJob(from) : emptyRunJob()));
  const [errors, setErrors] = useState<Errors>({});
  const [formError, setFormError] = useState<string | undefined>();
  const worker = workers.data?.workers.find((w) => w.id === s.workerId);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setFormError(undefined);
    const local = validateRunJob(s);
    setErrors(local);
    if (Object.keys(local).length) return;
    try {
      const res = await run.mutateAsync(toRunJobRequest(s));
      await invalidateService(queryClient, JobService);
      toast.success(s.definition.dryRun ? "Dry run queued" : "Job queued");
      if (res.job) await navigate({ to: "/jobs/$jobId", params: { jobId: res.job.id } });
    } catch (err) {
      const { errors: server, rest } = serverErrors(err);
      setErrors(server);
      if (rest.length) setFormError(rest.join(" "));
      else if (!Object.keys(server).length) setFormError(errorMessage(err));
    }
  };

  return (
    <>
      <PageHeader
        back={{ to: "/jobs", label: "Jobs" }}
        title={from ? "Run again with changes" : "Run a job"}
        description="Choose where the files are, where they go and which worker moves them. The job starts as soon as the worker picks it up."
        actions={
          from && (
            <Button variant="outline" asChild>
              <Link to="/schedules/new" search={{ job: from.id }}>
                <CalendarClock aria-hidden />
                Schedule this job
              </Link>
            </Button>
          )
        }
      />
      <form onSubmit={(e) => void submit(e)} noValidate className="flex max-w-5xl flex-col gap-6">
        <JobDefinitionEditor
          value={s.definition}
          onChange={(definition) => setS((p) => ({ ...p, definition }))}
          errors={errors}
          remotes={remotes.data?.remotes ?? []}
          backends={backends}
          worker={worker}
          how={
            <FieldShell
              id="run-worker"
              label="Worker"
              error={errors.worker}
              help="Online workers are listed first."
            >
              <WorkerPicker
                id="run-worker"
                workers={workers.data?.workers ?? []}
                value={s.workerId}
                onChange={(v) => setS((p) => ({ ...p, workerId: v }))}
                invalid={!!errors.worker}
                className="bg-card"
              />
            </FieldShell>
          }
        />
        {formError && (
          <p role="alert" className="rounded-md bg-danger-soft px-3 py-2 text-[13px] text-danger">
            {formError}
          </p>
        )}
        <div className="flex justify-end gap-2">
          <Button type="button" variant="outline" onClick={() => void navigate({ to: "/jobs" })}>
            Cancel
          </Button>
          <Button type="submit" disabled={run.isPending}>
            {run.isPending ? "Queuing…" : s.definition.dryRun ? "Start dry run" : "Run job"}
          </Button>
        </div>
      </form>
    </>
  );
}
