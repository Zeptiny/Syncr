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

/** schedulesText counts schedules in words: "1 schedule", "3 schedules". */
export function schedulesText(n: number): string {
  return `${n} ${n === 1 ? "schedule" : "schedules"}`;
}

/** Outcome is how a bulk action went, schedule by schedule. */
interface Outcome<R> {
  done: { s: Schedule; result: R }[];
  failed: { s: Schedule; err: unknown }[];
}

/** eachSchedule runs fn for every schedule, a few at a time, and collects how each went. */
async function eachSchedule<R>(
  list: Schedule[],
  fn: (s: Schedule) => Promise<R>,
): Promise<Outcome<R>> {
  const out: Outcome<R> = { done: [], failed: [] };
  const queue = [...list];
  const worker = async () => {
    for (let s = queue.shift(); s; s = queue.shift()) {
      try {
        out.done.push({ s, result: await fn(s) });
      } catch (err) {
        out.failed.push({ s, err });
      }
    }
  };
  await Promise.all(Array.from({ length: Math.min(4, list.length) }, worker));
  return out;
}

/** failureText lists what failed, one schedule per line (the first few). */
function failureText(failed: { s: Schedule; err: unknown }[]): string {
  const lines = failed.slice(0, 3).map(({ s, err }) => `${s.name}: ${errorMessage(err)}`);
  if (failed.length > 3) lines.push(`and ${failed.length - 3} more`);
  return lines.join("\n");
}

/**
 * useBulkScheduleActions runs, pauses, resumes, deletes and moves several
 * schedules at once, with one toast for the outcome. Each schedule is its
 * own request (a move is one request for all), so one that fails does not
 * stop the others; the toast names those that failed.
 */
export function useBulkScheduleActions() {
  const queryClient = useQueryClient();
  const runNow = useMutation(ScheduleService.method.runScheduleNow).mutateAsync;
  const setEnabled = useMutation(ScheduleService.method.setScheduleEnabled).mutateAsync;
  const del = useMutation(ScheduleService.method.deleteSchedule).mutateAsync;
  const move = useMutation(ScheduleService.method.moveSchedules).mutateAsync;
  const refresh = useCallback(async () => {
    await Promise.all([
      invalidateService(queryClient, ScheduleService),
      invalidateService(queryClient, JobService),
      invalidateService(queryClient, DashboardService),
    ]);
  }, [queryClient]);

  /** report toasts a bulk outcome: past ("Paused") when all went well, else base ("pause") with the failures. */
  const report = useCallback(
    <R>(o: Outcome<R>, total: number, past: string, base: string, extra?: string) => {
      if (!total) return;
      if (!o.failed.length) {
        toast.success(
          `${past} ${schedulesText(total)}`,
          extra ? { description: extra } : undefined,
        );
        return;
      }
      const title = o.done.length
        ? `${past} ${o.done.length} of ${schedulesText(total)}`
        : `Couldn't ${base} ${total === 1 ? "the schedule" : `the ${schedulesText(total)}`}`;
      toast.error(title, { description: failureText(o.failed) });
    },
    [],
  );

  const runMany = useCallback(
    async (list: Schedule[]) => {
      const o = await eachSchedule(list, (s) => runNow({ scheduleId: s.id }));
      await refresh();
      const waiting = o.done.filter(
        (d) => d.result.run?.status === ScheduleRunStatus.PENDING,
      ).length;
      report(
        o,
        list.length,
        "Started",
        "run",
        waiting
          ? `${waiting} ${waiting === 1 ? "waits" : "wait"} for ${waiting === 1 ? "its worker" : "their workers"}.`
          : undefined,
      );
    },
    [runNow, refresh, report],
  );

  /** setEnabledMany pauses or resumes the schedules that are not that way yet. */
  const setEnabledMany = useCallback(
    async (list: Schedule[], enabled: boolean) => {
      const todo = list.filter((s) => s.enabled !== enabled);
      const o = await eachSchedule(todo, (s) => setEnabled({ scheduleId: s.id, enabled }));
      await refresh();
      report(o, todo.length, enabled ? "Resumed" : "Paused", enabled ? "resume" : "pause");
    },
    [setEnabled, refresh, report],
  );

  /** deleteMany deletes the schedules; it rejects when none could be deleted (the dialog shows why). */
  const deleteMany = useCallback(
    async (list: Schedule[]) => {
      const o = await eachSchedule(list, (s) => del({ scheduleId: s.id }));
      await refresh();
      if (!o.done.length && o.failed.length) throw o.failed[0].err;
      report(o, list.length, "Deleted", "delete");
    },
    [del, refresh, report],
  );

  /** moveMany moves the schedules into the group ("" moves them out of theirs), all or none. */
  const moveMany = useCallback(
    async (list: Schedule[], groupId: string, groupName?: string) => {
      try {
        await move({ scheduleIds: list.map((s) => s.id), groupId });
        await refresh();
        toast.success(
          groupId
            ? `Moved ${schedulesText(list.length)} to ${groupName ?? "the group"}`
            : `Moved ${schedulesText(list.length)} out of their group`,
        );
      } catch (err) {
        toast.error(`Couldn't move ${list.length === 1 ? "the schedule" : "the schedules"}`, {
          description: errorMessage(err),
        });
      }
    },
    [move, refresh],
  );

  return { runMany, setEnabledMany, deleteMany, moveMany };
}
