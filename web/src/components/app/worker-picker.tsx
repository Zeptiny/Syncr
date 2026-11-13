import type { Worker } from "@/gen/syncr/v1/api_pb";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { workerStatusMeta } from "@/lib/status";
import { toneDot } from "@/lib/tones";
import { sortWorkers } from "@/lib/workers";
import { cn } from "@/lib/utils";

const AUTO = "__auto__";

/**
 * WorkerPicker selects a worker, online ones first. With autoLabel it also
 * offers "let Syncr choose", which is the empty value. unavailable names
 * why a worker cannot do the job ("outdated"): it is shown, with the
 * reason, but cannot be chosen.
 */
export function WorkerPicker({
  id,
  workers,
  value,
  onChange,
  invalid,
  className,
  autoLabel,
  disabled,
  unavailable,
  "aria-label": ariaLabel,
}: {
  id?: string;
  workers: Worker[];
  value: string;
  onChange: (id: string) => void;
  invalid?: boolean;
  className?: string;
  /** autoLabel adds a first choice with the empty value, e.g. "Automatic". */
  autoLabel?: string;
  disabled?: boolean;
  /** unavailable returns why a worker cannot be chosen, or undefined. */
  unavailable?: (w: Worker) => string | undefined;
  "aria-label"?: string;
}) {
  return (
    <Select
      value={value || (autoLabel ? AUTO : undefined)}
      onValueChange={(v) => onChange(v === AUTO ? "" : v)}
      disabled={disabled}
    >
      <SelectTrigger
        id={id}
        aria-label={ariaLabel}
        aria-invalid={invalid || undefined}
        className={cn("w-full", className)}
      >
        <SelectValue placeholder="Choose a worker" />
      </SelectTrigger>
      <SelectContent>
        {autoLabel && <SelectItem value={AUTO}>{autoLabel}</SelectItem>}
        {sortWorkers(workers).map((w) => {
          const meta = workerStatusMeta(w.status);
          const why = unavailable?.(w);
          return (
            <SelectItem key={w.id} value={w.id} disabled={why !== undefined}>
              <span
                className={cn("size-2 shrink-0 rounded-full", toneDot[meta.tone])}
                aria-hidden
              />
              <span className="truncate">{w.name}</span>
              <span className="ml-1 text-xs text-muted-foreground">
                {meta.label}
                {w.info?.os ? `, ${w.info.os}/${w.info.arch}` : ""}
                {why !== undefined && `, ${why}`}
              </span>
            </SelectItem>
          );
        })}
        {workers.length === 0 && (
          <div className="px-2 py-3 text-[13px] text-muted-foreground">No workers yet.</div>
        )}
      </SelectContent>
    </Select>
  );
}
