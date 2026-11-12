import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { toast } from "sonner";

import { ChannelIcon } from "@/components/app/channel-icon";
import { ErrorState } from "@/components/app/error-state";
import { PageHeader } from "@/components/app/page-header";
import { Panel } from "@/components/app/panel";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { ChannelService, ContactService } from "@/gen/syncr/v1/notify_pb";
import { invalidateService } from "@/lib/invalidate";

import { ContactForm } from "./contact-form";

/** NewContactPage picks a channel, then shows its form. */
export function NewContactPage({ type }: { type?: string }) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const channels = useQuery(ChannelService.method.listChannels, {}, { staleTime: Infinity });
  const create = useMutation(ContactService.method.createContact);
  const chosen = type ? channels.data?.channels.find((c) => c.id === type) : undefined;

  if (channels.error)
    return <ErrorState error={channels.error} onRetry={() => void channels.refetch()} />;

  if (!chosen) {
    return (
      <>
        <PageHeader
          back={{ to: "/notifications", label: "Notifications" }}
          title="Add a contact"
          description="Choose how Syncr should reach you. You can add as many contacts of each kind as you like."
        />
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {channels.isPending &&
            Array.from({ length: 3 }, (_, i) => <Skeleton key={i} className="h-28 rounded-lg" />)}
          {channels.data?.channels.map((c) => (
            <Link
              key={c.id}
              to="/notifications/new"
              search={{ type: c.id }}
              className="group flex gap-3.5 rounded-lg border bg-card p-4 transition-colors hover:border-primary/60 focus-visible:border-primary"
            >
              <ChannelIcon
                type={c.id}
                className="size-9 transition-colors group-hover:bg-primary group-hover:text-primary-foreground"
              />
              <div className="min-w-0">
                <div className="font-semibold">{c.label}</div>
                <p className="mt-0.5 text-[13px] leading-5 text-muted-foreground">
                  {c.description}
                </p>
              </div>
            </Link>
          ))}
        </div>
      </>
    );
  }

  return (
    <>
      <PageHeader
        back={{ to: "/notifications", label: "Notifications" }}
        title={`New ${chosen.label} contact`}
        description={chosen.description}
        actions={
          <Button variant="outline" asChild>
            <Link to="/notifications/new">Choose another kind</Link>
          </Button>
        }
      />
      <Panel className="max-w-4xl">
        <ContactForm
          channel={chosen}
          onCancel={() => void navigate({ to: "/notifications" })}
          onSubmit={async (input) => {
            const res = await create.mutateAsync({ input });
            await invalidateService(queryClient, ContactService);
            toast.success(`Added ${res.contact?.name ?? "contact"}`);
            if (res.contact)
              await navigate({
                to: "/notifications/$contactId",
                params: { contactId: res.contact.id },
              });
          }}
        />
      </Panel>
    </>
  );
}
