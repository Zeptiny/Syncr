import {
  ArrowRightLeft,
  Archive,
  CalendarClock,
  FolderSync,
  Scissors,
  ShieldCheck,
  Sparkles,
  type LucideIcon,
} from "lucide-react";

/** kindIcons are the icons of the job kinds a user can start or schedule. */
export const kindIcons: Record<string, LucideIcon> = {
  rclone_transfer: ArrowRightLeft,
  rclone_bisync: FolderSync,
  restic_backup: Archive,
  restic_forget: Scissors,
  restic_check: ShieldCheck,
  restic_prune: Sparkles,
};

/** otherKindIcon stands for a kind without an icon of its own. */
export const otherKindIcon: LucideIcon = CalendarClock;
