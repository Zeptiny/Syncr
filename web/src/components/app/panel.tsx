import { cn } from "@/lib/utils";

/** PanelProps configures Panel. */
export interface PanelProps extends Omit<React.ComponentProps<"section">, "title"> {
  title?: React.ReactNode;
  description?: React.ReactNode;
  actions?: React.ReactNode;
  /** flush removes the body padding (tables, lists). */
  flush?: boolean;
}

/** Panel is the working surface: an outlined area with an optional header. */
export function Panel({
  title,
  description,
  actions,
  flush,
  className,
  children,
  ...props
}: PanelProps) {
  return (
    <section className={cn("rounded-lg border bg-card text-card-foreground", className)} {...props}>
      {(title ?? actions) && (
        <header className="flex flex-wrap items-start justify-between gap-3 border-b px-5 py-3.5">
          <div className="min-w-0">
            {title && <h2 className="text-[15px] leading-6 font-semibold">{title}</h2>}
            {description && (
              <p className="mt-0.5 max-w-prose text-[13px] text-muted-foreground">{description}</p>
            )}
          </div>
          {actions && <div className="flex shrink-0 items-center gap-2">{actions}</div>}
        </header>
      )}
      <div className={cn(!flush && "p-5")}>{children}</div>
    </section>
  );
}
