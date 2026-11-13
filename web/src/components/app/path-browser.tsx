import { useState } from "react";

import { Button } from "@/components/ui/button";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "@/components/ui/dialog";
import { ProbeEntryType, type ProbeEntry } from "@/gen/syncr/v1/link_pb";
import { usePathListing, type PathSource } from "@/hooks/use-path-listing";
import { normalizePath, parentPath, rootPath } from "@/lib/paths";

import { FileTable } from "./file-table";
import { BrowserToolbar } from "./file-browser/browser-toolbar";

/** PathBrowserDialogProps configures PathBrowserDialog. */
export interface PathBrowserDialogProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  /** source is the place to browse: a worker, a remote or a snapshot. */
  source: PathSource;
  title: string;
  description?: React.ReactNode;
  /** mode says what can be chosen: folders, files or both. */
  mode: "dir" | "file" | "any";
  /** multiple lets several entries be ticked and returned together. */
  multiple?: boolean;
  /** onSelect gets the chosen paths; the dialog then closes. */
  onSelect: (paths: string[]) => void;
  /** initialPath is the folder to open first (default: the top). */
  initialPath?: string;
  /** rootLabel names the top in the breadcrumbs. */
  rootLabel?: string;
}

/**
 * PathBrowserDialog browses folders on a worker, a remote or a snapshot
 * and returns the chosen paths: the forms' path picker. Type a path and
 * press Enter to jump to it, or open folders from the list. In folder mode
 * the footer button chooses the folder that is open. It is a thin wrapper
 * around the file browser's toolbar and table.
 */
export function PathBrowserDialog(props: PathBrowserDialogProps) {
  // The body mounts with the dialog, so every opening starts fresh.
  return (
    <Dialog open={props.open} onOpenChange={props.onOpenChange}>
      <DialogContent className="flex max-h-[calc(100dvh-2rem)] flex-col gap-0 p-0 sm:max-w-2xl">
        {props.open && <Browser {...props} />}
      </DialogContent>
    </Dialog>
  );
}

function Browser({
  onOpenChange,
  source,
  title,
  description,
  mode,
  multiple,
  onSelect,
  initialPath,
  rootLabel,
}: PathBrowserDialogProps) {
  const kind = source.kind;
  const top = rootPath(kind);
  const [path, setPath] = useState(() => (initialPath ? normalizePath(initialPath, kind) : top));
  const [selected, setSelected] = useState<string[]>([]);
  const listing = usePathListing(source, path, { dirsOnly: mode === "dir" });
  // A worker's top lists its allowed folders, which bound the way up.
  const roots = usePathListing(source, top, { dirsOnly: true, enabled: kind === "worker" });
  const rootPaths = kind === "worker" ? (roots.entries ?? []).map((e) => e.path) : [];

  const go = (to: string) => {
    const next = to === top ? top : normalizePath(to, kind);
    setPath(next);
    if (!multiple) setSelected([]);
  };
  const canSelect = (e: ProbeEntry) =>
    mode === "any" || (mode === "dir") === (e.type === ProbeEntryType.DIR);
  // The open folder itself can be chosen, except the list of a worker's folders.
  const here = mode !== "file" && !(kind === "worker" && path === top) ? path : undefined;
  const choice = selected.length > 0 ? selected : here !== undefined ? [here] : [];
  const label =
    choice.length === 0
      ? "Select"
      : choice.length > 1
        ? `Select ${choice.length} items`
        : `Select ${choice[0] || (kind === "remote" ? "the remote's root" : "/")}`;
  const atTop = path === top;
  const names = { worker: "Allowed folders", remote: "Root", snapshot: "Snapshot" } as const;

  return (
    <>
      <DialogHeader className="border-b px-5 py-4">
        <DialogTitle>{title}</DialogTitle>
        <DialogDescription>
          {description ??
            (kind === "worker"
              ? "Only the worker's allowed folders can be opened."
              : "Listed through a worker, so it can take a moment.")}
          {listing.worker?.name && kind !== "worker" && <> Listed by {listing.worker.name}.</>}
        </DialogDescription>
      </DialogHeader>
      <BrowserToolbar
        kind={kind}
        path={path}
        onNavigate={go}
        rootLabel={rootLabel ?? names[kind]}
        roots={rootPaths}
        onRefresh={listing.refetch}
        isFetching={listing.isFetching}
        updatedAt={listing.dataUpdatedAt}
        className="px-5"
      />
      <FileTable
        key={path}
        className="min-h-48 flex-1"
        aria-label={`Contents of ${path || names[kind]}`}
        entries={listing.entries}
        isLoading={listing.isLoading}
        error={listing.error}
        onRetry={listing.refetch}
        truncated={listing.truncated}
        onOpen={(e) => go(e.path)}
        onUp={atTop ? undefined : () => go(parentPath(path, kind, rootPaths))}
        compact
        selection={
          multiple || mode !== "dir"
            ? { selected, onChange: setSelected, multiple, canSelect }
            : undefined
        }
        empty={
          <p className="py-8 text-center text-[13px] text-muted-foreground">
            {mode === "dir" ? "No folders in here." : "This folder is empty."}
          </p>
        }
      />
      <DialogFooter className="items-center border-t px-5 py-3">
        <Button type="button" variant="outline" onClick={() => onOpenChange(false)}>
          Cancel
        </Button>
        <Button
          type="button"
          disabled={choice.length === 0}
          className="max-w-full sm:max-w-[26rem]"
          onClick={() => {
            onSelect(choice);
            onOpenChange(false);
          }}
        >
          <span className="truncate">{label}</span>
        </Button>
      </DialogFooter>
    </>
  );
}
