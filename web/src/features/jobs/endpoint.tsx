import { ArrowLeftRight, ArrowRight, HardDrive } from "lucide-react";

import { cn } from "@/lib/utils";

import type { EndpointLabel, JobRoute } from "./model";

/** Endpoint renders a job location: remote name and path, or a local path. */
export function Endpoint({ e, className }: { e: EndpointLabel; className?: string }) {
  if (e.local) {
    return (
      <span className={cn("inline-flex min-w-0 items-center gap-1.5", className)}>
        <HardDrive
          className="size-3.5 shrink-0 text-muted-foreground"
          aria-label="Worker filesystem"
        />
        <span className="truncate font-mono text-[12.5px]">{e.path || "/"}</span>
      </span>
    );
  }
  return (
    <span className={cn("inline-flex min-w-0 items-baseline gap-1.5", className)}>
      <span
        className={cn("truncate font-medium", e.deleted && "text-muted-foreground line-through")}
      >
        {e.name}
      </span>
      {e.path && (
        <span className="truncate font-mono text-[12px] text-muted-foreground">{e.path}</span>
      )}
    </span>
  );
}

/**
 * RouteEndpoints renders a route's two locations joined by its direction:
 * an arrow for a transfer, a two-way arrow for a two-way sync.
 * endpointClassName styles each location (e.g. a maximum width).
 */
export function RouteEndpoints({
  route,
  className,
  endpointClassName,
}: {
  route: JobRoute;
  className?: string;
  endpointClassName?: string;
}) {
  const Arrow = route.twoWay ? ArrowLeftRight : ArrowRight;
  return (
    <div className={cn("flex min-w-0 items-center gap-2", className)}>
      <Endpoint e={route.a} className={endpointClassName} />
      <Arrow
        className="size-3.5 shrink-0 text-muted-foreground"
        aria-label={route.twoWay ? "and" : "to"}
      />
      <Endpoint e={route.b} className={endpointClassName} />
    </div>
  );
}
