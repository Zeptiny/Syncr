import { Search } from "lucide-react";
import { useMemo, useState } from "react";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import { Input } from "@/components/ui/input";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { ProbeEntryType, type ProbeEntry } from "@/gen/syncr/v1/link_pb";
import { formatBytes, formatDateTime, tsDate } from "@/lib/format";
import { cn } from "@/lib/utils";

import { EmptyState } from "./empty-state";
import { EntryIcon } from "./entry-icon";
import { ErrorState } from "./error-state";

/** FileSelection lets rows of a FileTable be ticked. */
export interface FileSelection {
  /** selected holds the ticked entries' paths. */
  selected: string[];
  onChange: (paths: string[]) => void;
  /** multiple allows more than one tick (default one). */
  multiple?: boolean;
  /** canSelect limits which entries can be ticked (default all). */
  canSelect?: (entry: ProbeEntry) => boolean;
}

/** FileTableProps configures FileTable. */
export interface FileTableProps {
  entries: ProbeEntry[] | undefined;
  isLoading?: boolean;
  error?: unknown;
  onRetry?: () => void;
  /** onOpen opens a folder (its name becomes a button). */
  onOpen?: (entry: ProbeEntry) => void;
  selection?: FileSelection;
  /** truncated says the folder holds more entries than were listed. */
  truncated?: boolean;
  /** compact drops the modified and mode columns (dialogs). */
  compact?: boolean;
  /** pageSize is how many rows show at once (default 200). */
  pageSize?: number;
  empty?: React.ReactNode;
  className?: string;
  "aria-label"?: string;
}

const isDir = (e: ProbeEntry) => e.type === ProbeEntryType.DIR;

/** sortEntries puts folders first, then sorts by name. */
function sortEntries(entries: ProbeEntry[]): ProbeEntry[] {
  return [...entries].sort(
    (a, b) =>
      Number(isDir(b)) - Number(isDir(a)) ||
      a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: "base" }),
  );
}

/**
 * FileTable lists the entries of one folder, folders first, with a filter
 * box and pages of 200 rows, both done in the browser. The path browser
 * dialog and the snapshot browser share it.
 */
