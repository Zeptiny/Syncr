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
import { ResticService } from "@/gen/syncr/v1/restic_pb";
import { useBackendMap } from "@/hooks/use-backends";
import { errorMessage, fieldViolations } from "@/lib/errors";
import { invalidateService } from "@/lib/invalidate";
import { isResticKind } from "@/lib/status";

import { draftRunJob, type ParsedDraft } from "../files/job-drafts";

import {
  definitionError,
  definitionState,
  emptyDefinition,
  forWorkerOS,
  isDryRun,
  toDefinition,
  validateDefinition,
  type DefinitionErrors,
  type EditableKind,
  type JobDefinitionState,
} from "./definition";
import { JobDefinitionEditor } from "./job-definition-editor";

/** RunJobState is the run-job form: a worker and the job definition. */
export interface RunJobState {
  workerId: string;
  definition: JobDefinitionState;
}

type Errors = DefinitionErrors & { worker?: string };

/** emptyRunJob is a blank form of a kind, on a repository if given. */
export function emptyRunJob(kind?: EditableKind, repositoryId?: string): RunJobState {
  return { workerId: "", definition: emptyDefinition(kind, repositoryId) };
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

/**
 * toRunJobRequest builds the RunJob request from the form. os is the
 * chosen worker's OS: what it cannot do (VSS off Windows) is left out.
 */
export function toRunJobRequest(
  s: RunJobState,
  os?: string,
): MessageInitShape<typeof RunJobRequestSchema> {
  return { workerId: s.workerId, definition: toDefinition(forWorkerOS(s.definition, os)) };
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

/** RunJobPageProps configures RunJobPage. */
export interface RunJobPageProps {
  /** from is an earlier job to run again with changes. */
  from?: Job;
  /** draft is a job prepared elsewhere, e.g. in the file browser (job-drafts.ts). */
  draft?: ParsedDraft;
  /** kind and repository preselect the form (from the URL). */
  kind?: EditableKind;
  repository?: string;
}

/**
 * RunJobPage queues a job on a worker: a transfer, a two-way sync, a
 * backup, a retention run, a check or a prune.
 */
export function RunJobPage({ from, draft, kind, repository }: RunJobPageProps) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const backends = useBackendMap();
  const workers = useQuery(WorkerAdminService.method.listWorkers, {});
  const remotes = useQuery(RemoteService.method.listRemotes, {});
  const repositories = useQuery(ResticService.method.listRepositories, {});
  const run = useMutation(JobService.method.runJob);
  const [s, setS] = useState<RunJobState>(() =>
    from ? fromJob(from) : draft ? draftRunJob(draft) : emptyRunJob(kind, repository),
  );
  const [saved] = useState(() => (from ? definitionState(from.definition) : undefined));
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
      const res = await run.mutateAsync(toRunJobRequest(s, worker?.info?.os));
      await invalidateService(queryClient, JobService);
      if (isResticKind(s.definition.kind)) await invalidateService(queryClient, ResticService);
      toast.success(isDryRun(s.definition) ? "Dry run queued" : "Job queued");
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
        description="Choose what to do and which worker does it. The job starts as soon as the worker picks it up; jobs on the same repository wait for each other."
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
          repositories={repositories.data?.repositories ?? []}
          worker={worker}
          saved={saved}
          onKindChange={(k) =>
            void navigate({
              to: "/jobs/new",
              search: (prev) => ({ ...prev, kind: k === "rclone_transfer" ? undefined : k }),
              replace: true,
            })
          }
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
            {run.isPending ? "Queuing…" : isDryRun(s.definition) ? "Start dry run" : "Run job"}
          </Button>
        </div>
      </form>
    </>
  );
}
