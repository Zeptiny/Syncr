import { cn } from "@/lib/utils";

/** KeyValueItem is one row of a KeyValueList. */
export interface KeyValueItem {
  label: React.ReactNode;
  value: React.ReactNode;
  /** mono sets the value in the monospace face (IDs, paths). */
  mono?: boolean;
}

/** KeyValueList is a two-column description list. */
export function KeyValueList({ items, className }: { items: KeyValueItem[]; className?: string }) {
  return (
    <dl
      className={cn(
        "grid grid-cols-1 gap-x-6 gap-y-1 sm:grid-cols-[minmax(9rem,auto)_1fr] sm:gap-y-3",
        className,
      )}
    >
      {items.map((it, i) => (
        <div key={i} className="contents">
          <dt className="pt-2 text-[13px] text-muted-foreground sm:pt-0">{it.label}</dt>
          <dd className={cn("min-w-0 break-words", it.mono && "font-mono text-[13px]")}>
            {it.value}
          </dd>
        </div>
      ))}
    </dl>
  );
}
