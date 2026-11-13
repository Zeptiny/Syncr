import { formatBytes, formatCount, toNumber } from "@/lib/format";
import { cn } from "@/lib/utils";

/** SizeCellProps configures SizeCell. */
export interface SizeCellProps {
  /** bytes, files and dirs are the folder's measured totals. */
  bytes: bigint;
  files: bigint;
  dirs: bigint;
  /** total is the measured open folder's bytes: the share bar's whole (none: no bar). */
  total?: bigint;
  /** atLeast: the measurement stopped early, so the figures are lower bounds. */
  atLeast?: boolean;
  /** measured says when, for the hover text: "measured 3 min ago". */
  measured?: string;
  className?: string;
}

/** share is part of whole in percent, rounded for display ("<1" for small, non-zero parts). */
function share(part: bigint, whole: bigint): { pct: number; text: string } {
  const w = toNumber(whole);
  const p = toNumber(part);
  if (w <= 0 || p <= 0) return { pct: 0, text: "0" };
  const pct = Math.min(100, (p / w) * 100);
  return { pct, text: pct < 1 ? "<1" : String(Math.round(pct)) };
}

/**
 * SizeCell is a measured folder's Size cell: its total and a bar of its
 * share of the open folder. The file and folder counts and the age of the
 * measurement are in the hover text and, for screen readers, in the cell.
 */
export function SizeCell({
  bytes,
  files,
  dirs,
  total,
  atLeast,
  measured,
  className,
}: SizeCellProps) {
  const s = total === undefined ? undefined : share(bytes, total);
  const counts = `${formatCount(files)} ${files === 1n ? "file" : "files"}, ${formatCount(dirs)} ${dirs === 1n ? "folder" : "folders"}`;
  const part = s ? `, ${s.text}% of this folder` : "";
  const detail = `${atLeast ? "at least " : ""}${counts}${part}`;
  return (
    <span
      className={cn("inline-flex flex-col items-end gap-1", className)}
      title={measured ? `${detail}; ${measured}` : detail}
    >
      <span className="text-foreground">
        {atLeast && <span aria-hidden>≥ </span>}
        {atLeast && <span className="sr-only">at least </span>}
        {formatBytes(bytes)}
      </span>
      {s && (
        <span aria-hidden className="block h-1 w-16 overflow-hidden rounded-full bg-muted">
          <span
            className={cn("block h-full rounded-full", atLeast ? "bg-warning" : "bg-primary")}
            style={{ width: `${s.pct}%` }}
          />
        </span>
      )}
      <span className="sr-only">
        , {counts}
        {part}
      </span>
    </span>
  );
}
