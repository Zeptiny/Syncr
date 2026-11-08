import { Check, ChevronsUpDown, Globe, Search } from "lucide-react";
import { useMemo, useRef, useState } from "react";

import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Popover, PopoverContent, PopoverTrigger } from "@/components/ui/popover";
import {
  browserTimeZone,
  filterZones,
  timeZones,
  tzOffset,
  zoneLabel,
  type ZoneOption,
} from "@/lib/timezones";
import { cn } from "@/lib/utils";

/** TimezonePickerProps configures TimezonePicker. */
export interface TimezonePickerProps {
  id?: string;
  value: string;
  onChange: (zone: string) => void;
  invalid?: boolean;
  className?: string;
}

/**
 * TimezonePicker is a searchable list of the browser's IANA zones (plus
 * UTC), each with its current UTC offset. The viewer's own zone is first.
 */
export function TimezonePicker({ id, value, onChange, invalid, className }: TimezonePickerProps) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const list = useRef<HTMLUListElement>(null);
  const local = browserTimeZone();
  const zones = useMemo<ZoneOption[]>(() => {
    const now = new Date();
    const all = timeZones();
    const ordered = [local, ...all.filter((z) => z !== local)];
    return ordered.map((zone) => ({ zone, offset: tzOffset(zone, now) }));
  }, [local]);
  const shown = useMemo(() => filterZones(query, zones), [query, zones]);

  const choose = (zone: string) => {
    onChange(zone);
    setOpen(false);
    setQuery("");
  };
  const move = (delta: number) => {
    const next = Math.max(0, Math.min(shown.length - 1, active + delta));
    setActive(next);
    list.current?.children[next]?.scrollIntoView({ block: "nearest" });
  };

  return (
    <Popover
      open={open}
      onOpenChange={(o) => {
        setOpen(o);
        if (o) {
          setQuery("");
          setActive(
            Math.max(
              0,
              zones.findIndex((z) => z.zone === value),
            ),
          );
        }
      }}
    >
      <PopoverTrigger asChild>
        <Button
          id={id}
          type="button"
          variant="outline"
          role="combobox"
          aria-expanded={open}
          aria-invalid={invalid || undefined}
          className={cn("w-full justify-between bg-card font-normal", className)}
        >
          <span className="flex min-w-0 items-center gap-2">
            <Globe className="text-muted-foreground" aria-hidden />
            <span className="truncate">{value ? zoneLabel(value) : "Choose a time zone"}</span>
          </span>
          <span className="flex shrink-0 items-center gap-2">
            {value && (
              <span className="tabular font-mono text-xs text-muted-foreground">
                UTC{tzOffset(value)}
              </span>
            )}
            <ChevronsUpDown className="opacity-50" aria-hidden />
          </span>
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-[min(24rem,calc(100vw-2rem))] p-0">
        <div className="relative border-b">
          <Search
            className="pointer-events-none absolute top-1/2 left-3 size-4 -translate-y-1/2 text-muted-foreground"
            aria-hidden
          />
          <Input
            autoFocus
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setActive(0);
            }}
            onKeyDown={(e) => {
              if (e.key === "ArrowDown" || e.key === "ArrowUp") {
                e.preventDefault();
                move(e.key === "ArrowDown" ? 1 : -1);
              } else if (e.key === "Enter" && shown[active]) {
                e.preventDefault();
                choose(shown[active].zone);
              }
            }}
            placeholder="Search a city or an offset like +05:30"
            aria-label="Search time zones"
            aria-controls={`${id ?? "tz"}-list`}
            aria-activedescendant={shown[active] ? `${id ?? "tz"}-opt-${active}` : undefined}
            className="h-10 rounded-none border-0 pl-9 shadow-none focus-visible:ring-0"
          />
        </div>
        <ul
          ref={list}
          id={`${id ?? "tz"}-list`}
          role="listbox"
          aria-label="Time zones"
          className="max-h-72 overflow-y-auto p-1"
        >
          {shown.map((z, i) => (
            <li
              key={z.zone}
              id={`${id ?? "tz"}-opt-${i}`}
              role="option"
              aria-selected={z.zone === value}
              onMouseMove={() => setActive(i)}
              onClick={() => choose(z.zone)}
              className={cn(
                "flex cursor-pointer items-center gap-2 rounded-md px-2 py-1.5 text-[13px]",
                i === active && "bg-accent",
              )}
            >
              <Check
                className={cn("size-3.5 shrink-0", z.zone === value ? "text-primary" : "invisible")}
                aria-hidden
              />
              <span className="min-w-0 flex-1 truncate">
                {zoneLabel(z.zone)}
                {z.zone === local && (
                  <span className="ml-1.5 text-xs text-muted-foreground">your zone</span>
                )}
              </span>
              <span className="tabular shrink-0 font-mono text-xs text-muted-foreground">
                {z.offset}
              </span>
            </li>
          ))}
          {shown.length === 0 && (
            <li className="px-2 py-6 text-center text-[13px] text-muted-foreground">
              No time zone matches “{query}”.
            </li>
          )}
        </ul>
      </PopoverContent>
    </Popover>
  );
}
