import { ArrowLeftRight, ArrowRight } from "lucide-react";

import { kindIcons, otherKindIcon } from "@/components/app/kind-icons";
import type { Schedule } from "@/gen/syncr/v1/schedules_pb";
import { kindLabel } from "@/lib/status";
import { cn } from "@/lib/utils";

import { jobRoute, resticSummary, type EndpointLabel } from "../jobs/model";
import { asJob, kindOf } from "./model";

/** endpointText is a location in a few words: a remote's name, or a worker path. */
function endpointText(e: EndpointLabel): string {
  return e.local ? e.path || "/" : (e.name ?? "");
}

/** endpointTitle is a location in full: the remote's name and its path. */
function endpointTitle(e: EndpointLabel): string {
  if (e.local) return e.path || "/";
  return e.path ? `${e.name}: ${e.path}` : (e.name ?? "");
}

function EndpointShort({ e }: { e: EndpointLabel }) {
  return (
    <span
      className={cn(
        "min-w-0 truncate",
        e.local && "font-mono text-[11.5px]",
        e.deleted && "line-through",
      )}
    >
      {endpointText(e)}
    </span>
  );
}

/**
 * ScheduleWhat says in one line what a schedule runs: its kind's icon, then
 * the transfer's or two-way sync's two locations (remotes by name, worker
 * folders by path; the full locations are in the tooltip), or the restic
 * job's summary.
 */
export function ScheduleWhat({ schedule, className }: { schedule: Schedule; className?: string }) {
  const kind = kindOf(schedule);
  const Icon = kindIcons[kind] ?? otherKindIcon;
  const route = jobRoute(asJob(schedule));
  const restic = route
    ? undefined
    : resticSummary(schedule.definition, schedule.repository, schedule.remotes);
  const Arrow = route?.twoWay ? ArrowLeftRight : ArrowRight;
  return (
    <div
      className={cn("flex min-w-0 items-center gap-1.5 text-xs text-muted-foreground", className)}
    >
      <Icon className="size-3.5 shrink-0" aria-label={kindLabel(kind)} />
      {route ? (
        <span
          className="flex min-w-0 items-center gap-1.5"
          title={`${endpointTitle(route.a)} ${route.twoWay ? "⇄" : "→"} ${endpointTitle(route.b)}`}
        >
          <EndpointShort e={route.a} />
          <Arrow className="size-3 shrink-0" aria-label={route.twoWay ? "and" : "to"} />
          <EndpointShort e={route.b} />
        </span>
      ) : (
        <span className="truncate" title={restic?.title}>
          {restic?.title ?? kindLabel(kind)}
        </span>
      )}
    </div>
  );
}
