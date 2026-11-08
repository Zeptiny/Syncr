import { Bell, Mail, MessagesSquare, Webhook, type LucideIcon } from "lucide-react";

import { cn } from "@/lib/utils";

const icons: Record<string, LucideIcon> = {
  discord: MessagesSquare,
  email: Mail,
  webhook: Webhook,
};

/** ChannelIcon is the glyph of a notification channel, in a tinted square. */
export function ChannelIcon({ type, className }: { type: string; className?: string }) {
  const Icon = icons[type] ?? Bell;
  return (
    <span
      className={cn(
        "grid size-8 shrink-0 place-items-center rounded-md bg-secondary text-secondary-foreground",
        className,
      )}
      aria-hidden
    >
      <Icon className="size-4" />
    </span>
  );
}
