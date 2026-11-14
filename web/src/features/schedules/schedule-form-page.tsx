import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { toast } from "sonner";

import { ContactLinksEditor } from "@/components/app/contact-links-editor";
import { CronInput } from "@/components/app/cron-input";
import { FieldShell } from "@/components/app/descriptor-form";
import { PageHeader } from "@/components/app/page-header";
import { Panel } from "@/components/app/panel";
import { PolicyFields } from "@/components/app/policy-fields";
import { TargetPicker } from "@/components/app/target-picker";
import { TimezonePicker } from "@/components/app/timezone-picker";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { WorkerAdminService, WorkerStatus } from "@/gen/syncr/v1/api_pb";
import { DashboardService } from "@/gen/syncr/v1/dashboard_pb";
import { ContactService } from "@/gen/syncr/v1/notify_pb";
import { RemoteService } from "@/gen/syncr/v1/remotes_pb";
import { ResticService } from "@/gen/syncr/v1/restic_pb";
import { ScheduleService, type Schedule } from "@/gen/syncr/v1/schedules_pb";
import { useBackendMap } from "@/hooks/use-backends";
import { errorMessage } from "@/lib/errors";
import { invalidateService } from "@/lib/invalidate";
import { labelledWorkers } from "@/lib/labels";
import { sortWorkers } from "@/lib/workers";

import { filtersNeedResync } from "../jobs/bisync-definition";
import {
  definitionWorkerProblem,
  scheduleKinds,
  type JobDefinitionState,
} from "../jobs/definition";
import { JobDefinitionEditor } from "../jobs/job-definition-editor";
import {
  labelTargetAllowed,
  scheduleServerErrors,
  singleWorkerReason,
  toScheduleInput,
  validateSchedule,
  type ScheduleErrors,
  type ScheduleFormState,
} from "./model";

/** ScheduleFormProps configures ScheduleForm. */
export interface ScheduleFormProps {
  initial: ScheduleFormState;
  /** schedule is the stored schedule when editing. */
  schedule?: Schedule;
  /** title and description head the page. */
  title: string;
  description?: string;
}

/**
 * ScheduleFormPage creates or edits a schedule: when it runs (cron and
 * time zone), where (a worker or a label), the job, what happens when a run
 * can't start on time, and who hears about it. The job is a transfer, a
 * two-way sync (on one worker), a backup, a retention run, a check or a
 * prune; restores are never scheduled.
 */
