import type { LucideIcon } from "lucide-react";
import { ArrowDown, ArrowUp, FolderX, MoreHorizontal, Search } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { Checkbox } from "@/components/ui/checkbox";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
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
import { useFileSort } from "@/hooks/use-file-sort";
import { nextSort, sortEntries, type SortKey } from "@/lib/sort-entries";
import { cn } from "@/lib/utils";

import { EmptyState } from "./empty-state";
import { EntryIcon } from "./entry-icon";
import { ErrorState } from "./error-state";
import { isMissingFolder } from "./file-browser/helpers";

/** FileSelection lets rows of a FileTable be ticked. */
export interface FileSelection {
  /** selected holds the ticked entries' paths, in the order they were ticked. */
  selected: string[];
  onChange: (paths: string[]) => void;
  /** multiple allows more than one tick (default one). */
  multiple?: boolean;
  /** canSelect limits which entries can be ticked (default all). */
  canSelect?: (entry: ProbeEntry) => boolean;
}

/** RowMenuItem is one entry of a row's menu, supplied by the page. */
export interface RowMenuItem {
  /** label names the action; it must be unique within the menu. */
  label: string;
  icon?: LucideIcon;
  onSelect: () => void;
  disabled?: boolean;
  /** separated draws a line above the item. */
  separated?: boolean;
  /** destructive colours the item as one that removes something. */
  destructive?: boolean;
}

/** FileTableProps configures FileTable. */
export interface FileTableProps {
  entries: ProbeEntry[] | undefined;
  isLoading?: boolean;
  error?: unknown;
  onRetry?: () => void;
  /** onOpen opens a folder (its name becomes a button; Enter on its row). */
  onOpen?: (entry: ProbeEntry) => void;
  /** onUp goes up one folder (Backspace or Alt+Up on a row). */
  onUp?: () => void;
  selection?: FileSelection;
  /** truncated says the folder holds more entries than were listed. */
  truncated?: boolean;
  /** compact drops the modified and mode columns (dialogs). */
  compact?: boolean;
  /** pageSize is how many rows show at once (default 200). */
  pageSize?: number;
  empty?: React.ReactNode;
  /**
   * rowMenu returns the items of a row's menu (the "Actions for <name>"
   * button); a row with no items has no menu.
   */
  rowMenu?: (entry: ProbeEntry) => RowMenuItem[];
  /** folderSize is a folder's measured size, used to sort by size. */
  folderSize?: (entry: ProbeEntry) => bigint | undefined;
  /** renderFolderSize renders a folder's Size cell; the default is "–". */
  renderFolderSize?: (entry: ProbeEntry) => React.ReactNode;
  /** missingHint explains a folder that isn't there (see missingFolderHint). */
  missingHint?: string;
  /**
   * onGoToParent is offered as "Go to the nearest parent" when the folder
   * does not exist (a deleted folder, another snapshot).
   */
  onGoToParent?: () => void;
  /** autoFocus focuses the starting row once rows show (keyboard moves between folders). */
  autoFocus?: boolean;
  /** focusPath is the row to start on, e.g. the folder just left when going up. */
  focusPath?: string;
  className?: string;
  "aria-label"?: string;
}

const isDir = (e: ProbeEntry) => e.type === ProbeEntryType.DIR;
const count = (n: number) => n.toLocaleString("en-US");

/**
 * FileTable lists the entries of one folder, folders first, sortable by
 * name, size and time, with a filter box and pages of 200 rows, all done
 * in the browser. Rows take a roving focus: arrows, Home and End move,
 * Enter opens a folder, Space ticks, Shift+Space and Shift+click tick a
 * range, Backspace or Alt+Up goes up, Ctrl+A ticks every row matching the
 * filter and Escape clears the filter, then the selection. The file
 * browser, its picker dialog and the snapshot browser share it.
 */
