import { formatBytes, percent } from "@/lib/format";
import { cn } from "@/lib/utils";

/** UsageBar shows how much of a storage's space is used. */
export function UsageBar({
  used,
  total,
  label = "Space used",
  className,
}: {
  used: number;
  total: number;
  label?: string;
  className?: string;
}) {
  const pct = percent(used, total) ?? 0;
  const tone = pct >= 95 ? "bg-danger" : pct >= 80 ? "bg-warning" : "bg-primary";
  return (
    <div className={cn("flex flex-col gap-1", className)}>
      <div className="flex items-baseline justify-between gap-3 text-xs">
        <span className="text-muted-foreground">{label}</span>
        <span className="tabular">
          {formatBytes(used)} of {formatBytes(total)}
          <span className="text-muted-foreground"> ({Math.round(pct)}%)</span>
        </span>
      </div>
      <div
        role="progressbar"
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={Math.round(pct)}
        className="h-1.5 w-full overflow-hidden rounded-full bg-muted"
      >
        <span className={cn("block h-full rounded-full", tone)} style={{ width: `${pct}%` }} />
      </div>
    </div>
  );
}
