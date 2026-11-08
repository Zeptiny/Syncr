import type { LucideIcon } from "lucide-react";

import { cn } from "@/lib/utils";

/** EmptyStateProps configures EmptyState. */
export interface EmptyStateProps {
  icon?: LucideIcon;
  title: React.ReactNode;
  description?: React.ReactNode;
  action?: React.ReactNode;
  className?: string;
}

/** EmptyState says what is missing and offers the action that fills it. */
export function EmptyState({ icon: Icon, title, description, action, className }: EmptyStateProps) {
  return (
    <div className={cn("flex flex-col items-center px-6 py-12 text-center", className)}>
      {Icon && (
        <div className="mb-4 grid size-11 place-items-center rounded-xl border bg-muted text-muted-foreground">
          <Icon className="size-5" aria-hidden />
        </div>
      )}
      <h3 className="text-[15px] font-semibold">{title}</h3>
      {description && (
        <p className="mt-1 max-w-[46ch] text-[13px] leading-5 text-muted-foreground">
          {description}
        </p>
      )}
      {action && <div className="mt-5 flex flex-wrap justify-center gap-2">{action}</div>}
    </div>
  );
}
