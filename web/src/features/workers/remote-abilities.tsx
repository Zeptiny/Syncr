import { Check, X } from "lucide-react";

import type { Worker } from "@/gen/syncr/v1/api_pb";
import { workerAbilities } from "@/lib/remote-sources";
import { cn } from "@/lib/utils";

/**
 * RemoteAbilities shows what a worker can do for backups of a remote:
 * "Mirror ✓ · Mount ✗ – /dev/fuse is missing". A worker that reported
 * nothing (an older release, or one not connected yet) shows empty.
 * stacked puts each method on its own line, for narrow places.
 */
export function RemoteAbilities({
  worker,
  empty = null,
  stacked,
  className,
}: {
  worker: Worker;
  empty?: React.ReactNode;
  stacked?: boolean;
  className?: string;
}) {
  const rows = workerAbilities(worker);
  if (!rows) return <>{empty}</>;
  return (
    <span
      className={cn(
        "inline-flex text-[13px]",
        stacked ? "flex-col gap-0.5" : "flex-wrap items-baseline gap-x-2 gap-y-0.5",
        className,
      )}
    >
      <span className="sr-only">
        {rows
          .map((r) => `${r.label}: ${r.ok ? "yes" : `no${r.reason ? `, ${r.reason}` : ""}`}.`)
          .join(" ")}
      </span>
      {rows.map((r, i) => (
        <span key={r.label} className="inline-flex items-start gap-1" aria-hidden>
          {!stacked && i > 0 && <span className="text-muted-foreground">·</span>}
          <span className="shrink-0">{r.label}</span>
          {r.ok ? (
            <Check className="mt-[3px] size-3.5 shrink-0 text-success" />
          ) : (
            <X className="mt-[3px] size-3.5 shrink-0 text-danger" />
          )}
          {!r.ok && r.reason && (
            <span className="min-w-0 pt-px text-xs leading-[18px] text-muted-foreground">
              {/* A no-break space keeps the dash with the reason's first word. */}–{"\u00a0"}
              {r.reason}
            </span>
          )}
        </span>
      ))}
    </span>
  );
}
