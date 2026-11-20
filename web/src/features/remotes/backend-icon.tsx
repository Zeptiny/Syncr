import {
  ArrowDownUp,
  Box,
  Cloud,
  CloudCog,
  Combine,
  Container,
  Database,
  Globe,
  HardDrive,
  LockKeyhole,
  Network,
  Terminal,
  type LucideIcon,
} from "lucide-react";

import { cn } from "@/lib/utils";

const icons: Record<string, LucideIcon> = {
  s3: Database,
  azureblob: Container,
  b2: Cloud,
  crypt: LockKeyhole,
  ftp: ArrowDownUp,
  gcs: CloudCog,
  sftp: Terminal,
  smb: Network,
  local: HardDrive,
  union: Combine,
  webdav: Globe,
};

/** backendIcon is the glyph of a backend type: a generic box for a type it does not know. */
export function backendIcon(id: string): LucideIcon {
  return icons[id] ?? Box;
}

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
