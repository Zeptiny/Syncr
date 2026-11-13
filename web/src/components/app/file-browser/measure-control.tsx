import { Loader2, Ruler, TriangleAlert } from "lucide-react";

import { Button } from "@/components/ui/button";
import type { FolderMeasure } from "@/hooks/use-folder-measure";
import { formatBytes, formatCount, formatDateTime } from "@/lib/format";
import { scanErrorText } from "@/lib/scans";
import { cn } from "@/lib/utils";

/** MeasureControlProps configures MeasureControl. */
export interface MeasureControlProps {
  measure: FolderMeasure;
  /** measuredAgo is when the open folder was measured, as text ("3 minutes ago"). */
  measuredAgo?: string;
  className?: string;
}

const plural = (n: bigint, one: string, many: string) =>
  `${formatCount(n)} ${n === 1n ? one : many}`;

/**
 * MeasureControl is the toolbar's "Calculate sizes": the button, then the
 * open folder's total and its age, and what limits it: a walk stopped at
 * the time limit ("at least …"), entries that could not be read, child
 * folders left out, a worker busy with another scan.
 */
export function MeasureControl({ measure, measuredAgo, className }: MeasureControlProps) {
  if (!measure.canMeasure) return null;
  const r = measure.result;
  const m = r?.measurement;
  const atLeast = !!m && (!m.complete || m.unreadable > 0n);
  const notes: string[] = [];
  if (m && !m.complete) notes.push(`stopped after ${Math.round(r.elapsed)} s`);
  if (m && m.unreadable > 0n)
    notes.push(`${plural(m.unreadable, "entry", "entries")} could not be read`);
  if (m?.childrenTruncated)
    notes.push(`only the largest ${formatCount(m.children.length)} folders have sizes`);

  return (
    <div className={cn("flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1", className)}>
      <Button
        type="button"
        variant="outline"
        size="sm"
        disabled={measure.isMeasuring}
        onClick={measure.measure}
        title="Measure every folder here in one pass; it can take up to a minute"
      >
        {measure.isMeasuring ? (
          <Loader2 className="animate-spin" aria-hidden />
        ) : (
          <Ruler aria-hidden />
        )}
        {measure.isMeasuring ? "Measuring…" : r ? "Measure again" : "Calculate sizes"}
      </Button>
      <span role="status" className="min-w-0 text-xs text-muted-foreground">
        {measure.isMeasuring ? (
          "This can take up to a minute."
        ) : m ? (
          <>
            <span className="tabular font-medium text-foreground">
              {atLeast ? "At least " : ""}
              {formatBytes(m.bytes)}
            </span>{" "}
            in {plural(m.files, "file", "files")}
            {measuredAgo && (
              <>
                , measured{" "}
                <time
                  dateTime={new Date(r.measuredAt).toISOString()}
                  title={formatDateTime(new Date(r.measuredAt))}
                >
                  {measuredAgo}
                </time>
              </>
            )}
            {notes.length > 0 && (
              <span className="text-warning">
                {". "}
                <TriangleAlert className="inline size-3.5 align-[-2px]" aria-hidden />{" "}
                {notes.join("; ").replace(/^./, (c) => c.toUpperCase())}.
              </span>
            )}
          </>
        ) : null}
      </span>
      {measure.error !== undefined && (
        <span role="alert" className="text-xs text-danger">
          {scanErrorText(measure.error)}
        </span>
      )}
    </div>
  );
}
