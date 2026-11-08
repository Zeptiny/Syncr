import { Link, type LinkProps } from "@tanstack/react-router";
import { ChevronLeft } from "lucide-react";

import { cn } from "@/lib/utils";

/** PageHeaderProps configures PageHeader. */
export interface PageHeaderProps {
  title: React.ReactNode;
  description?: React.ReactNode;
  /** actions sit on the right (primary action last). */
  actions?: React.ReactNode;
  /** back links to the parent page. */
  back?: { to: LinkProps["to"]; label: string };
  /** meta renders under the title, e.g. a status badge and IDs. */
  meta?: React.ReactNode;
  className?: string;
}

/** PageHeader is the title row at the top of every page. */
export function PageHeader({
  title,
  description,
  actions,
  back,
  meta,
  className,
}: PageHeaderProps) {
  return (
    <div
      className={cn(
        "mb-6 flex flex-col gap-4 sm:flex-row sm:items-end sm:justify-between",
        className,
      )}
    >
      <div className="min-w-0">
        {back && (
          <Link
            to={back.to}
            className="mb-2 inline-flex items-center gap-1 text-[13px] text-muted-foreground hover:text-foreground"
          >
            <ChevronLeft className="size-3.5" aria-hidden />
            {back.label}
          </Link>
        )}
        <h1 className="text-[22px] leading-8 font-semibold tracking-[-0.01em] break-words">
          {title}
        </h1>
        {meta && <div className="mt-1.5 flex flex-wrap items-center gap-x-3 gap-y-1.5">{meta}</div>}
        {description && (
          <p className="mt-1 max-w-[68ch] text-sm text-muted-foreground">{description}</p>
        )}
      </div>
      {actions && <div className="flex shrink-0 flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}
