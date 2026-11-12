import {
  ArrowRightLeft,
  Archive,
  Scissors,
  ShieldCheck,
  Sparkles,
  type LucideIcon,
} from "lucide-react";

import { ChoiceCards } from "./choice-cards";

/** KindOption is one job kind a form can create. */
export interface KindOption {
  kind: string;
  label: string;
  help: string;
  icon: LucideIcon;
}

/** kindOptions are the kinds a user can start or schedule, in display order. */
const kindOptions: KindOption[] = [
  {
    kind: "rclone_transfer",
    label: "Transfer",
    help: "Copy, sync or move files between two places.",
    icon: ArrowRightLeft,
  },
  {
    kind: "restic_backup",
    label: "Backup",
    help: "Save folders into a repository as a new snapshot.",
    icon: Archive,
  },
  {
    kind: "restic_forget",
    label: "Retention",
    help: "Remove old snapshots, keeping the ones your rules name.",
    icon: Scissors,
  },
  {
    kind: "restic_check",
    label: "Check",
    help: "Verify that a repository is intact and readable.",
    icon: ShieldCheck,
  },
  {
    kind: "restic_prune",
    label: "Prune",
    help: "Reclaim the space of data no snapshot uses.",
    icon: Sparkles,
  },
];

/**
 * KindPicker chooses what a job does. Only the kinds a user may start are
 * offered: restores start from a snapshot, and deleting a repository's data
 * starts from the repository.
 */
export function KindPicker({
  value,
  onChange,
  kinds,
  labelId = "job-kind",
}: {
  value: string;
  onChange: (kind: string) => void;
  /** kinds limits the offered kinds (default: all five). */
  kinds?: string[];
  labelId?: string;
}) {
  const offered = kindOptions.filter((o) => !kinds || kinds.includes(o.kind));
  return (
    <ChoiceCards
      label="Kind of job"
      labelId={labelId}
      value={value}
      onChange={onChange}
      className="sm:grid-cols-2 lg:grid-cols-5"
      options={offered.map((o) => ({
        value: o.kind,
        label: (
          <span className="inline-flex items-center gap-1.5">
            <o.icon className="size-3.5 text-muted-foreground" aria-hidden />
            {o.label}
          </span>
        ),
        help: o.help,
      }))}
    />
  );
}
