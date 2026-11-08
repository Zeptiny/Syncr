import type { JobStatus, WorkerStatus } from "@/gen/syncr/v1/api_pb";
import { jobStatusMeta, workerStatusMeta, type StatusMeta, type Tone } from "@/lib/status";
import { cn } from "@/lib/utils";

const tones: Record<Tone, string> = {
  live: "bg-live-soft text-live",
  success: "bg-success-soft text-success",
  danger: "bg-danger-soft text-danger",
  warning: "bg-warning-soft text-warning",
  neutral: "bg-neutral-soft text-neutral",
};

/** StatusBadgeProps selects a job or a worker status. */
export type StatusBadgeProps = { className?: string } & (
  | { job: JobStatus; cancelRequested?: boolean; worker?: never }
  | { worker: WorkerStatus; job?: never; cancelRequested?: never }
);

/** StatusBadge shows a job or worker status as a coloured, labelled dot. */
export function StatusBadge(props: StatusBadgeProps) {
  const meta: StatusMeta =
    props.job !== undefined
      ? jobStatusMeta(props.job, props.cancelRequested)
      : workerStatusMeta(props.worker);
  return (
    <span
      data-tone={meta.tone}
      className={cn(
        "inline-flex h-6 items-center gap-1.5 rounded-full px-2.5 text-xs font-medium whitespace-nowrap",
        tones[meta.tone],
        props.className,
      )}
    >
      <span className="relative flex size-1.5" aria-hidden>
        {meta.pulse && (
          <span className="absolute inline-flex size-full animate-ping rounded-full bg-current opacity-60" />
        )}
        <span className="relative inline-flex size-1.5 rounded-full bg-current" />
      </span>
      {meta.label}
    </span>
  );
}
