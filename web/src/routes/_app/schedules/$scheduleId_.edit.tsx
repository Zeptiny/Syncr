import { useQuery } from "@connectrpc/connect-query";
import { createFileRoute } from "@tanstack/react-router";

import { ErrorState } from "@/components/app/error-state";
import { Skeleton } from "@/components/ui/skeleton";
import { ScheduleService } from "@/gen/syncr/v1/schedules_pb";
import { scheduleState } from "@/features/schedules/model";
import { ScheduleFormPage } from "@/features/schedules/schedule-form-page";

export const Route = createFileRoute("/_app/schedules/$scheduleId_/edit")({
  component: EditSchedule,
});

function EditSchedule() {
  const { scheduleId } = Route.useParams();
  const q = useQuery(ScheduleService.method.getSchedule, { scheduleId });
  if (q.error) return <ErrorState error={q.error} onRetry={() => void q.refetch()} />;
  const s = q.data?.schedule;
  if (!s) return <Skeleton className="h-96 w-full max-w-5xl" />;
  return (
    <ScheduleFormPage
      key={`${s.id}:${s.version}`}
      schedule={s}
      title={`Edit ${s.name}`}
      initial={scheduleState(s)}
    />
  );
}
