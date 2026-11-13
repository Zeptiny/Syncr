import { ChevronDown, ChevronLeft, ChevronRight } from "lucide-react";

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

import { shortId } from "./model";
import { switcherChoices } from "./snapshot-groups";

/** SnapshotSwitcherProps configures SnapshotSwitcher. */
export interface SnapshotSwitcherProps {
  /** snapshots are the repository's (the cached list). */
  snapshots: ResticSnapshot[];
  /** current is the open snapshot, when the list has it. */
  current: ResticSnapshot | undefined;
  /** onSwitch opens another snapshot; the page keeps the open folder. */
  onSwitch: (snapshotId: string) => void;
}

const when = (s: ResticSnapshot) => formatDateTime(tsDate(s.time));

/**
 * SnapshotSwitcher moves the snapshot browser to an older or newer
 * snapshot of the same host and paths, or to any snapshot from its menu
 * (same host and paths first). The page keeps the open folder; a folder
 * the other snapshot lacks shows "This folder isn't here".
 */
export function SnapshotSwitcher({ snapshots, current, onSwitch }: SnapshotSwitcherProps) {
  const { same, others, older, newer } = switcherChoices(snapshots, current);
  const item = (s: ResticSnapshot, host: boolean) => (
    <DropdownMenuRadioItem key={s.id} value={s.id} className="gap-3">
      <span className="tabular">{when(s)}</span>
      {host && <span className="text-xs text-muted-foreground">{s.host || "–"}</span>}
      <span className="ml-auto font-mono text-xs text-muted-foreground">{shortId(s.id)}</span>
    </DropdownMenuRadioItem>
  );
  return (
    <div className="flex items-center" role="group" aria-label="Switch snapshot">
      <Button
        variant="outline"
        size="icon"
        className="rounded-r-none"
        aria-label={older ? `Older snapshot, ${when(older)}` : "Older snapshot"}
        title="Older snapshot of the same host and paths"
        disabled={!older}
        onClick={() => older && onSwitch(older.id)}
      >
        <ChevronLeft aria-hidden />
      </Button>
      <DropdownMenu>
        <DropdownMenuTrigger asChild>
          <Button
            variant="outline"
            className="-mx-px max-w-[16rem] rounded-none"
            aria-label="Choose a snapshot"
            disabled={snapshots.length === 0}
          >
            <span className="tabular truncate">{current ? when(current) : "Snapshots"}</span>
            <ChevronDown aria-hidden />
          </Button>
        </DropdownMenuTrigger>
        <DropdownMenuContent
          align="end"
          className="max-h-[min(28rem,70dvh)] min-w-72 overflow-y-auto"
        >
          <DropdownMenuRadioGroup value={current?.id ?? ""} onValueChange={onSwitch}>
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
      <Button
        variant="outline"
        size="icon"
        className="rounded-l-none"
        aria-label={newer ? `Newer snapshot, ${when(newer)}` : "Newer snapshot"}
        title="Newer snapshot of the same host and paths"
        disabled={!newer}
        onClick={() => newer && onSwitch(newer.id)}
      >
        <ChevronRight aria-hidden />
      </Button>
    </div>
  );
}
