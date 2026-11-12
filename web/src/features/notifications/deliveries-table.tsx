import { useMutation } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { createColumnHelper } from "@tanstack/react-table";
import { RotateCw } from "lucide-react";
import { useMemo } from "react";
import { toast } from "sonner";

import { ChannelIcon } from "@/components/app/channel-icon";
import { DataTable, type Pager } from "@/components/app/data-table";
import { RelativeTime } from "@/components/app/relative-time";
import { DeliveryStatusBadge } from "@/components/app/status-badge";
import { Button } from "@/components/ui/button";
import {
  ContactService,
  DeliveryStatus,
  type Contact,
  type Delivery,
} from "@/gen/syncr/v1/notify_pb";
import { errorMessage } from "@/lib/errors";
import { tsDate } from "@/lib/format";
import { invalidateService } from "@/lib/invalidate";
import { eventMeta } from "@/lib/status";

const col = createColumnHelper<Delivery>();

function When({ d }: { d: Delivery }) {
  if (d.status === DeliveryStatus.DELIVERED)
    return (
      <span className="text-[13px] whitespace-nowrap">
        Delivered <RelativeTime date={tsDate(d.deliveredAt ?? d.lastAttemptAt)} />
      </span>
    );
  if (d.status === DeliveryStatus.PENDING && d.nextAttemptAt)
    return (
      <span className="text-[13px] whitespace-nowrap">
        {d.attempts ? "Retries" : "Sends"} <RelativeTime date={tsDate(d.nextAttemptAt)} />
      </span>
    );
  if (d.status === DeliveryStatus.SENDING)
    return <span className="text-[13px] text-live">Sending now</span>;
  return (
    <span className="text-[13px] whitespace-nowrap text-muted-foreground">
      Last tried <RelativeTime date={tsDate(d.lastAttemptAt ?? d.createdAt)} />
    </span>
  );
}

/** DeliveriesTableProps configures DeliveriesTable. */
export interface DeliveriesTableProps {
  deliveries: Delivery[] | undefined;
  isLoading?: boolean;
  error?: unknown;
  onRetry?: () => void;
  pager?: Pager;
  /** contacts, when given, adds a contact column (the log of every contact). */
  contacts?: Map<string, Contact>;
}

/**
 * RetryButton sends a delivery that gave up again. The mutation lives here
 * and not in the table: its state changes with every press, and column
 * definitions that depended on it would make the table mount every cell
 * anew (taking the focus from the pressed button).
 */
function RetryButton({ delivery }: { delivery: Delivery }) {
  const queryClient = useQueryClient();
  const retry = useMutation(ContactService.method.retryDelivery);
  return (
    <Button
      variant="outline"
      size="sm"
      disabled={retry.isPending}
      onClick={() =>
        void retry
          .mutateAsync({ deliveryId: delivery.id })
          .then(() => invalidateService(queryClient, ContactService))
          .then(() => toast.success("Sending it again"))
          .catch((err: unknown) => toast.error(errorMessage(err)))
      }
    >
      <RotateCw aria-hidden />
      Retry
    </Button>
  );
}

/**
 * DeliveriesTable is a delivery log: what was sent, to whom, how it went,
 * how many attempts it took and the last error. Deliveries that gave up
 * can be sent again.
 */
export function DeliveriesTable({
  deliveries,
  isLoading,
  error,
  onRetry,
  pager,
  contacts,
}: DeliveriesTableProps) {
  const columns = useMemo(
    () => [
      col.accessor("status", {
        header: "Status",
        cell: ({ row: { original: d } }) => (
          <DeliveryStatusBadge status={d.status} attempts={d.attempts} />
        ),
        meta: { className: "w-28" },
      }),
      col.display({
        id: "what",
        header: "Notification",
        cell: ({ row: { original: d } }) => (
          <div className="flex min-w-0 flex-col">
            <span className="truncate text-[13px] font-medium">{d.title}</span>
            <span className="flex items-center gap-2 text-xs text-muted-foreground">
              {eventMeta(d.event).label}
              {d.jobId && (
                <Link
                  to="/jobs/$jobId"
                  params={{ jobId: d.jobId }}
                  className="text-primary hover:underline"
                >
                  Job
                </Link>
              )}
              {d.scheduleId && (
                <Link
                  to="/schedules/$scheduleId"
                  params={{ scheduleId: d.scheduleId }}
                  className="text-primary hover:underline"
                >
                  Schedule
                </Link>
              )}
            </span>
          </div>
        ),
        meta: { className: "min-w-[14rem] max-w-[20rem]" },
      }),
      ...(contacts
        ? [
            col.accessor("contactId", {
              header: "Contact",
              cell: (c) => {
                const ct = contacts.get(c.getValue());
                return (
                  <span className="flex items-center gap-2 text-[13px] whitespace-nowrap">
                    <ChannelIcon type={ct?.channelType ?? ""} className="size-6 [&_svg]:size-3.5" />
                    {ct?.name ?? "Deleted contact"}
                  </span>
                );
              },
            }),
          ]
        : []),
      col.display({
        id: "attempts",
        header: "Attempts",
        cell: ({ row: { original: d } }) => (
          <span className="tabular text-[13px] text-muted-foreground">
            {d.attempts} of {d.maxAttempts || 8}
          </span>
        ),
      }),
      col.display({ id: "when", header: "When", cell: ({ row }) => <When d={row.original} /> }),
      col.accessor("lastError", {
        header: "Last error",
        cell: (c) =>
          c.getValue() ? (
            <span
              className="line-clamp-2 max-w-[20rem] font-mono text-[12px] leading-4.5 break-words text-danger"
              title={c.getValue()}
            >
              {c.getValue()}
            </span>
          ) : (
            <span className="text-[13px] text-muted-foreground">–</span>
          ),
      }),
      col.display({
        id: "retry",
        header: () => <span className="sr-only">Retry</span>,
        cell: ({ row: { original: d } }) =>
          d.status === DeliveryStatus.DEAD ? <RetryButton delivery={d} /> : null,
        meta: { className: "w-24 text-right" },
      }),
    ],
    [contacts],
  );
  return (
    <DataTable
      aria-label="Deliveries"
      columns={columns}
      data={deliveries}
      isLoading={isLoading}
      error={error}
      onRetry={onRetry}
      pager={pager}
      getRowId={(d) => d.id}
      empty={
        <p className="p-8 text-center text-[13px] text-muted-foreground">
          Nothing has been sent yet. Notifications appear here as runs finish.
        </p>
      }
    />
  );
}