export function FileTable({
  entries,
  isLoading,
  error,
  onRetry,
  onOpen,
  selection,
  truncated,
  compact,
  pageSize = 200,
  empty,
  className,
  ...aria
}: FileTableProps) {
  const [filter, setFilter] = useState("");
  const [shown, setShown] = useState(pageSize);
  const sorted = useMemo(() => sortEntries(entries ?? []), [entries]);
  const needle = filter.trim().toLowerCase();
  const matching = useMemo(
    () => (needle ? sorted.filter((e) => e.name.toLowerCase().includes(needle)) : sorted),
    [sorted, needle],
  );
  const rows = matching.slice(0, shown);
  const selectable = (e: ProbeEntry) => !!selection && (selection.canSelect?.(e) ?? true);
  const selected = new Set(selection?.selected ?? []);
  const toggle = (e: ProbeEntry, on: boolean) => {
    if (!selection) return;
    if (!selection.multiple) selection.onChange(on ? [e.path] : []);
    else
      selection.onChange(
        on ? [...selection.selected, e.path] : selection.selected.filter((p) => p !== e.path),
      );
  };
  const tickable = rows.filter(selectable);
  const allOn = tickable.length > 0 && tickable.every((e) => selected.has(e.path));
  const cols = (selection ? 1 : 0) + 2 + (compact ? 0 : 2);

  if (error !== undefined && error !== null)
    return <ErrorState error={error} title="Couldn't list this folder" onRetry={onRetry} />;

  return (
    <div className={cn("flex min-h-0 flex-col", className)}>
      {truncated && (
        <p role="status" className="border-b bg-warning-soft px-4 py-2 text-[13px] text-warning">
          This folder holds more entries than can be listed. Only the first{" "}
          {(entries?.length ?? 0).toLocaleString("en-US")} are shown.
        </p>
      )}
      <div className="flex items-center gap-2 border-b px-3 py-2">
        <div className="relative min-w-0 flex-1 sm:max-w-64">
          <Search
            className="pointer-events-none absolute top-1/2 left-2.5 size-3.5 -translate-y-1/2 text-muted-foreground"
            aria-hidden
          />
          <Input
            value={filter}
            onChange={(e) => {
              setFilter(e.target.value);
              setShown(pageSize);
            }}
            placeholder="Filter this folder"
            aria-label="Filter this folder"
            className="h-8 bg-card pl-8 text-[13px]"
          />
        </div>
        {entries && (
          <span className="tabular ml-auto text-xs whitespace-nowrap text-muted-foreground">
            {needle
              ? `${matching.length.toLocaleString("en-US")} of ${entries.length.toLocaleString("en-US")}`
              : `${entries.length.toLocaleString("en-US")} ${entries.length === 1 ? "item" : "items"}`}
          </span>
        )}
      </div>
      <div className="min-h-0 flex-1 overflow-auto">
        <Table aria-label={aria["aria-label"] ?? "Files"}>
          <TableHeader>
            <TableRow>
              {selection && (
                <TableHead className="w-9 pr-0">
                  {selection.multiple && (
                    <Checkbox
                      aria-label="Select all shown"
                      checked={allOn}
                      disabled={tickable.length === 0}
                      onCheckedChange={(c) => {
                        const paths = new Set(tickable.map((e) => e.path));
                        const others = selection.selected.filter((p) => !paths.has(p));
                        selection.onChange(c ? [...others, ...paths] : others);
                      }}
                    />
                  )}
                </TableHead>
              )}
              <TableHead>Name</TableHead>
              <TableHead className="w-24 text-right">Size</TableHead>
              {!compact && (
                <>
                  <TableHead className="hidden w-44 md:table-cell">Modified</TableHead>
                  <TableHead className="hidden w-28 lg:table-cell">Mode</TableHead>
                </>
              )}
            </TableRow>
          </TableHeader>
          <TableBody>
            {isLoading || !entries ? (
              Array.from({ length: 6 }, (_, i) => (
                <TableRow key={i} data-testid="file-skeleton">
                  <TableCell colSpan={cols}>
                    <Skeleton className="h-5 w-full" />
                  </TableCell>
                </TableRow>
              ))
            ) : rows.length === 0 ? (
              <TableRow className="hover:bg-transparent">
                <TableCell colSpan={cols}>
                  {needle ? (
                    <EmptyState title="Nothing here matches the filter" className="py-8" />
                  ) : (
                    (empty ?? <EmptyState title="This folder is empty" className="py-8" />)
                  )}
                </TableCell>
              </TableRow>
            ) : (
              rows.map((e) => {
                const on = selected.has(e.path);
                return (
                  <TableRow key={e.path} data-state={on ? "selected" : undefined}>
                    {selection && (
                      <TableCell className="w-9 pr-0">
                        {selectable(e) && (
                          <Checkbox
                            aria-label={`Select ${e.name}`}
                            checked={on}
                            onCheckedChange={(c) => toggle(e, c === true)}
                          />
                        )}
                      </TableCell>
                    )}
                    <TableCell className="max-w-0">
                      <span className="flex min-w-0 items-center gap-2">
                        <EntryIcon type={e.type} />
                        {isDir(e) && onOpen ? (
                          <button
                            type="button"
                            onClick={() => onOpen(e)}
                            className="truncate rounded text-left text-[13px] font-medium hover:underline focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
                            title={e.name}
                          >
                            {e.name}
                          </button>
                        ) : (
                          <span className="truncate text-[13px]" title={e.name}>
                            {e.name}
                          </span>
                        )}
                      </span>
                    </TableCell>
                    <TableCell className="tabular text-right text-[13px] whitespace-nowrap text-muted-foreground">
                      {isDir(e) ? "–" : formatBytes(e.size)}
                    </TableCell>
                    {!compact && (
                      <>
                        <TableCell className="hidden text-[13px] whitespace-nowrap text-muted-foreground md:table-cell">
                          {e.modTime ? formatDateTime(tsDate(e.modTime)) : "–"}
                        </TableCell>
                        <TableCell className="hidden font-mono text-xs text-muted-foreground lg:table-cell">
                          {e.mode || "–"}
                        </TableCell>
                      </>
                    )}
                  </TableRow>
                );
              })
            )}
          </TableBody>
        </Table>
      </div>
      {matching.length > shown && (
        <div className="flex items-center justify-between gap-3 border-t px-4 py-2 text-[13px] text-muted-foreground">
          <span className="tabular">
            Showing {rows.length.toLocaleString("en-US")} of{" "}
            {matching.length.toLocaleString("en-US")}
          </span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            onClick={() => setShown((n) => n + pageSize)}
          >
            Show {Math.min(pageSize, matching.length - shown)} more
          </Button>
        </div>
      )}
    </div>
  );
}
