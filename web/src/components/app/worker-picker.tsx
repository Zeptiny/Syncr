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

/** WorkerPicker selects a worker, online ones first. */
export function WorkerPicker({
  id,
  workers,
  value,
  onChange,
  invalid,
  className,
}: {
  id?: string;
  workers: Worker[];
  value: string;
  onChange: (id: string) => void;
  invalid?: boolean;
  className?: string;
}) {
  return (
    <Select value={value || undefined} onValueChange={onChange}>
      <SelectTrigger
        id={id}
        aria-invalid={invalid || undefined}
        className={cn("w-full", className)}
      >
        <SelectValue placeholder="Choose a worker" />
      </SelectTrigger>
      <SelectContent>
        {sortWorkers(workers).map((w) => {
          const meta = workerStatusMeta(w.status);
          return (
            <SelectItem key={w.id} value={w.id}>
              <span
                className={cn("size-2 shrink-0 rounded-full", toneDot[meta.tone])}
                aria-hidden
              />
              <span className="truncate">{w.name}</span>
              <span className="ml-1 text-xs text-muted-foreground">
                {meta.label}
                {w.info?.os ? `, ${w.info.os}/${w.info.arch}` : ""}
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
