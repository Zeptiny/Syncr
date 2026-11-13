import { ChevronDown } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import type { Worker } from "@/gen/syncr/v1/api_pb";
import type { ProbeWorkerRef } from "@/gen/syncr/v1/probe_pb";
import { isOnline } from "@/lib/workers";

import { eligibleWorkers } from "./helpers";

const AUTO = "__auto__";

/** WorkerChooserProps configures WorkerChooser. */
export interface WorkerChooserProps {
  workers: Worker[];
  /** op is the probe operation the listing needs ("list_remote", "restic_ls"). */
  op: string;
  /** value is the chosen worker's ID; empty lets the controller choose. */
  value: string;
  onChange: (workerId: string) => void;
  /** listedBy is the worker that answered the last listing. */
  listedBy?: ProbeWorkerRef;
  /** required drops "Automatic": a remote on a worker's own disk. */
  required?: boolean;
  /** label names the choice for screen readers (default "Listing worker"). */
  label?: string;
  /** menuLabel heads the menu (default "Listed by"). */
  menuLabel?: string;
}

/**
 * WorkerChooser is the "by nas-01 ▾" of a browser's toolbar: which worker
 * lists a remote or a snapshot. Only online workers that can do the
 * listing are offered.
 */
export function WorkerChooser({
  workers,
  op,
  value,
  onChange,
  listedBy,
  required,
  label = "Listing worker",
  menuLabel = "Listed by",
}: WorkerChooserProps) {
  const offered = eligibleWorkers(workers, op);
  const chosen = value ? workers.find((w) => w.id === value) : undefined;
  if (chosen && !offered.includes(chosen)) offered.unshift(chosen);
  const name = chosen?.name ?? listedBy?.name;
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          variant={required && !value ? "outline" : "ghost"}
          size="xs"
          className="-my-1 font-medium text-foreground"
          aria-label={`${label}: ${name ?? (required ? "none chosen" : "automatic")}. Choose another`}
        >
          {name ? `by ${name}` : required ? "Choose a worker" : "by any worker"}
          <ChevronDown aria-hidden />
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="min-w-56">
        <DropdownMenuLabel>{menuLabel}</DropdownMenuLabel>
        <DropdownMenuSeparator />
        <DropdownMenuRadioGroup
          value={value || AUTO}
          onValueChange={(v) => onChange(v === AUTO ? "" : v)}
        >
          {!required && (
            <DropdownMenuRadioItem value={AUTO}>
              Automatic
              {!value && listedBy?.name && (
                <span className="text-xs text-muted-foreground">({listedBy.name})</span>
              )}
            </DropdownMenuRadioItem>
          )}
          {offered.map((w) => (
            <DropdownMenuRadioItem key={w.id} value={w.id} disabled={!isOnline(w)}>
              {w.name}
              {!isOnline(w) && <span className="text-xs text-muted-foreground">offline</span>}
            </DropdownMenuRadioItem>
          ))}
        </DropdownMenuRadioGroup>
        {offered.length === 0 && (
          <p className="px-2 py-2 text-[13px] text-muted-foreground">
            No online worker can do this.
          </p>
        )}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
