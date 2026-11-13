import { ChevronDown } from "lucide-react";

import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuGroup,
  DropdownMenuLabel,
  DropdownMenuRadioGroup,
  DropdownMenuRadioItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import type { ResticSnapshot } from "@/gen/syncr/v1/link_pb";
import { formatDateTime, tsDate } from "@/lib/format";
import { cn } from "@/lib/utils";

import { shortId } from "./model";
import { switcherChoices } from "./snapshot-groups";

/** SnapshotMenuProps configures SnapshotMenu. */
export interface SnapshotMenuProps {
  /** snapshots are the repository's (the cached list). */
  snapshots: ResticSnapshot[];
  /** value is the chosen snapshot's ID ("" for none, or allLabel). */
  value: string;
  onChange: (snapshotId: string) => void;
  /** label names the menu for screen readers: "Compare from". */
  label: string;
  /** near puts the snapshots of its host and paths first. */
  near?: ResticSnapshot;
  /** allLabel offers a first choice "" with this name ("All snapshots"). */
  allLabel?: string;
  /** placeholder is the button's text while nothing is chosen. */
  placeholder?: string;
  /** exclude leaves one snapshot out (the other side of a comparison). */
  exclude?: string;
  /** trigger replaces the button's content (the menu then never shows the value). */
  trigger?: React.ReactNode;
  variant?: "outline" | "ghost";
  size?: "default" | "sm";
  disabled?: boolean;
  className?: string;
}

const when = (s: ResticSnapshot) => formatDateTime(tsDate(s.time));

/**
 * SnapshotMenu chooses one snapshot of a repository from a menu: the
 * snapshots of the same host and paths first (near), then the others,
 * newest first, each with its time, host and short ID.
 */
export function SnapshotMenu({
  snapshots,
  value,
  onChange,
  label,
  near,
  allLabel,
  placeholder = "Choose a snapshot",
  exclude,
  trigger,
  variant = "outline",
  size = "default",
  disabled,
  className,
}: SnapshotMenuProps) {
  const listed = snapshots.filter((s) => s.id !== exclude);
  const { same, others } = switcherChoices(listed, near);
  const chosen = snapshots.find((s) => s.id === value);
  const text = chosen ? when(chosen) : value ? shortId(value) : (allLabel ?? placeholder);
  const item = (s: ResticSnapshot, host: boolean) => (
    <DropdownMenuRadioItem key={s.id} value={s.id} className="gap-3">
      <span className="tabular">{when(s)}</span>
      {host && <span className="text-xs text-muted-foreground">{s.host || "–"}</span>}
      <span className="ml-auto font-mono text-xs text-muted-foreground">{shortId(s.id)}</span>
    </DropdownMenuRadioItem>
  );
  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button
          variant={variant}
          size={size}
          className={cn("max-w-full justify-between", className)}
          aria-label={trigger ? undefined : `${label}: ${text}`}
          disabled={disabled || listed.length === 0}
        >
          {trigger ?? (
            <>
              <span className="tabular truncate">{text}</span>
              <ChevronDown aria-hidden />
            </>
          )}
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent
        align="start"
        className="max-h-[min(28rem,70dvh)] min-w-72 overflow-y-auto"
      >
        <DropdownMenuRadioGroup value={value} onValueChange={onChange}>
          {allLabel !== undefined && (
            <>
              <DropdownMenuRadioItem value="">{allLabel}</DropdownMenuRadioItem>
              <DropdownMenuSeparator />
            </>
          )}
          {same.length > 0 && (
            <DropdownMenuGroup>
              <DropdownMenuLabel className="text-xs text-muted-foreground">
                Same host and paths
              </DropdownMenuLabel>
              {same.map((s) => item(s, false))}
            </DropdownMenuGroup>
          )}
          {same.length > 0 && others.length > 0 && <DropdownMenuSeparator />}
          {others.length > 0 && (
            <DropdownMenuGroup>
              <DropdownMenuLabel className="text-xs text-muted-foreground">
                {same.length > 0 ? "Other snapshots" : "Snapshots"}
              </DropdownMenuLabel>
              {others.map((s) => item(s, true))}
            </DropdownMenuGroup>
          )}
        </DropdownMenuRadioGroup>
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
