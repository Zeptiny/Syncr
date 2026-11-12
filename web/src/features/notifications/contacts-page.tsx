import { useQuery } from "@connectrpc/connect-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { createColumnHelper } from "@tanstack/react-table";
import { BellRing, Plus } from "lucide-react";
import { useMemo } from "react";

import { ChannelIcon } from "@/components/app/channel-icon";
import { DataTable } from "@/components/app/data-table";
import { EmptyState } from "@/components/app/empty-state";
import { PageHeader } from "@/components/app/page-header";
import { StatusPill } from "@/components/app/status-badge";
import { Button } from "@/components/ui/button";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  ChannelService,
  ContactService,
  DeliveryStatus,
  type Contact,
} from "@/gen/syncr/v1/notify_pb";
import { useKeysetPager } from "@/hooks/use-keyset-pager";
import { eventMeta } from "@/lib/status";

import { DeliveriesTable } from "./deliveries-table";
import type { NotificationsSearch } from "./search";

const col = createColumnHelper<Contact>();

function UsedBy({ c }: { c: Contact }) {
  const parts: React.ReactNode[] = [];
  if (c.usedBy.length) {
    parts.push(
      <span key="s">
        {c.usedBy.length === 1
          ? c.usedBy[0].name
          : `${c.usedBy[0].name} and ${c.usedBy.length - 1} more`}
      </span>,
    );
  }
  if (c.jobEvents.length) {
    parts.push(
      <span key="m" className="text-muted-foreground">
        jobs by hand: {c.jobEvents.map((e) => eventMeta(e).label.toLowerCase()).join(", ")}
      </span>,
    );
  }
  if (!parts.length) return <span className="text-[13px] text-muted-foreground">Nothing yet</span>;
  return <div className="flex min-w-0 flex-col text-[13px]">{parts}</div>;
}

function ContactsTable() {
  const navigate = useNavigate();
  const q = useQuery(ContactService.method.listContacts, {});
  const channels = useQuery(ChannelService.method.listChannels, {}, { staleTime: Infinity });
  const labels = useMemo(
    () => new Map((channels.data?.channels ?? []).map((c) => [c.id, c.label])),
    [channels.data],
  );
  const columns = useMemo(
    () => [
      col.accessor("name", {
        header: "Contact",
        cell: ({ row: { original: c } }) => (
          <div className="flex min-w-0 items-center gap-3">
            <ChannelIcon type={c.channelType} />
            <div className="flex min-w-0 flex-col">
              <span className="truncate font-medium">{c.name}</span>
              <span className="truncate text-xs text-muted-foreground">
                {labels.get(c.channelType) ?? c.channelType}
              </span>
            </div>
          </div>
        ),
      }),
      col.accessor("summary", {
        header: "Sends to",
        cell: (c) => <span className="text-[13px]">{c.getValue() || "–"}</span>,
      }),
      col.display({
        id: "used",
        header: "Notifies about",
        cell: ({ row }) => <UsedBy c={row.original} />,
      }),
      col.accessor("enabled", {
        header: "Status",
        cell: (c) => (
          <StatusPill
            meta={
              c.getValue()
                ? { label: "Enabled", tone: "success" }
                : { label: "Disabled", tone: "neutral" }
            }
          />
        ),
        meta: { className: "text-right" },
      }),
    ],
    [labels],
  );
  return (
    <DataTable
      aria-label="Contacts"
      columns={columns}
      data={q.data?.contacts}
      isLoading={q.isPending}
      error={q.error ?? undefined}
      onRetry={() => void q.refetch()}
      getRowId={(c) => c.id}
      rowLabel={(c) => c.name}
      onRowClick={(c) =>
        void navigate({ to: "/notifications/$contactId", params: { contactId: c.id } })
      }
      empty={
        <EmptyState
          icon={BellRing}
          title="No contacts yet"
          description="A contact is where Syncr tells you about failed, interrupted or missed runs: a Discord channel, an email address or a webhook."
          action={
            <Button asChild>
              <Link to="/notifications/new">Add a contact</Link>
            </Button>
          }
        />
      }
    />
  );
}

function DeliveryLog({ search }: { search: NotificationsSearch }) {
  const navigate = useNavigate();
  const problems = search.status === "problems";
  const pager = useKeysetPager(String(problems));
  const contacts = useQuery(ContactService.method.listContacts, {});
  const q = useQuery(
    ContactService.method.listDeliveries,
    {
      statuses: problems ? [DeliveryStatus.PENDING, DeliveryStatus.DEAD] : [],
      pageSize: 25,
      pageToken: pager.token,
    },
    { placeholderData: (prev) => prev, refetchInterval: 10_000 },
  );
  const contactMap = useMemo(
    () => new Map((contacts.data?.contacts ?? []).map((c) => [c.id, c])),
    [contacts.data],
  );
  return (
    <div className="flex flex-col gap-3">
      <Label className="flex items-center gap-2 self-start text-[13px] font-normal">
        <Switch
          checked={problems}
          onCheckedChange={(c) =>
            void navigate({
              to: "/notifications",
              search: { tab: "deliveries", status: c ? "problems" : undefined },
              replace: true,
            })
          }
        />
        Only retrying and failed
      </Label>
      <DeliveriesTable
        deliveries={q.data?.deliveries}
        isLoading={q.isPending}
        error={q.error ?? undefined}
        onRetry={() => void q.refetch()}
        contacts={contactMap}
        pager={{
          page: pager.page,
          canPrev: pager.canPrev,
          canNext: !!q.data?.nextPageToken,
          onPrev: pager.prev,
          onNext: () => pager.next(q.data?.nextPageToken ?? ""),
        }}
      />
    </div>
  );
}

/** NotificationsPage lists the contacts, and the log of what was sent. */
export function NotificationsPage({ search }: { search: NotificationsSearch }) {
  const navigate = useNavigate();
  const tab = search.tab ?? "contacts";
  return (
    <>
      <PageHeader
        title="Notifications"
        description="Where Syncr tells you how your jobs went. Link a contact to a schedule to choose what it hears about."
        actions={
          <Button asChild>
            <Link to="/notifications/new">
              <Plus aria-hidden />
              Add contact
            </Link>
          </Button>
        }
      />
      <Tabs
        value={tab}
        onValueChange={(v) =>
          void navigate({
            to: "/notifications",
            search: v === "deliveries" ? { tab: "deliveries" } : {},
            replace: true,
          })
        }
        className="mb-4"
      >
        <TabsList>
          <TabsTrigger value="contacts">Contacts</TabsTrigger>
          <TabsTrigger value="deliveries">Delivery log</TabsTrigger>
        </TabsList>
      </Tabs>
      {tab === "contacts" ? <ContactsTable /> : <DeliveryLog search={search} />}
    </>
  );
}
