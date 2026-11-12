import { ChevronRight } from "lucide-react";
import { Fragment } from "react";

import { pathCrumbs, rootPath, type PathKind } from "@/lib/paths";
import { cn } from "@/lib/utils";

/** PathBreadcrumbsProps configures PathBreadcrumbs. */
export interface PathBreadcrumbsProps {
  path: string;
  kind: PathKind;
  /** rootLabel names the top: a remote's name, "Snapshot", "Allowed folders". */
  rootLabel: string;
  onNavigate: (path: string) => void;
  /**
   * driveRoot shows the first step of a snapshot made on Windows as a
   * drive ("C:") instead of a folder named "C".
   */
  driveRoot?: boolean;
  /** canOpen says whether a step may be opened (default: all). */
  canOpen?: (path: string) => boolean;
  className?: string;
}

/** PathBreadcrumbs shows where a browser is, each step a link back to it. */
export function PathBreadcrumbs({
  path,
  kind,
  rootLabel,
  onNavigate,
  driveRoot,
  canOpen,
  className,
}: PathBreadcrumbsProps) {
  const crumbs = pathCrumbs(path, kind);
  const step = (label: string, to: string, last: boolean) => {
    const cls = "max-w-48 truncate rounded px-1 py-0.5 font-mono text-[12.5px]";
    if (last)
      return (
        <span aria-current="page" className={cn(cls, "font-semibold")}>
          {label}
        </span>
      );
    if (canOpen && !canOpen(to))
      return <span className={cn(cls, "text-muted-foreground")}>{label}</span>;
    return (
      <button
        type="button"
        onClick={() => onNavigate(to)}
        className={cn(cls, "text-muted-foreground hover:bg-accent hover:text-foreground")}
      >
        {label}
      </button>
    );
  };
  return (
    <nav
      aria-label="Breadcrumbs"
      className={cn("flex min-w-0 flex-wrap items-center gap-0.5", className)}
    >
      {step(rootLabel, rootPath(kind), crumbs.length === 0)}
      {crumbs.map((c, i) => (
        <Fragment key={c.path}>
          <ChevronRight className="size-3 shrink-0 text-muted-foreground" aria-hidden />
          {step(
            driveRoot && i === 0 && /^[A-Za-z]$/.test(c.label) ? `${c.label}:` : c.label,
            c.path,
            i === crumbs.length - 1,
          )}
        </Fragment>
      ))}
    </nav>
  );
}
