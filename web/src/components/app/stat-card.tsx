import { cn } from "@/lib/utils";

/** StatCardProps configures StatCard. */
export interface StatCardProps {
  label: React.ReactNode;
  value: React.ReactNode;
  /** sub is a secondary line, e.g. "of 12.4 GiB". */
  sub?: React.ReactNode;
  tone?: "default" | "live" | "danger";
  className?: string;
}

/** StatCard is one figure in a StatGrid. */
export function StatCard({ label, value, sub, tone = "default", className }: StatCardProps) {
  return (
    <div className={cn("min-w-0 bg-card px-4 py-3.5", className)}>
      <div className="text-[13px] text-muted-foreground">{label}</div>
      <div
        className={cn(
          "tabular mt-1 truncate text-xl leading-7 font-semibold tracking-[-0.01em]",
          tone === "live" && "text-live",
          tone === "danger" && "text-danger",
        )}
      >
        {value}
      </div>
      {sub && <div className="tabular mt-0.5 truncate text-xs text-muted-foreground">{sub}</div>}
    </div>
  );
}

/** StatGrid lays StatCards out as one outlined band with hairline dividers. */
export function StatGrid({
  className,
  children,
}: {
  className?: string;
  children: React.ReactNode;
}) {
  return (
    <div
      className={cn(
        "grid grid-cols-2 gap-px overflow-hidden rounded-lg border bg-border md:grid-cols-5",
        // An odd last figure spans the row on two columns.
        "[&>*:last-child:nth-child(odd)]:col-span-2 md:[&>*:last-child:nth-child(odd)]:col-span-1",
        className,
      )}
    >
      {children}
    </div>
  );
}
