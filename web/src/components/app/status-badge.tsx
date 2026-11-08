import type { JobStatus, WorkerStatus } from "@/gen/syncr/v1/api_pb";
import type { DeliveryStatus } from "@/gen/syncr/v1/notify_pb";
import type { ScheduleRunStatus } from "@/gen/syncr/v1/schedules_pb";
import {
  deliveryStatusMeta,
  jobStatusMeta,
  scheduleRunStatusMeta,
  workerStatusMeta,
  type StatusMeta,
} from "@/lib/status";
import { toneClasses } from "@/lib/tones";
import { cn } from "@/lib/utils";

/** StatusPill shows any status as a coloured, labelled dot. */
export function StatusPill({
  meta,
  className,
  title,
}: {
  meta: StatusMeta;
  className?: string;
  title?: string;
}) {
  return (
    <span
      data-tone={meta.tone}
      title={title}
      className={cn(
        "inline-flex h-6 items-center gap-1.5 rounded-full px-2.5 text-xs font-medium whitespace-nowrap",
        toneClasses[meta.tone],
        className,
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
  return <StatusPill meta={meta} className={props.className} />;
}

/** ScheduleRunStatusBadge shows where a schedule run is. */
export function ScheduleRunStatusBadge({
  status,
  className,
}: {
  status: ScheduleRunStatus;
  className?: string;
}) {
  return <StatusPill meta={scheduleRunStatusMeta(status)} className={className} />;
}

/** DeliveryStatusBadge shows a notification delivery's status. */
export function DeliveryStatusBadge({
  status,
  attempts,
  className,
}: {
  status: DeliveryStatus;
  attempts?: number;
  className?: string;
}) {
  return <StatusPill meta={deliveryStatusMeta(status, attempts)} className={className} />;
}
