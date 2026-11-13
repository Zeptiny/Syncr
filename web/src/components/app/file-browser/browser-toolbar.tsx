import { ArrowUp, CornerDownLeft, RefreshCw } from "lucide-react";
import { useState } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { useNow } from "@/hooks/use-now";
import { formatDateTime, formatRelative } from "@/lib/format";
import { normalizePath, parentPath, rootPath, type PathKind } from "@/lib/paths";
import { cn } from "@/lib/utils";

import { PathBreadcrumbs } from "../path-breadcrumbs";

/** BrowserToolbarProps configures BrowserToolbar. */
export interface BrowserToolbarProps {
  kind: PathKind;
  /** path is the open folder. */
  path: string;
  onNavigate: (path: string) => void;
  /** rootLabel names the top in the breadcrumbs. */
  rootLabel: string;
  /** roots are a worker's allowed folders: Up from one of them lists them all. */
  roots?: string[];
  /** driveRoot shows a Windows snapshot's first step as a drive ("C:"). */
  driveRoot?: boolean;
  /** onRefresh lists the folder again (the Refresh button). */
  onRefresh?: () => void;
  isFetching?: boolean;
  /** updatedAt is when the listing arrived, in ms ("Listed 2 min ago"); 0 hides it. */
  updatedAt?: number;
  /** listedBy follows "Listed … ago": the worker, or a chooser of it. */
  listedBy?: React.ReactNode;
  /** actions are the page's buttons for the open folder, on the left. */
  actions?: React.ReactNode;
  /** extra sits on the right: the remote's usage, "Calculate sizes". */
  extra?: React.ReactNode;
  className?: string;
}

/**
 * BrowserToolbar is the top of a file browser: Up, a path box (Enter
 * jumps), Refresh, the breadcrumbs, and a line saying when and by which
 * worker the folder was listed.
 */
export function BrowserToolbar({
  kind,
  path,
  onNavigate,
  rootLabel,
  roots = [],
  driveRoot,
  onRefresh,
  isFetching,
  updatedAt,
  listedBy,
  actions,
  extra,
  className,
}: BrowserToolbarProps) {
  const top = rootPath(kind);
  const [typed, setTyped] = useState(path);
  const [shownPath, setShownPath] = useState(path);
  if (path !== shownPath) {
    setShownPath(path);
    setTyped(path);
  }
  const go = (to: string) => onNavigate(to.trim() === top ? top : normalizePath(to, kind));
  const status = !!updatedAt || !!listedBy || !!actions || !!extra;

  return (
    <div className={cn("flex flex-col gap-2 border-b px-4 py-3", className)}>
      <form
        className="flex items-center gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          e.stopPropagation();
          go(typed);
        }}
      >
        <Button
          type="button"
          variant="outline"
          size="icon"
          aria-label="Up one folder"
          disabled={path === top}
          onClick={() => onNavigate(parentPath(path, kind, roots))}
        >
          <ArrowUp aria-hidden />
        </Button>
        <Input
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
          aria-label="Path"
          // A hint, not a value: in the sans font, unlike the paths typed.
          placeholder={
            kind === "remote" ? "Type a path, e.g. photos/2026" : "Type a path, e.g. /srv/photos"
          }
          spellCheck={false}
          autoComplete="off"
          className="h-9 min-w-0 flex-1 bg-card font-mono text-[13px] placeholder:font-sans"
        />
        <Button type="submit" variant="outline" size="icon" aria-label="Go to this path">
          <CornerDownLeft aria-hidden />
        </Button>
        {onRefresh && (
          <Button
            type="button"
            variant="outline"
            size="icon"
            aria-label="Refresh"
            title="List this folder again"
            disabled={isFetching}
            onClick={onRefresh}
          >
            <RefreshCw className={cn(isFetching && "animate-spin")} aria-hidden />
          </Button>
        )}
      </form>
      <PathBreadcrumbs
        path={path}
        kind={kind}
        rootLabel={rootLabel}
        driveRoot={driveRoot}
        onNavigate={onNavigate}
      />
      {status && (
        <div className="flex flex-wrap items-center gap-x-3 gap-y-2">
          {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
          {(!!updatedAt || listedBy) && (
            <span className="flex min-w-0 flex-wrap items-center gap-x-1 text-xs text-muted-foreground sm:ml-auto">
              {updatedAt ? <ListedAgo at={updatedAt} /> : null}
              {listedBy}
            </span>
          )}
          {extra && (
            <div className={cn("flex items-center gap-2", !updatedAt && !listedBy && "sm:ml-auto")}>
              {extra}
            </div>
          )}
        </div>
      )}
    </div>
  );
}

function ListedAgo({ at }: { at: number }) {
  const now = useNow();
  const date = new Date(at);
  // A listing that just arrived reads "just now" even if the clock lags.
  const shown = formatRelative(date, now.getTime() < at ? date : now);
  return (
    <span>
      Listed{" "}
      <time dateTime={date.toISOString()} title={formatDateTime(date)}>
        {shown}
      </time>
    </span>
  );
}
