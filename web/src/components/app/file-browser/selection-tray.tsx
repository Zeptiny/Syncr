import { ClipboardCopy, ListChecks, X } from "lucide-react";
import { useState } from "react";
import { toast } from "sonner";

import { Button } from "@/components/ui/button";
import {
  Sheet,
  SheetContent,
  SheetDescription,
  SheetHeader,
  SheetTitle,
} from "@/components/ui/sheet";
import { ProbeEntryType, type ProbeEntry } from "@/gen/syncr/v1/link_pb";
import { copyText } from "@/lib/clipboard";
import { formatBytes } from "@/lib/format";
import { pathCrumbs, rootPath, type PathKind } from "@/lib/paths";
import { cn } from "@/lib/utils";

import { EntryIcon } from "../entry-icon";
import { sizeText } from "./helpers";
import {
  groupByFolder,
  knownSize,
  pathsText,
  removePaths,
  uncovered,
  type Selection,
} from "./selection";

/** SelectionTrayProps configures SelectionTray. */
export interface SelectionTrayProps {
  kind: PathKind;
  selection: Selection;
  onChange: (selection: Selection) => void;
  /** folderSize is a folder's measured size, counted in the total. */
  folderSize?: (entry: ProbeEntry) => bigint | undefined;
  /** folderSizeAtLeast says a folder's measured size is a lower bound. */
  folderSizeAtLeast?: (entry: ProbeEntry) => boolean;
  /** actions are the page's buttons for the selection. */
  actions?: React.ReactNode;
  /** rootLabel names the top folder in the group headings. */
  rootLabel: string;
  /** mobile shows a bottom bar that opens the tray in a sheet. */
  mobile?: boolean;
  className?: string;
}

/**
 * SelectionTray shows what is selected across folders, grouped by folder,
 * with remove buttons, the known total size, "Copy paths" (one per line)
 * and Clear, plus the page's actions. Items inside a selected folder are
 * listed as "in <folder>": actions leave them to their folder. At phone
 * width it is a bottom bar that opens a sheet.
 */
export function SelectionTray(props: SelectionTrayProps) {
  const { selection, kind, folderSize, folderSizeAtLeast, mobile } = props;
  const [open, setOpen] = useState(false);
  if (selection.size === 0) return null;
  const size = knownSize(selection, kind, folderSize, folderSizeAtLeast);
  const summary = `${selection.size.toLocaleString("en-US")} selected`;

  if (mobile)
    return (
      <>
        {/* Room for the bar, so it never hides the last rows. */}
        <div aria-hidden className="h-16" />
        <div
          role="region"
          aria-label="Selection"
          className="fixed inset-x-0 bottom-0 z-40 flex items-center gap-3 border-t bg-card px-4 py-3 shadow-[0_-4px_12px_rgb(0_0_0/0.08)]"
        >
          <div className="flex min-w-0 flex-1 flex-col">
            <span className="text-[13px] font-semibold">{summary}</span>
            <span className="tabular truncate text-xs text-muted-foreground">{sizeText(size)}</span>
          </div>
          <Button size="sm" onClick={() => setOpen(true)}>
            <ListChecks aria-hidden />
            Show selection
          </Button>
        </div>
        <Sheet open={open} onOpenChange={setOpen}>
          <SheetContent side="bottom" className="max-h-[85dvh] gap-0 p-0">
            <SheetHeader className="border-b px-4 py-3">
              <SheetTitle>{summary}</SheetTitle>
              <SheetDescription className="tabular">{sizeText(size)}</SheetDescription>
            </SheetHeader>
            <TrayBody {...props} onEmptied={() => setOpen(false)} />
          </SheetContent>
        </Sheet>
      </>
    );

  return (
    <section
      aria-label="Selection"
      className={cn("flex min-h-0 flex-col rounded-lg border bg-card", props.className)}
    >
      <header className="flex flex-wrap items-baseline justify-between gap-x-3 gap-y-1 border-b px-4 py-3">
        <h2 className="text-[14px] font-semibold">{summary}</h2>
        <span className="tabular text-xs text-muted-foreground">{sizeText(size)}</span>
      </header>
      <TrayBody {...props} />
    </section>
  );
}

function TrayBody({
  kind,
  selection,
  onChange,
  folderSize,
  folderSizeAtLeast,
  actions,
  rootLabel,
  onEmptied,
}: SelectionTrayProps & { onEmptied?: () => void }) {
  const groups = groupByFolder(selection, kind);
  const change = (s: Selection) => {
    onChange(s);
    if (s.size === 0) onEmptied?.();
  };
  const folderLabel = (folder: string) =>
    folder === rootPath(kind) ? rootLabel : (pathCrumbs(folder, kind).at(-1)?.label ?? folder);
  const copy = () => {
    const n = uncovered(selection, kind).length;
    copyText(pathsText(selection, kind)).then(
      () => toast.success(n === 1 ? "Copied 1 path" : `Copied ${n} paths`),
      () => toast.error("Couldn't copy to the clipboard"),
    );
  };

  return (
    <>
      <div className="flex flex-wrap items-center gap-2 border-b px-4 py-2.5">
        {actions}
        <Button variant="outline" size="sm" onClick={copy}>
          <ClipboardCopy aria-hidden />
          Copy paths
        </Button>
        <Button variant="ghost" size="sm" onClick={() => change(new Map())}>
          Clear
        </Button>
      </div>
      <ul className="min-h-0 flex-1 overflow-y-auto py-1" aria-label="Selected items">
        {groups.map((g) => (
          <li key={g.folder} className="py-1">
            <span
              className="block truncate px-4 py-1 font-mono text-[11.5px] text-muted-foreground"
              title={g.folder || rootLabel}
            >
              {g.folder === rootPath(kind) ? rootLabel : g.folder}
            </span>
            <ul>
              {g.items.map((it) => {
                const dir = it.entry.type === ProbeEntryType.DIR;
                const measured = dir ? folderSize?.(it.entry) : undefined;
                return (
                  <li
                    key={it.path}
                    className={cn(
                      "flex items-center gap-2 px-4 py-1 text-[13px]",
                      it.coveredBy !== undefined && "text-muted-foreground",
                    )}
                  >
                    <EntryIcon type={it.entry.type} />
                    <span className="flex min-w-0 flex-1 flex-col">
                      <span className="truncate" title={it.path}>
                        {it.entry.name}
                      </span>
                      {it.coveredBy !== undefined && (
                        <span className="truncate text-xs">in {folderLabel(it.coveredBy)}</span>
                      )}
                    </span>
                    {/* An unmeasured folder shows no size: a dash beside the
                        remove button would read as a minus button. */}
                    {(!dir || measured !== undefined) && (
                      <span className="tabular shrink-0 text-xs text-muted-foreground">
                        {dir && measured !== undefined
                          ? `${folderSizeAtLeast?.(it.entry) ? "≥ " : ""}${formatBytes(measured)}`
                          : formatBytes(it.entry.size)}
                      </span>
                    )}
                    <Button
                      variant="ghost"
                      size="icon-xs"
                      aria-label={`Remove ${it.entry.name}`}
                      title="Remove from the selection"
                      onClick={() => change(removePaths(selection, [it.path]))}
                    >
                      <X aria-hidden />
                    </Button>
                  </li>
                );
              })}
            </ul>
          </li>
        ))}
      </ul>
    </>
  );
}
