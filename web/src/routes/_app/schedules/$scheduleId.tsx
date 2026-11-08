import { createFileRoute } from "@tanstack/react-router";

import { ScheduleDetailPage } from "@/features/schedules/schedule-detail-page";

export const Route = createFileRoute("/_app/schedules/$scheduleId")({
  component: ScheduleDetail,
});

function ScheduleDetail() {
  const { scheduleId } = Route.useParams();
  return <ScheduleDetailPage key={scheduleId} scheduleId={scheduleId} />;
}
