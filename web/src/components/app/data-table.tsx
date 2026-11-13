import {
  flexRender,
  getCoreRowModel,
  useReactTable,
  type ColumnDef,
  type RowData,
} from "@tanstack/react-table";
import { ChevronLeft, ChevronRight } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { cn } from "@/lib/utils";

import { ErrorState } from "./error-state";

declare module "@tanstack/react-table" {
  // eslint-disable-next-line @typescript-eslint/no-unused-vars
  interface ColumnMeta<TData extends RowData, TValue> {
    /** className applies to the column's header and cells. */
    className?: string;
  }
}

/** Pager is the keyset paging control of a DataTable. */
export interface Pager {
  page: number;
  canPrev: boolean;
  canNext: boolean;
  onPrev: () => void;
  onNext: () => void;
}

/** DataTableProps configures DataTable. */
export interface DataTableProps<T> {
  // ColumnDef's value type varies per column.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  columns: ColumnDef<T, any>[];
  data: T[] | undefined;
  isLoading?: boolean;
  error?: unknown;
  onRetry?: () => void;
  /** empty renders when the data is an empty list. */
  empty?: React.ReactNode;
  getRowId?: (row: T) => string;
  /** onRowClick makes rows act as links (Enter works too). */
  onRowClick?: (row: T) => void;
  rowLabel?: (row: T) => string;
  pager?: Pager;
  skeletonRows?: number;
  className?: string;
  "aria-label"?: string;
}

/**
 * DataTable renders rows with TanStack Table and handles the loading,
 * empty and error states and the keyset pager.
 */
export function DataTable<T>({
  columns,
  data,
  isLoading,
  error,
  onRetry,
  empty,
  getRowId,
  onRowClick,
  rowLabel,
  pager,
  skeletonRows = 6,
  className,
  ...aria
}: DataTableProps<T>) {
  const table = useReactTable({
    data: data ?? [],
    columns,
    getCoreRowModel: getCoreRowModel(),
    getRowId: getRowId ? (row) => getRowId(row) : undefined,
  });
  const colCount = table.getVisibleLeafColumns().length;
  const rows = table.getRowModel().rows;
  const showError = error !== undefined && error !== null && !isLoading;
  const showSkeleton = isLoading && !data;

  return (
    <div className={cn("overflow-hidden rounded-lg border bg-card", className)}>
      {/* relative: screen-reader-only header text (absolute) stays inside the scroll box. */}
      <div className="relative overflow-x-auto">
        <table
          className="w-full caption-bottom text-sm"
          aria-label={aria["aria-label"]}
          aria-busy={isLoading}
        >
          <thead>
            {table.getHeaderGroups().map((hg) => (
              <tr key={hg.id} className="border-b bg-muted/60">
                {hg.headers.map((h) => (
                  <th
                    key={h.id}
                    scope="col"
                    className={cn(
                      "h-9 px-4 text-left text-xs font-medium whitespace-nowrap text-muted-foreground",
                      h.column.columnDef.meta?.className,
                    )}
                  >
                    {h.isPlaceholder ? null : flexRender(h.column.columnDef.header, h.getContext())}
                  </th>
                ))}
              </tr>
            ))}
          </thead>
          <tbody>
            {showSkeleton &&
              Array.from({ length: skeletonRows }, (_, i) => (
                <tr key={`s${i}`} className="border-b last:border-0" data-testid="skeleton-row">
                  {Array.from({ length: colCount }, (_, j) => (
                    <td key={j} className="h-12 px-4">
                      <Skeleton className={cn("h-3.5", j === 0 ? "w-40" : "w-20")} />
                    </td>
                  ))}
                </tr>
              ))}
            {showError && (
              <tr>
                <td colSpan={colCount}>
                  <ErrorState error={error} onRetry={onRetry} />
                </td>
              </tr>
            )}
            {!showSkeleton && !showError && rows.length === 0 && (
              <tr>
                <td colSpan={colCount}>
                  {empty ?? (
                    <p className="p-8 text-center text-muted-foreground">Nothing here yet.</p>
                  )}
                </td>
              </tr>
            )}
            {!showSkeleton &&
              !showError &&
              rows.map((row) => (
                <tr
                  key={row.id}
                  className={cn(
                    "border-b transition-colors last:border-0",
                    onRowClick &&
                      "cursor-pointer hover:bg-accent/60 focus-visible:bg-accent/60 focus-visible:outline-none",
                  )}
                  onClick={onRowClick ? () => onRowClick(row.original) : undefined}
                  onKeyDown={
                    onRowClick
                      ? (e) => {
                          if (e.key === "Enter") onRowClick(row.original);
                        }
                      : undefined
                  }
                  tabIndex={onRowClick ? 0 : undefined}
                  aria-label={rowLabel?.(row.original)}
                >
                  {row.getVisibleCells().map((cell) => (
                    <td
                      key={cell.id}
                      className={cn(
                        "h-12 px-4 py-2 align-middle",
                        cell.column.columnDef.meta?.className,
                      )}
                    >
                      {flexRender(cell.column.columnDef.cell, cell.getContext())}
                    </td>
                  ))}
                </tr>
              ))}
          </tbody>
        </table>
      </div>
      {pager && (pager.canPrev || pager.canNext) && (
        <nav
          aria-label="Pagination"
          className="flex items-center justify-between border-t px-4 py-2 text-[13px] text-muted-foreground"
        >
          <span>Page {pager.page}</span>
          <div className="flex gap-1.5">
            <Button variant="outline" size="sm" disabled={!pager.canPrev} onClick={pager.onPrev}>
              <ChevronLeft aria-hidden />
              Newer
            </Button>
            <Button variant="outline" size="sm" disabled={!pager.canNext} onClick={pager.onNext}>
              Older
              <ChevronRight aria-hidden />
            </Button>
          </div>
        </nav>
      )}
    </div>
  );
}
