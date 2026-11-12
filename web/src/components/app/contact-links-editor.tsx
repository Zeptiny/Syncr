import { Link } from "@tanstack/react-router";
import { BellPlus, Plus, Trash2 } from "lucide-react";

import { Button } from "@/components/ui/button";
import { Select, SelectContent, SelectItem, SelectTrigger } from "@/components/ui/select";
import type { Contact, NotificationEvent } from "@/gen/syncr/v1/notify_pb";
import { defaultLinkEvents } from "@/lib/status";

import { ChannelIcon } from "./channel-icon";
import { EventChips } from "./event-chips";

/** ContactLink is one contact a schedule notifies. */
export interface ContactLink {
  contactId: string;
  events: NotificationEvent[];
}

/** ContactLinksEditorProps configures ContactLinksEditor. */
export interface ContactLinksEditorProps {
  value: ContactLink[];
  onChange: (v: ContactLink[]) => void;
  /** contacts are the user's contacts, to choose from. */
  contacts: Contact[];
  /** errors are by link index. */
  errors?: Record<number, string>;
}

/**
 * ContactLinksEditor picks the contacts a schedule notifies and, for each,
 * the events. A new link starts with failed, interrupted and missed.
 */
export function ContactLinksEditor({
  value,
  onChange,
  contacts,
  errors = {},
}: ContactLinksEditorProps) {
  const byId = new Map(contacts.map((c) => [c.id, c]));
  const free = contacts.filter((c) => !value.some((l) => l.contactId === c.id));
  const set = (i: number, events: NotificationEvent[]) =>
    onChange(value.map((l, j) => (j === i ? { ...l, events } : l)));

  if (!contacts.length && !value.length) {
    return (
      <div className="flex flex-col items-start gap-3 rounded-lg border border-dashed px-4 py-4 text-[13px] text-muted-foreground sm:flex-row sm:items-center sm:justify-between">
        <span>
          Add a contact (Discord, email or a webhook) to hear about failed or missed runs.
        </span>
        <Button variant="outline" size="sm" asChild>
          <Link to="/notifications/new">
            <BellPlus aria-hidden />
            Add a contact
          </Link>
        </Button>
      </div>
    );
  }

  return (
    <div className="flex flex-col gap-2">
      {value.length > 0 && (
        <ul className="flex flex-col divide-y rounded-lg border">
          {value.map((l, i) => {
            const c = byId.get(l.contactId);
            return (
              <li
                key={l.contactId}
                className="flex flex-col gap-2 px-3.5 py-3 sm:flex-row sm:items-center sm:gap-4"
              >
                <div className="flex min-w-0 items-center gap-2.5 sm:w-52">
                  <ChannelIcon type={c?.channelType ?? ""} className="size-7" />
                  <div className="min-w-0">
                    <div className="truncate text-[13px] font-medium">
                      {c?.name ?? "Deleted contact"}
                    </div>
                    <div className="truncate text-xs text-muted-foreground">{c?.summary}</div>
                  </div>
                </div>
                <div className="flex min-w-0 flex-1 flex-col gap-1">
                  <EventChips
                    value={l.events}
                    onChange={(ev) => set(i, ev)}
                    label={`Events for ${c?.name ?? "contact"}`}
                    invalid={!!errors[i]}
                  />
                  {errors[i] && <p className="text-xs text-danger">{errors[i]}</p>}
                </div>
                <Button
                  type="button"
                  variant="ghost"
                  size="icon-sm"
                  className="self-end text-muted-foreground sm:self-center"
                  onClick={() => onChange(value.filter((_, j) => j !== i))}
                  aria-label={`Stop notifying ${c?.name ?? "contact"}`}
                >
                  <Trash2 aria-hidden />
                </Button>
              </li>
            );
          })}
        </ul>
      )}
      {free.length > 0 && (
        <Select
          value=""
          onValueChange={(id) =>
            onChange([...value, { contactId: id, events: [...defaultLinkEvents] }])
          }
        >
          <SelectTrigger
            className="w-full bg-card sm:w-64"
            aria-label={value.length ? "Notify another contact" : "Notify a contact"}
          >
            <span className="flex items-center gap-2 text-muted-foreground">
              <Plus className="size-4" aria-hidden />
              {value.length ? "Notify another contact" : "Notify a contact"}
            </span>
          </SelectTrigger>
          <SelectContent position="popper" align="start">
            {free.map((c) => (
              <SelectItem key={c.id} value={c.id}>
                <ChannelIcon type={c.channelType} className="size-5 [&_svg]:size-3" />
                {c.name}
              </SelectItem>
            ))}
          </SelectContent>
        </Select>
      )}
    </div>
  );
}
