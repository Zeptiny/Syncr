import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { Trash2 } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { ChannelIcon } from "@/components/app/channel-icon";
import { ConfirmDialog } from "@/components/app/confirm-dialog";
import { ErrorState } from "@/components/app/error-state";
import { PageHeader } from "@/components/app/page-header";
import { Panel } from "@/components/app/panel";
import { RelativeTime } from "@/components/app/relative-time";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { ChannelService, ContactService } from "@/gen/syncr/v1/notify_pb";
import { ScheduleService } from "@/gen/syncr/v1/schedules_pb";
import { useKeysetPager } from "@/hooks/use-keyset-pager";
import { tsDate } from "@/lib/format";
import { invalidateService } from "@/lib/invalidate";

import { ContactForm } from "./contact-form";
import { DeliveriesTable } from "./deliveries-table";

/** ContactPage edits a contact, tests it and shows what was sent to it. */
export function ContactPage({ contactId }: { contactId: string }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const q = useQuery(ContactService.method.getContact, { contactId });
  const channels = useQuery(ChannelService.method.listChannels, {}, { staleTime: Infinity });
  const update = useMutation(ContactService.method.updateContact);
  const del = useMutation(ContactService.method.deleteContact);
  const pager = useKeysetPager(contactId);
  const deliveries = useQuery(
    ContactService.method.listDeliveries,
    { contactId, pageSize: 10, pageToken: pager.token },
    { placeholderData: (prev) => prev, refetchInterval: 10_000 },
  );
  const [deleting, setDeleting] = useState(false);

  if (q.error) return <ErrorState error={q.error} onRetry={() => void q.refetch()} />;
  const contact = q.data?.contact;
  const channel = channels.data?.channels.find((c) => c.id === contact?.channelType);
  if (!contact || !channel) {
    return (
      <div className="flex flex-col gap-4">
        <Skeleton className="h-8 w-72" />
        <Skeleton className="h-96 w-full max-w-4xl" />
      </div>
    );
  }
  const used = contact.usedBy;

  return (
    <>
      <PageHeader
        back={{ to: "/notifications", label: "Notifications" }}
        title={
          <span className="flex items-center gap-3">
            <ChannelIcon type={channel.id} className="size-9" />
            {contact.name}
          </span>
        }
        meta={
          <span className="text-[13px] text-muted-foreground">
            {channel.label}, {contact.summary || "no summary"}, updated{" "}
            <RelativeTime date={tsDate(contact.updatedAt)} />
          </span>
        }
      />
      <div className="flex flex-col gap-6">
        <Panel className="max-w-4xl">
          <ContactForm
            key={String(contact.version)}
            channel={channel}
            contact={contact}
            onCancel={() => void navigate({ to: "/notifications" })}
            onSubmit={async (input) => {
              await update.mutateAsync({ contactId: contact.id, version: contact.version, input });
              await Promise.all([
                invalidateService(queryClient, ContactService),
                invalidateService(queryClient, ScheduleService),
              ]);
              toast.success(`Saved ${input.name ?? contact.name}`);
            }}
            footer={
              <Button
                type="button"
                variant="ghost"
                className="text-danger hover:text-danger"
                onClick={() => setDeleting(true)}
              >
                <Trash2 aria-hidden />
                Delete contact
              </Button>
            }
          />
        </Panel>
        {used.length > 0 && (
          <p className="max-w-4xl text-[13px] text-muted-foreground">
            Linked to{" "}
            {used.map((s, i) => (
              <span key={s.id}>
                {i > 0 && (i === used.length - 1 ? " and " : ", ")}
                <Link
                  to="/schedules/$scheduleId"
                  params={{ scheduleId: s.id }}
                  className="text-primary hover:underline"
                >
                  {s.name}
                </Link>
              </span>
            ))}
            . Choose its events on each schedule.
          </p>
        )}
        <section className="flex flex-col gap-3">
          <div>
            <h2 className="text-[15px] font-semibold">Delivery log</h2>
            <p className="text-[13px] text-muted-foreground">
              What was sent to {contact.name}. Failed sends are retried for about an hour, then
              given up; you can retry those.
            </p>
          </div>
          <DeliveriesTable
            deliveries={deliveries.data?.deliveries}
            isLoading={deliveries.isPending}
            error={deliveries.error ?? undefined}
            onRetry={() => void deliveries.refetch()}
            pager={{
              page: pager.page,
              canPrev: pager.canPrev,
              canNext: !!deliveries.data?.nextPageToken,
              onPrev: pager.prev,
              onNext: () => pager.next(deliveries.data?.nextPageToken ?? ""),
            }}
          />
        </section>
      </div>
      <ConfirmDialog
        open={deleting}
        onOpenChange={setDeleting}
        destructive
        title={`Delete ${contact.name}?`}
        description={
          used.length
            ? `${used.map((s) => s.name).join(", ")} ${used.length === 1 ? "stops" : "stop"} notifying it. Its saved credentials and delivery log are erased.`
            : "Its saved credentials and delivery log are erased."
        }
        confirmLabel="Delete contact"
        onConfirm={async () => {
          await del.mutateAsync({ contactId: contact.id });
          await Promise.all([
            invalidateService(queryClient, ContactService),
            invalidateService(queryClient, ScheduleService),
          ]);
          toast.success(`Deleted ${contact.name}`);
          await navigate({ to: "/notifications" });
        }}
      />
    </>
  );
}