export function ScheduleFormPage({ initial, schedule, title, description }: ScheduleFormProps) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const backends = useBackendMap();
  const workers = useQuery(WorkerAdminService.method.listWorkers, {});
  const remotes = useQuery(RemoteService.method.listRemotes, {});
  const contacts = useQuery(ContactService.method.listContacts, {});
  const repositories = useQuery(ResticService.method.listRepositories, {});
  const create = useMutation(ScheduleService.method.createSchedule);
  const update = useMutation(ScheduleService.method.updateSchedule);
  const [s, setS] = useState<ScheduleFormState>(initial);
  const [errors, setErrors] = useState<ScheduleErrors>({});
  const [formError, setFormError] = useState<string | undefined>();
  const set = <K extends keyof ScheduleFormState>(k: K, v: ScheduleFormState[K]) =>
    setS((p) => ({ ...p, [k]: v }));
  // A job that keeps its state (or its mirror) on one worker cannot run on a label.
  const setDefinition = (definition: JobDefinitionState) =>
    setS((p) => ({
      ...p,
      definition,
      target:
        p.target.kind === "label" && !labelTargetAllowed(definition)
          ? { ...p.target, kind: "worker" }
          : p.target,
    }));
  // The stored definition, when editing: changed filters of a two-way sync need a resync.
  const saved = schedule ? initial.definition : undefined;
  const needsResync =
    saved?.kind === "rclone_bisync" &&
    s.definition.kind === "rclone_bisync" &&
    filtersNeedResync(saved.bisync, s.definition.bisync);
  const workerList = workers.data?.workers ?? [];
  const worker =
    s.target.kind === "worker" ? workerList.find((w) => w.id === s.target.workerId) : undefined;
  // A label has no single worker: paths are browsed on the first online one.
  const labelWorker =
    s.target.kind === "label"
      ? sortWorkers(labelledWorkers(workerList, s.target.label)).find(
          (w) => w.status === WorkerStatus.ONLINE,
        )
      : undefined;
  const label = s.target.kind === "label" ? s.target.label.trim().toLowerCase() : "";
  const browseNote = !label
    ? undefined
    : labelWorker
      ? `Absolute paths on the workers labelled ${label}. Browse shows ${labelWorker.name}, the first one online.`
      : `No worker labelled ${label} is online, so folders cannot be browsed now. Type absolute paths inside the allowed folders of the workers with the label.`;
  // The workers a run may go to, for the hooks they offer and the folders
  // they allow: the chosen one, or every worker with the label that has
  // reported in.
  const runTarget =
    s.target.kind === "label"
      ? {
          workers: labelledWorkers(workerList, s.target.label).filter((w) => w.info),
          label: s.target.label,
        }
      : { workers: worker?.info ? [worker] : [] };
  const pending = create.isPending || update.isPending;
  const single = singleWorkerReason(s.definition);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setFormError(undefined);
    const local = validateSchedule(s);
    setErrors(local);
    if (Object.keys(local).length) {
      setFormError("Check the highlighted fields.");
      return;
    }
    try {
      const input = toScheduleInput(s, worker?.info?.os);
      const res = schedule
        ? await update.mutateAsync({ scheduleId: schedule.id, version: schedule.version, input })
        : await create.mutateAsync({ input });
      await Promise.all([
        invalidateService(queryClient, ScheduleService),
        invalidateService(queryClient, DashboardService),
        invalidateService(queryClient, ContactService),
        invalidateService(queryClient, ResticService),
      ]);
      if (needsResync)
        toast.success(`Saved ${input.name}`, {
          description:
            "The filters changed, so its next run stops with “Resync required”. The schedule's page then offers Resync now (Run now starts that run at once).",
        });
      else toast.success(schedule ? `Saved ${input.name}` : `Created ${input.name}`);
      const id = res.schedule?.id ?? schedule?.id;
      if (id) await navigate({ to: "/schedules/$scheduleId", params: { scheduleId: id } });
    } catch (err) {
      const { errors: server, rest } = scheduleServerErrors(err);
      setErrors(server);
      if (rest.length) setFormError(rest.join(" "));
      else if (!Object.keys(server).length) setFormError(errorMessage(err));
      else setFormError("Check the highlighted fields.");
    }
  };

  return (
    <>
      <PageHeader
        back={{ to: "/schedules", label: "Schedules" }}
        title={title}
        description={description}
      />
      <form onSubmit={(e) => void submit(e)} noValidate className="flex max-w-5xl flex-col gap-6">
        <Panel title="Name">
          <div className="grid grid-cols-1 gap-5 md:grid-cols-[minmax(0,1fr)_minmax(0,1.4fr)]">
            <FieldShell id="sched-name" label="Name" error={errors.name}>
              <Input
                id="sched-name"
                value={s.name}
                onChange={(e) => set("name", e.target.value)}
                placeholder="Nightly photos → B2"
                className="bg-card"
                maxLength={64}
                autoComplete="off"
                aria-invalid={errors.name ? true : undefined}
              />
            </FieldShell>
            <FieldShell id="sched-desc" label="Description" optional>
              <Input
                id="sched-desc"
                value={s.description}
                onChange={(e) => set("description", e.target.value)}
                placeholder="What it is for, so others know"
                className="bg-card"
                autoComplete="off"
              />
            </FieldShell>
          </div>
        </Panel>

        <Panel
          title="When"
          description="A cron expression: minute, hour, day of month, month and day of week."
        >
          <div className="grid grid-cols-1 gap-5 md:grid-cols-[minmax(0,1.4fr)_minmax(0,1fr)]">
            <FieldShell id="sched-cron" label="Runs at">
              <CronInput
                id="sched-cron"
                value={s.cron}
                onChange={(v) => set("cron", v)}
                timezone={s.timezone}
                error={errors.cron}
              />
            </FieldShell>
            <FieldShell
              id="sched-tz"
              label="Time zone"
              error={errors.timezone}
              help="Runs follow this zone's clock, daylight saving included."
            >
              <TimezonePicker
                id="sched-tz"
                value={s.timezone}
                onChange={(v) => set("timezone", v)}
                invalid={!!errors.timezone}
              />
            </FieldShell>
          </div>
        </Panel>

        <JobDefinitionEditor
          idPrefix="sched"
          value={s.definition}
          onChange={setDefinition}
          saved={saved}
          errors={errors}
          remotes={remotes.data?.remotes ?? []}
          backends={backends}
          repositories={repositories.data?.repositories ?? []}
          kinds={scheduleKinds}
          worker={worker}
          browseWorker={labelWorker}
          browseNote={browseNote}
          runTarget={runTarget}
          where={
            <TargetPicker
              id="sched-target"
              value={s.target}
              onChange={(v) => set("target", v)}
              workers={workerList}
              error={errors.target}
              labelDisabled={
                single
                  ? `${single}, so its schedule runs on a specific worker, not on any worker with a label.`
                  : undefined
              }
              unavailable={definitionWorkerProblem(s.definition)}
            />
          }
        />

        <Panel title="When a run can't start on time">
          <PolicyFields
            idPrefix="sched-policy"
            missed={s.missed}
            onMissedChange={(v) => set("missed", v)}
            overlap={s.overlap}
            onOverlapChange={(v) => set("overlap", v)}
            missedError={errors.missed}
          />
        </Panel>

        <Panel
          title="Notifications"
          description="Who hears about this schedule's runs, and for which outcomes."
        >
          <ContactLinksEditor
            value={s.contacts}
            onChange={(v) => set("contacts", v)}
            contacts={contacts.data?.contacts ?? []}
            errors={errors.contacts}
          />
        </Panel>

        {formError && (
          <p role="alert" className="rounded-md bg-danger-soft px-3 py-2 text-[13px] text-danger">
            {formError}
          </p>
        )}
        <div className="flex flex-col-reverse gap-3 sm:flex-row sm:items-center sm:justify-between">
          <Label className="flex items-center gap-2.5 text-[13px] font-normal">
            <Switch checked={s.enabled} onCheckedChange={(v) => set("enabled", v)} />
            {s.enabled
              ? "Active: runs start at the times above"
              : "Paused: no runs start until you resume it"}
          </Label>
          <div className="flex justify-end gap-2">
            <Button
              type="button"
              variant="outline"
              onClick={() =>
                void (schedule
                  ? navigate({ to: "/schedules/$scheduleId", params: { scheduleId: schedule.id } })
                  : navigate({ to: "/schedules" }))
              }
            >
              Cancel
            </Button>
            <Button type="submit" disabled={pending}>
              {pending ? "Saving…" : schedule ? "Save changes" : "Create schedule"}
            </Button>
          </div>
        </div>
      </form>
    </>
  );
}
