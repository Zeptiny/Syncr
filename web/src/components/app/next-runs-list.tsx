import { useNow } from "@/hooks/use-now";
import { formatDateTime, formatRelative } from "@/lib/format";
import { formatInZone, tzOffset } from "@/lib/timezones";
import { cn } from "@/lib/utils";

const months = new Intl.RelativeTimeFormat("en", { numeric: "auto" });

// farRelative keeps runs months ahead relative ("in 3 months") too.
function farRelative(date: Date, now: Date): string {
  const days = (date.getTime() - now.getTime()) / 86_400_000;
  if (days < 30) return formatRelative(date, now);
  if (days < 365) return months.format(Math.round(days / 30.44), "month");
  return months.format(Math.round(days / 365.25), "year");
}

/**
 * NextRunsList shows upcoming run times as wall-clock times in the
 * schedule's zone, with their UTC offset; hovering shows the viewer's own
 * time. An offset that differs from the first run's is highlighted.
 */
export function NextRunsList({
  runs,
  timezone,
  className,
  dim,
}: {
  runs: Date[];
  timezone: string;
  className?: string;
  /** dim marks the list as stale while a new preview loads. */
  dim?: boolean;
}) {
  const now = useNow(30_000);
  const first = runs[0] ? tzOffset(timezone, runs[0]) : "";
  return (
    <ol
      className={cn("flex flex-col", dim && "opacity-60 transition-opacity", className)}
      aria-label="Next runs"
    >
      {runs.map((r) => {
        const offset = tzOffset(timezone, r);
        const shifted = offset !== first;
        return (
          <li
            key={r.getTime()}
            className="flex items-baseline gap-3 border-b border-dashed py-1.5 text-[13px] last:border-0"
            title={`Your time: ${formatDateTime(r)}`}
          >
            <time
              dateTime={r.toISOString()}
              className="tabular min-w-0 flex-1 truncate font-medium"
            >
              {formatInZone(r, timezone, r.getFullYear() !== now.getFullYear())}
            </time>
            <span
              className={cn(
                "tabular shrink-0 rounded px-1 font-mono text-[11.5px]",
                shifted ? "bg-warning-soft text-warning" : "text-muted-foreground",
              )}
            >
              UTC{offset}
            </span>
            <span className="tabular w-24 shrink-0 text-right text-xs text-muted-foreground">
              {farRelative(r, now)}
            </span>
          </li>
        );
      })}
    </ol>
  );
}
