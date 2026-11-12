import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { useCallback } from "react";
import { toast } from "sonner";

import { JobService } from "@/gen/syncr/v1/api_pb";
import { DashboardService } from "@/gen/syncr/v1/dashboard_pb";
import { ScheduleRunStatus, ScheduleService, type Schedule } from "@/gen/syncr/v1/schedules_pb";
import { errorMessage } from "@/lib/errors";
import { invalidateService } from "@/lib/invalidate";

/**
 * useScheduleActions runs a schedule now and pauses or resumes it, with a
 * toast for the outcome. Lists and the dashboard are refreshed afterwards.
 * runNow and setEnabled keep their identity across renders, so tables may
 * depend on them in memoized column definitions.
 */
export function useScheduleActions() {
  const queryClient = useQueryClient();
  const runNowMutation = useMutation(ScheduleService.method.runScheduleNow);
  const setEnabledMutation = useMutation(ScheduleService.method.setScheduleEnabled);
  // mutateAsync is stable for the life of the mutation; the mutation
  // result object is new on every render.
  const runScheduleNow = runNowMutation.mutateAsync;
  const setScheduleEnabled = setEnabledMutation.mutateAsync;
  const refresh = useCallback(async () => {
    await Promise.all([
      invalidateService(queryClient, ScheduleService),
      invalidateService(queryClient, JobService),
      invalidateService(queryClient, DashboardService),
    ]);
  }, [queryClient]);
  const runNow = useCallback(
    async (s: Schedule) => {
      try {
        const res = await runScheduleNow({ scheduleId: s.id });
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
    [runScheduleNow, refresh],
  );
  const setEnabled = useCallback(
    async (s: Schedule, enabled: boolean) => {
      try {
        await setScheduleEnabled({ scheduleId: s.id, enabled });
        await refresh();
        toast.success(enabled ? `Resumed ${s.name}` : `Paused ${s.name}`);
      } catch (err) {
        toast.error(errorMessage(err));
      }
    },
    [setScheduleEnabled, refresh],
  );
  return {
    pending: runNowMutation.isPending || setEnabledMutation.isPending,
    runNow,
    setEnabled,
  };
}
