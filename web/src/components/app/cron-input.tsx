import { timestampDate } from "@bufbuild/protobuf/wkt";
import { useQuery } from "@connectrpc/connect-query";
import { keepPreviousData } from "@tanstack/react-query";
import { CalendarClock, ChevronDown, SunMoon } from "lucide-react";
import { useMemo } from "react";

import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { Input } from "@/components/ui/input";
import { Tooltip, TooltipContent, TooltipTrigger } from "@/components/ui/tooltip";
import { ScheduleService } from "@/gen/syncr/v1/schedules_pb";
import { useDebounced } from "@/hooks/use-debounced";
import { cronPresets, describeCron } from "@/lib/cron";
import { errorMessage, fieldViolations } from "@/lib/errors";
import { cn } from "@/lib/utils";

import { hasOffsetChange } from "@/lib/timezones";

import { NextRunsList } from "./next-runs-list";

/** CronInputProps configures CronInput. */
export interface CronInputProps {
  id: string;
  value: string;
  onChange: (v: string) => void;
  /** timezone is the schedule's zone; previews are computed in it. */
  timezone: string;
  /** error is a submit error for the field, shown instead of the preview's. */
  error?: string;
  /** count is how many upcoming runs to show. */
  count?: number;
}

/**
 * CronInput edits a five-field cron expression. It describes it in words
 * as you type (cronstrue) and lists the next runs, which the controller
 * computes (PreviewSchedule), so daylight-saving rules live in one place.
 */
export function CronInput({ id, value, onChange, timezone, error, count = 5 }: CronInputProps) {
  const described = describeCron(value);
  const cron = useDebounced(value.trim(), 300);
  const tz = useDebounced(timezone, 300);
  const settled = describeCron(cron);
  const preview = useQuery(
    ScheduleService.method.previewSchedule,
    { cron, timezone: tz, count },
    {
      enabled: settled.ok && !!tz,
      placeholderData: keepPreviousData,
      retry: false,
      staleTime: 30_000,
    },
  );
  const runs = useMemo(
    () => (preview.data?.nextRuns ?? []).map((t) => timestampDate(t)),
    [preview.data],
  );
  const serverError = preview.error
    ? (fieldViolations(preview.error).find((v) => v.field === "cron" || v.field === "timezone")
        ?.message ?? errorMessage(preview.error))
    : undefined;
  const problem = error ?? (described.ok ? serverError : described.error);
  const stale = preview.isPlaceholderData || cron !== value.trim();
  const dst = runs.length > 1 && hasOffsetChange(runs, tz);

  return (
    <div className="flex flex-col gap-3" data-field={id}>
      <div className="flex gap-2">
        <Input
          id={id}
          value={value}
          onChange={(e) => onChange(e.target.value)}
          className="bg-card font-mono text-[13.5px] tracking-wide"
          spellCheck={false}
          autoComplete="off"
          placeholder="30 2 * * *"
          aria-invalid={problem ? true : undefined}
          aria-describedby={`${id}-desc`}
        />
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Button type="button" variant="outline" className="shrink-0 bg-card">
              Presets
              <ChevronDown className="opacity-60" aria-hidden />
            </Button>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end" className="w-64">
            {cronPresets.map((p) => (
              <DropdownMenuItem key={p.expr} onSelect={() => onChange(p.expr)}>
                <span className="flex-1">{p.label}</span>
                <span className="font-mono text-xs text-muted-foreground">{p.expr}</span>
              </DropdownMenuItem>
            ))}
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
      <div id={`${id}-desc`} aria-live="polite">
        {problem ? (
          <p role="alert" className="text-xs leading-5 text-danger">
            {problem}
          </p>
        ) : (
          described.ok && (
            <p className="flex items-center gap-1.5 text-[13px]">
              <CalendarClock className="size-4 shrink-0 text-primary" aria-hidden />
              <span>{described.text}</span>
              <span className="text-muted-foreground">({timezone})</span>
            </p>
          )
        )}
      </div>
      {!problem && runs.length > 0 && (
        <div className="rounded-lg border bg-muted/40 px-3.5 pt-2 pb-1.5">
          <div className="flex items-center justify-between gap-2 pb-1">
            <span className="text-xs font-medium text-muted-foreground">Next runs</span>
            {dst && (
              <Tooltip>
                <TooltipTrigger asChild>
                  <span
                    tabIndex={0}
                    className="inline-flex items-center gap-1 rounded-full bg-warning-soft px-2 py-0.5 text-[11px] font-medium text-warning"
                  >
                    <SunMoon className="size-3" aria-hidden />
                    Clocks change
                  </span>
                </TooltipTrigger>
                <TooltipContent className="max-w-64">
                  These runs cross a daylight-saving change. They keep their wall-clock time; a time
                  the clocks skip runs when they jump forward.
                </TooltipContent>
              </Tooltip>
            )}
          </div>
          <NextRunsList runs={runs} timezone={tz} dim={stale} />
        </div>
      )}
      {!problem && settled.ok && preview.isPending && preview.fetchStatus !== "idle" && (
        <p className={cn("text-xs text-muted-foreground")}>Working out the next runs…</p>
      )}
    </div>
  );
}
