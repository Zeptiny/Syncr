import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { toast } from "sonner";

import { JobService } from "@/gen/syncr/v1/api_pb";
import { DashboardService } from "@/gen/syncr/v1/dashboard_pb";
import { ScheduleRunStatus, ScheduleService, type Schedule } from "@/gen/syncr/v1/schedules_pb";
import { errorMessage } from "@/lib/errors";
import { invalidateService } from "@/lib/invalidate";

/**
 * useScheduleActions runs a schedule now and pauses or resumes it, with a
 * toast for the outcome. Lists and the dashboard are refreshed afterwards.
 */
export function useScheduleActions() {
  const queryClient = useQueryClient();
  const runNow = useMutation(ScheduleService.method.runScheduleNow);
  const setEnabled = useMutation(ScheduleService.method.setScheduleEnabled);
  const refresh = async () => {
    await Promise.all([
      invalidateService(queryClient, ScheduleService),
      invalidateService(queryClient, JobService),
      invalidateService(queryClient, DashboardService),
    ]);
  };
  return {
    pending: runNow.isPending || setEnabled.isPending,
    runNow: async (s: Schedule) => {
      try {
        const res = await runNow.mutateAsync({ scheduleId: s.id });
        await refresh();
        if (res.run?.status === ScheduleRunStatus.PENDING)
          toast.info(`${s.name} is waiting for its worker`, {
            description: "It starts as soon as the worker is online, within its deadline.",
          });
        else toast.success(`Started ${s.name}`);
      } catch (err) {
        toast.error(`Couldn't run ${s.name}`, { description: errorMessage(err) });
      }
    },
    setEnabled: async (s: Schedule, enabled: boolean) => {
      try {
        await setEnabled.mutateAsync({ scheduleId: s.id, enabled });
        await refresh();
        toast.success(enabled ? `Resumed ${s.name}` : `Paused ${s.name}`);
      } catch (err) {
        toast.error(errorMessage(err));
      }
    },
  };
}
