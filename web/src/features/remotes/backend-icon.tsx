import {
  Box,
  Cloud,
  Combine,
  Database,
  Globe,
  HardDrive,
  Terminal,
  type LucideIcon,
} from "lucide-react";

import { cn } from "@/lib/utils";

const icons: Record<string, LucideIcon> = {
  s3: Database,
  b2: Cloud,
  sftp: Terminal,
  local: HardDrive,
  union: Combine,
  webdav: Globe,
};

/** BackendIcon is the glyph of a backend type, in a tinted square. */
export function BackendIcon({ id, className }: { id: string; className?: string }) {
  const Icon = icons[id] ?? Box;
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
