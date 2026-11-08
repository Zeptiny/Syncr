import { Check } from "lucide-react";

import type { NotificationEvent } from "@/gen/syncr/v1/notify_pb";
import { eventMeta, scheduleEvents } from "@/lib/status";
import { cn } from "@/lib/utils";

import { toneDot } from "@/lib/tones";

/** EventChipsProps configures EventChips. */
export interface EventChipsProps {
  value: NotificationEvent[];
  onChange: (v: NotificationEvent[]) => void;
  /** options are the events offered, in order (default: a schedule's). */
  options?: NotificationEvent[];
  /** label names the group for assistive technology. */
  label: string;
  invalid?: boolean;
  className?: string;
}

/**
 * EventChips picks which events notify: one toggle chip per event, in a
 * stable order. The value keeps that order whatever the click order.
 */
export function EventChips({
  value,
  onChange,
  options = scheduleEvents,
  label,
  invalid,
  className,
}: EventChipsProps) {
  const toggle = (e: NotificationEvent) => {
    const on = value.includes(e);
    const next = on ? value.filter((x) => x !== e) : [...value, e];
    onChange(options.filter((o) => next.includes(o)));
  };
  return (
    <div
      role="group"
      aria-label={label}
      aria-invalid={invalid || undefined}
      className={cn("flex flex-wrap gap-1.5", className)}
    >
      {options.map((e) => {
        const meta = eventMeta(e);
        const on = value.includes(e);
        return (
          <button
            key={e}
            type="button"
            role="checkbox"
            aria-checked={on}
            title={meta.help}
            onClick={() => toggle(e)}
            className={cn(
              "inline-flex h-7 items-center gap-1.5 rounded-full border px-2.5 text-xs font-medium transition-colors",
              on
                ? "border-foreground/20 bg-card text-foreground shadow-xs"
                : "border-dashed bg-transparent text-muted-foreground hover:border-foreground/30 hover:text-foreground",
              invalid && !on && "border-danger/50",
            )}
          >
            {on ? (
              <Check className="size-3" aria-hidden />
            ) : (
              <span
                className={cn("size-1.5 rounded-full opacity-60", toneDot[meta.tone])}
                aria-hidden
              />
            )}
            {meta.label}
          </button>
        );
      })}
    </div>
  );
}