export function FileTable({
  entries,
  isLoading,
  error,
  onRetry,
  onOpen,
  onUp,
  selection,
  truncated,
  compact,
  pageSize = 200,
  empty,
  rowMenu,
  folderSize,
  renderFolderSize,
  missingHint = "It was moved or deleted.",
  onGoToParent,
  autoFocus,
  focusPath,
  className,
  ...aria
}: FileTableProps) {
  const [filter, setFilter] = useState("");
  const [shown, setShown] = useState(pageSize);
  const [sort, setSort] = useFileSort();
  const sorted = useMemo(
    () => sortEntries(entries ?? [], sort, folderSize),
    [entries, sort, folderSize],
  );
  const needle = filter.trim().toLowerCase();
  const matching = useMemo(
    () => (needle ? sorted.filter((e) => e.name.toLowerCase().includes(needle)) : sorted),
    [sorted, needle],
  );
  const rows = matching.slice(0, shown);
  const multiple = !!selection?.multiple;
  const selectable = (e: ProbeEntry) => !!selection && (selection.canSelect?.(e) ?? true);
  const selectedList = selection?.selected;
  const selected = useMemo(() => new Set(selectedList ?? []), [selectedList]);
  const tickable = matching.filter(selectable);
  const allOn = tickable.length > 0 && tickable.every((e) => selected.has(e.path));
  const menus = !!rowMenu;
  const cols = (selection ? 1 : 0) + 2 + (compact ? 0 : 2) + (menus ? 1 : 0);

  // Roving focus: one row is the tab stop; arrows move it. Until a row is
  // focused, the tab stop is focusPath's row (or the first).
  const [active, setActive] = useState<number | null>(null);
  const rowRefs = useRef<(HTMLTableRowElement | null)[]>([]);
  const pendingFocus = useRef<number | null>(null);
  const anchor = useRef<string | undefined>(undefined);
  const started = useRef(false);
  const start = focusPath
    ? Math.max(
        rows.findIndex((e) => e.path === focusPath),
        0,
      )
    : 0;
  const at = Math.min(active ?? start, Math.max(rows.length - 1, 0));

  useEffect(() => {
    // A row asked for before it was shown (the next page).
    if (pendingFocus.current === null) return;
    const el = rowRefs.current[pendingFocus.current];
    if (el) {
      pendingFocus.current = null;
      el.focus();
    }
  });
  useEffect(() => {
    // The first rows take the focus after a keyboard move between folders.
    if (started.current || rows.length === 0) return;
    started.current = true;
    if (autoFocus) rowRefs.current[at]?.focus();
  }, [rows.length, autoFocus, at]);

  // A polite announcement of the selection's size.
  const total = selectedList?.length ?? 0;
  const [lastTotal, setLastTotal] = useState(total);
  const [announce, setAnnounce] = useState("");
  if (total !== lastTotal) {
    setLastTotal(total);
    setAnnounce(total === 0 ? "Nothing selected" : `${count(total)} selected`);
  }

  const change = (paths: string[]) => selection?.onChange(paths);
  const toggle = (e: ProbeEntry, on: boolean) => {
    if (!selection || !selectable(e)) return;
    anchor.current = e.path;
    if (!multiple) change(on ? [e.path] : []);
    else
      change(on ? [...selection.selected, e.path] : selection.selected.filter((p) => p !== e.path));
  };
  // range ticks (or unticks, like the anchor) every row between the anchor and e.
  const range = (e: ProbeEntry) => {
    if (!selection || !multiple || !selectable(e)) return toggle(e, !selected.has(e.path));
    const from = anchor.current ?? e.path;
    const order = matching.map((x) => x.path);
    let lo = order.indexOf(from);
    const hi = order.indexOf(e.path);
    if (lo < 0) lo = hi;
    const span = matching.slice(Math.min(lo, hi), Math.max(lo, hi) + 1).filter(selectable);
    const on = from === e.path ? !selected.has(e.path) : selected.has(from);
    const paths = new Set(span.map((x) => x.path));
    const rest = selection.selected.filter((p) => !paths.has(p));
    change(on ? [...rest, ...span.map((x) => x.path)] : rest);
    anchor.current = e.path;
  };
  const selectAll = (on: boolean) => {
    if (!selection || !multiple) return;
    const paths = new Set(tickable.map((e) => e.path));
    const others = selection.selected.filter((p) => !paths.has(p));
    change(on ? [...others, ...paths] : others);
    if (on) setAnnounce(`Selected all ${count(paths.size)}`);
  };

  const focusRow = (i: number) => {
    const next = Math.max(0, Math.min(i, matching.length - 1));
    setActive(next);
    const el = rowRefs.current[next];
    if (next < rows.length && el) el.focus();
    else {
      pendingFocus.current = next;
      setShown((n) => (next < n ? n : n + pageSize));
    }
  };

  const onRowKey = (ev: React.KeyboardEvent<HTMLTableRowElement>, e: ProbeEntry, i: number) => {
    const own = ev.target === ev.currentTarget;
    const mod = ev.ctrlKey || ev.metaKey;
    switch (ev.key) {
      case "ArrowDown":
        ev.preventDefault();
        return focusRow(i + 1);
      case "ArrowUp":
        ev.preventDefault();
        if (ev.altKey) return onUp?.();
        return focusRow(i - 1);
      case "Home":
        if (!own) return;
        ev.preventDefault();
        return focusRow(0);
      case "End":
        if (!own) return;
        ev.preventDefault();
        return focusRow(rows.length - 1);
      case "Enter":
        if (!own) return;
        ev.preventDefault();
        if (isDir(e) && onOpen) onOpen(e);
        return;
      case " ":
        if (!own) return;
        ev.preventDefault();
        if (ev.shiftKey) return range(e);
        return toggle(e, !selected.has(e.path));
      case "Backspace":
        if (!own || !onUp) return;
        ev.preventDefault();
        return onUp();
      case "a":
      case "A":
        if (!mod || !own || !multiple) return;
        ev.preventDefault();
        return selectAll(true);
      case "Escape":
        if (!own) return;
        if (filter) {
          ev.preventDefault();
          ev.stopPropagation();
          setFilter("");
          setShown(pageSize);
        } else if (selection && selection.selected.length > 0) {
          ev.preventDefault();
          ev.stopPropagation();
          change([]);
        }
        return;
    }
  };

  if (error !== undefined && error !== null) {
    if (isMissingFolder(error))
      return (
        <div role="alert" className="flex flex-col items-center px-6 py-12 text-center">
          <div className="mb-4 grid size-11 place-items-center rounded-xl bg-warning-soft text-warning">
            <FolderX className="size-5" aria-hidden />
          </div>
          <h3 className="text-[15px] font-semibold">This folder isn't here</h3>
          <p className="mt-1 max-w-[52ch] text-[13px] leading-5 text-muted-foreground">
            {missingHint}
          </p>
          {onGoToParent && (
            <Button variant="outline" size="sm" className="mt-5" onClick={onGoToParent}>
              Go to the nearest parent
            </Button>
          )}
        </div>
      );
    return <ErrorState error={error} title="Couldn't list this folder" onRetry={onRetry} />;
  }

  const allNote =
    multiple && allOn && (matching.length > rows.length || truncated)
      ? truncated && !needle
        ? `Selected the first ${count(tickable.length)} entries, all that could be listed.`
        : `Selected all ${count(tickable.length)}${needle ? " matching the filter" : ""}, not only the ${count(rows.length)} shown.`
      : undefined;

  const sortHead = (key: SortKey, label: string, cls?: string, right?: boolean) => {
    const on = sort.key === key;
    const Icon = sort.desc ? ArrowDown : ArrowUp;
    return (
      <TableHead
        className={cls}
        aria-sort={on ? (sort.desc ? "descending" : "ascending") : undefined}
      >
        <button
          type="button"
          onClick={() => setSort(nextSort(sort, key))}
          className={cn(
            "-mx-1 inline-flex items-center gap-1 rounded px-1 py-0.5 hover:text-foreground focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none",
            right && "flex-row-reverse",
            on && "text-foreground",
          )}
        >
          {label}
          <Icon className={cn("size-3", !on && "invisible")} aria-hidden />
        </button>
      </TableHead>
    );
  };

  return (
    <div className={cn("flex min-h-0 flex-col", className)}>
      {truncated && (
        <p role="status" className="border-b bg-warning-soft px-4 py-2 text-[13px] text-warning">
          This folder holds more entries than can be listed. Only the first{" "}
          {count(entries?.length ?? 0)} are shown.
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
            onKeyDown={(e) => {
              if (e.key === "Escape" && filter) {
                e.preventDefault();
                e.stopPropagation();
                setFilter("");
                setShown(pageSize);
              } else if (e.key === "ArrowDown" && rows.length > 0) {
                e.preventDefault();
                focusRow(at);
              }
            }}
            placeholder="Filter this folder"
            aria-label="Filter this folder"
            className="h-8 bg-card pl-8 text-[13px]"
          />
        </div>
        {entries && (
          <span className="tabular ml-auto text-xs whitespace-nowrap text-muted-foreground">
            {needle
              ? `${count(matching.length)} of ${count(entries.length)}`
              : `${count(entries.length)} ${entries.length === 1 ? "item" : "items"}`}
          </span>
        )}
      </div>
      {allNote && <p className="border-b bg-muted/60 px-4 py-1.5 text-[13px]">{allNote}</p>}
      <p aria-live="polite" className="sr-only">
        {announce}
      </p>
      <div className="min-h-0 flex-1 overflow-auto">
        <Table
          role="grid"
          aria-label={aria["aria-label"] ?? "Files"}
          aria-multiselectable={multiple || undefined}
        >
          <TableHeader>
            <TableRow>
              {selection && (
                <TableHead className="w-9 pr-0">
                  {multiple && (
                    <Checkbox
                      aria-label="Select all"
                      checked={allOn}
                      disabled={tickable.length === 0}
                      onCheckedChange={() => selectAll(!allOn)}
                    />
                  )}
                </TableHead>
              )}
              {sortHead("name", "Name")}
              {sortHead("size", "Size", "w-24 text-right", true)}
              {!compact && (
                <>
                  {sortHead("modified", "Modified", "hidden w-44 md:table-cell")}
                  <TableHead className="hidden w-28 lg:table-cell">Mode</TableHead>
                </>
              )}
              {menus && (
                <TableHead className="w-10">
                  <span className="sr-only">Actions</span>
                </TableHead>
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
              rows.map((e, i) => {
                const on = selected.has(e.path);
                const tab = i === at ? 0 : -1;
                const items = rowMenu?.(e) ?? [];
                return (
                  <TableRow
                    key={e.path}
                    ref={(el) => {
                      rowRefs.current[i] = el;
                    }}
                    tabIndex={tab}
                    aria-selected={selection ? on : undefined}
                    data-state={on ? "selected" : undefined}
                    onFocus={() => setActive(i)}
                    onKeyDown={(ev) => onRowKey(ev, e, i)}
                    onClick={(ev) => {
                      if (ev.shiftKey && ev.target === ev.currentTarget) range(e);
                    }}
                    className="outline-none focus-visible:bg-accent/60 focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-inset"
                  >
                    {selection && (
                      <TableCell className="w-9 pr-0">
                        {selectable(e) && (
                          <Checkbox
                            tabIndex={tab}
                            aria-label={`Select ${e.name}`}
                            checked={on}
                            onClick={(ev) => {
                              if (ev.shiftKey && multiple) {
                                ev.preventDefault();
                                range(e);
                              }
                            }}
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
                            tabIndex={tab}
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
                      {isDir(e) ? (renderFolderSize?.(e) ?? "–") : formatBytes(e.size)}
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
                    {menus && (
                      <TableCell className="w-10 py-1 pr-2 text-right">
                        {items.length > 0 && <RowMenu name={e.name} items={items} tab={tab} />}
                      </TableCell>
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
            Showing {count(rows.length)} of {count(matching.length)}
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

function RowMenu({ name, items, tab }: { name: string; items: RowMenuItem[]; tab: number }) {
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          variant="ghost"
          size="icon-sm"
          tabIndex={tab}
          aria-label={`Actions for ${name}`}
          // The row's own keys (Space, Enter) must not fire on the button.
          onKeyDown={(e) => e.stopPropagation()}
        >
          <MoreHorizontal aria-hidden />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end">
        {items.map((it) => (
          <RowMenuEntry key={it.label} item={it} />
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}

function RowMenuEntry({ item }: { item: RowMenuItem }) {
  const Icon = item.icon;
  return (
    <>
      {item.separated && <DropdownMenuSeparator />}
      <DropdownMenuItem
        disabled={item.disabled}
        variant={item.destructive ? "destructive" : undefined}
        onSelect={item.onSelect}
      >
        {Icon && <Icon aria-hidden />}
        {item.label}
      </DropdownMenuItem>
    </>
  );
}
