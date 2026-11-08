import { useNow } from "@/hooks/use-now";
import { formatDateTime, formatRelative } from "@/lib/format";
import { cn } from "@/lib/utils";

/** RelativeTime renders "5 minutes ago", with the exact time on hover. */
export function RelativeTime({
  date,
  empty = "never",
  className,
}: {
  date: Date | undefined;
  empty?: string;
  className?: string;
}) {
  const now = useNow();
  if (!date) return <span className={cn("text-muted-foreground", className)}>{empty}</span>;
  return (
    <time dateTime={date.toISOString()} title={formatDateTime(date)} className={className}>
      {formatRelative(date, now)}
    </time>
  );
}
