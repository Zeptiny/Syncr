import { useQuery } from "@connectrpc/connect-query";
import { createFileRoute, useLocation } from "@tanstack/react-router";
import { useMemo } from "react";

import { ErrorState } from "@/components/app/error-state";
import { Skeleton } from "@/components/ui/skeleton";
import { JobService } from "@/gen/syncr/v1/api_pb";
import { ScheduleService } from "@/gen/syncr/v1/schedules_pb";
import { draftSchedule, readJobDraft } from "@/features/files/job-drafts";
import { emptySchedule, scheduleFromJob, scheduleState } from "@/features/schedules/model";
import { ScheduleFormPage } from "@/features/schedules/schedule-form-page";
import { validateNewScheduleSearch } from "@/features/schedules/search";

export const Route = createFileRoute("/_app/schedules/new")({
  validateSearch: validateNewScheduleSearch,
  component: NewSchedule,
});

function NewSchedule() {
  const { from, job, kind, repository } = Route.useSearch();
  const state = useLocation().state.jobDraft;
  const draft = useMemo(() => readJobDraft({ jobDraft: state }), [state]);
  const src = useQuery(
    ScheduleService.method.getSchedule,
    { scheduleId: from ?? "" },
    { enabled: !!from },
  );
  const j = useQuery(JobService.method.getJob, { jobId: job ?? "" }, { enabled: !!job && !from });
  const title = "New schedule";
  const description =
    "Run a job by itself at the times you choose. If its worker is offline then, it can run when the worker is back.";
  if (from) {
    if (src.error) return <ErrorState error={src.error} onRetry={() => void src.refetch()} />;
    if (!src.data?.schedule) return <Skeleton className="h-96 w-full max-w-5xl" />;
    const s = src.data.schedule;
    return (
      <ScheduleFormPage
        key={from}
        title={`Duplicate ${s.name}`}
        description={description}
        initial={{ ...scheduleState(s), name: `${s.name} (copy)`, enabled: true }}
      />
    );
  }
  if (job) {
    if (j.error) return <ErrorState error={j.error} onRetry={() => void j.refetch()} />;
    if (!j.data?.job) return <Skeleton className="h-96 w-full max-w-5xl" />;
    return (
      <ScheduleFormPage
        key={job}
        title="Schedule this job"
        description={description}
        initial={scheduleFromJob(j.data.job)}
      />
    );
  }
  return (
    <ScheduleFormPage
      title={title}
      description={description}
      initial={draft ? draftSchedule(draft) : emptySchedule(undefined, kind, repository)}
    />
  );
}
