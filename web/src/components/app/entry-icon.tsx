import { File, FileQuestion, Folder, Link2 } from "lucide-react";

import { ProbeEntryType } from "@/gen/syncr/v1/link_pb";
import { cn } from "@/lib/utils";

/** EntryIcon shows what a listed entry is: a folder, a file, a link or something else. */
export function EntryIcon({ type, className }: { type: ProbeEntryType; className?: string }) {
  const cls = cn("size-4 shrink-0", className);
  switch (type) {
    case ProbeEntryType.DIR:
      return <Folder className={cn(cls, "text-primary")} aria-label="Folder" />;
    case ProbeEntryType.SYMLINK:
      return <Link2 className={cn(cls, "text-muted-foreground")} aria-label="Link" />;
    case ProbeEntryType.FILE:
      return <File className={cn(cls, "text-muted-foreground")} aria-label="File" />;
    default:
      return <FileQuestion className={cn(cls, "text-muted-foreground")} aria-label="Other" />;
  }
}
