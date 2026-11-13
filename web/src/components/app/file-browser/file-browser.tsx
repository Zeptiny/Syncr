import { useEffect, useMemo, useRef, useState } from "react";
import { MousePointerClick } from "lucide-react";

import type { Worker } from "@/gen/syncr/v1/api_pb";
import type { ProbeEntry } from "@/gen/syncr/v1/link_pb";
import type { FolderMeasure } from "@/hooks/use-folder-measure";
import { usePathListing, type PathSource } from "@/hooks/use-path-listing";
import { useIsMobile } from "@/hooks/use-mobile";
import { useNow } from "@/hooks/use-now";
import { formatRelative } from "@/lib/format";
import { normalizePath, parentPath, rootPath } from "@/lib/paths";
import { cn } from "@/lib/utils";

import { EmptyState } from "../empty-state";
import { FileTable, type RowMenuItem } from "../file-table";
import { BrowserToolbar } from "./browser-toolbar";
import { isMissingFolder, missingFolderHint } from "./helpers";
import { MeasureControl } from "./measure-control";
import {
  commonParent,
  knownSize,
  uncovered,
  withPaths,
  type KnownSize,
  type SelectedItem,
  type Selection,
} from "./selection";
import { SelectionTray } from "./selection-tray";
import { SizeCell } from "./size-cell";
import { WorkerChooser } from "./worker-chooser";

/** SelectionContext is what a page's selection actions act on. */
export interface SelectionContext {
  selection: Selection;
  /** items are the selected items no selected folder holds, in selection order. */
  items: SelectedItem[];
  /** parent is the deepest folder holding every item (undefined: none, e.g. two drives). */
  parent: string | undefined;
  size: KnownSize;
  clear: () => void;
}

/** WorkerChoice offers the worker that lists a remote or a snapshot. */
export interface WorkerChoice {
  workers: Worker[];
  /** op is the probe operation listing needs: "list_remote" or "restic_ls". */
  op: string;
  onChange: (workerId: string) => void;
  /** required: there is no automatic choice (a remote on a worker's own disk). */
  required?: boolean;
}

/** FileBrowserProps configures FileBrowser. */
export interface FileBrowserProps {
  /** source is what is browsed; its workerId is the chosen listing worker. */
  source: PathSource;
  /** path is the open folder, as the page's URL has it. */
  path: string;
  onNavigate: (path: string) => void;
  /** selection is controlled; see useBrowserSelection. */
  selection: Selection;
  onSelectionChange: (selection: Selection) => void;
  /** rootLabel names the top: "Allowed folders", the remote's name, "Snapshot". */
  rootLabel: string;
  /** driveRoot shows a Windows snapshot's first step as a drive. */
  driveRoot?: boolean;
  /** canSelect limits which entries can be ticked (default all). */
  canSelect?: (entry: ProbeEntry) => boolean;
  /** selectionActions renders the page's buttons for the selection, in the tray. */
  selectionActions?: (ctx: SelectionContext) => React.ReactNode;
  /** rowMenu returns a row's menu items (see copyPathItem). */
  rowMenu?: (entry: ProbeEntry) => RowMenuItem[];
  /** folderSize is a folder's measured size: Size sort and the tray's total. */
  folderSize?: (entry: ProbeEntry) => bigint | undefined;
  /** renderFolderSize renders a folder's Size cell (default "–", or the measured size). */
  renderFolderSize?: (entry: ProbeEntry) => React.ReactNode;
  /**
   * measure adds "Calculate sizes" (useFolderMeasure): the toolbar's
   * button and total, the folders' Size cells, the Size sort and the
   * tray's total. Refresh forgets the open folder's measurement.
   */
  measure?: FolderMeasure;
  /** workerChoice adds "by <worker> ▾" to the toolbar. */
  workerChoice?: WorkerChoice;
  /**
   * waitingForWorker, when set, is shown instead of the listing until a
   * worker is chosen (no listing is asked for meanwhile).
   */
  waitingForWorker?: string;
  /** toolbarActions are the page's buttons for the open folder. */
  toolbarActions?: React.ReactNode;
  /** toolbarExtra sits on the toolbar's right: usage, "Calculate sizes". */
  toolbarExtra?: React.ReactNode;
  className?: string;
}

/**
 * FileBrowser browses one source a folder at a time: the toolbar (Up,
 * path, breadcrumbs, Refresh, when and by whom it was listed), the table
 * and the selection tray. It knows nothing about jobs or restores: the
 * page supplies the selection's actions and the rows' menu items.
 */
export function FileBrowser({
  source,
  path,
  onNavigate,
  selection,
  onSelectionChange,
  rootLabel,
  driveRoot,
  canSelect,
  selectionActions,
  rowMenu,
  folderSize,
  renderFolderSize,
  measure,
  workerChoice,
  waitingForWorker,
  toolbarActions,
  toolbarExtra,
  className,
}: FileBrowserProps) {
  const kind = source.kind;
  const top = rootPath(kind);
  const mobile = useIsMobile();
  const waiting = waitingForWorker !== undefined;
  const listing = usePathListing(source, path, { enabled: !waiting });
  // A worker's top lists its allowed folders, which bound the way up.
  const roots = usePathListing(source, top, { enabled: kind === "worker" && !waiting });
  const rootPaths = useMemo(
    () => (kind === "worker" ? (roots.entries ?? []).map((e) => e.path) : []),
    [kind, roots.entries],
  );

  // Keyboard moves between folders keep the focus in the table, on the
  // folder just left when going up.
  const [focus, setFocus] = useState<{ path: string; from?: string } | undefined>();
  const go = (to: string, fromTable: boolean) => {
    setFocus(fromTable ? { path: to, from: path } : undefined);
    onNavigate(to);
  };
  const up = () => path !== top && go(parentPath(path, kind, rootPaths), true);

  // "Go to the nearest parent" climbs until a folder lists.
  const [climbing, setClimbing] = useState(false);
  const missing = isMissingFolder(listing.error);
  if (climbing && !listing.isLoading && (!missing || path === top)) setClimbing(false);
  const navigate = useRef(onNavigate);
  useEffect(() => {
    navigate.current = onNavigate;
  });
  useEffect(() => {
    if (climbing && missing && !listing.isLoading && path !== top)
      navigate.current(parentPath(path, kind, rootPaths));
  }, [climbing, missing, listing.isLoading, path, top, kind, rootPaths]);

  // Measured sizes, unless the page brings its own.
  const now = useNow(30_000, !!measure && measure.folders.size > 0);
  const ago = (at: number) => formatRelative(new Date(at), now.getTime() < at ? new Date(at) : now);
  const sizeOf = folderSize ?? measure?.folderSize;
  const sizeAtLeast = folderSize ? undefined : measure?.folderAtLeast;
  const sizeCell =
    renderFolderSize ??
    (measure
      ? (e: ProbeEntry) => {
          const f = measure.folders.get(normalizePath(e.path, kind));
          if (!f) return undefined;
          return (
            <SizeCell
              bytes={f.bytes}
              files={f.files}
              dirs={f.dirs}
              total={measure.child(e) ? measure.result?.measurement.bytes : undefined}
              atLeast={f.atLeast}
              measured={`measured ${ago(f.measuredAt)}`}
            />
          );
        }
      : undefined);

  const ctx: SelectionContext = {
    selection,
    items: uncovered(selection, kind),
    parent: commonParent(
      uncovered(selection, kind).map((i) => i.path),
      kind,
    ),
    size: knownSize(selection, kind, sizeOf, sizeAtLeast),
    clear: () => onSelectionChange(new Map()),
  };

  const tray = (
    <SelectionTray
      kind={kind}
      selection={selection}
      onChange={onSelectionChange}
      folderSize={sizeOf}
      folderSizeAtLeast={sizeAtLeast}
      rootLabel={rootLabel}
      actions={selectionActions?.(ctx)}
      mobile={mobile}
      className="@4xl:sticky @4xl:top-4 @4xl:max-h-[calc(100dvh-2rem)]"
    />
  );

  return (
    <div className={cn("@container", className)}>
      <div className="grid items-start gap-4 @4xl:grid-cols-[minmax(0,1fr)_17rem]">
        <section
          aria-label="Files"
          className="min-w-0 rounded-lg border bg-card text-card-foreground"
        >
          <BrowserToolbar
            kind={kind}
            path={path}
            onNavigate={(to) => go(to, false)}
            rootLabel={rootLabel}
            roots={rootPaths}
            driveRoot={driveRoot}
            onRefresh={
              waiting
                ? undefined
                : () => {
                    measure?.forget();
                    listing.refetch();
                  }
            }
            isFetching={listing.isFetching}
            updatedAt={listing.dataUpdatedAt}
            listedBy={
              workerChoice ? (
                <WorkerChooser
                  workers={workerChoice.workers}
                  op={workerChoice.op}
                  value={source.kind === "worker" ? source.workerId : (source.workerId ?? "")}
                  onChange={workerChoice.onChange}
                  listedBy={listing.worker}
                  required={workerChoice.required}
                />
              ) : undefined
            }
            actions={toolbarActions}
            extra={
              measure && !waiting ? (
                <>
                  <MeasureControl
                    measure={measure}
                    measuredAgo={measure.result ? ago(measure.result.measuredAt) : undefined}
                  />
                  {toolbarExtra}
                </>
              ) : (
                toolbarExtra
              )
            }
          />
          {waiting ? (
            <EmptyState icon={MousePointerClick} title={waitingForWorker} className="py-12" />
          ) : (
            <FileTable
              key={path}
              aria-label={`Contents of ${path || rootLabel}`}
              entries={listing.entries}
              isLoading={listing.isLoading}
              error={listing.error}
              onRetry={listing.refetch}
              truncated={listing.truncated}
              onOpen={(e) => go(e.path, true)}
              onUp={path === top ? undefined : up}
              selection={{
                selected: [...selection.keys()],
                onChange: (paths) =>
                  onSelectionChange(withPaths(selection, paths, listing.entries ?? [])),
                multiple: true,
                canSelect,
              }}
              rowMenu={rowMenu}
              folderSize={sizeOf}
              renderFolderSize={sizeCell}
              missingHint={missingFolderHint(kind)}
              onGoToParent={path === top ? undefined : () => setClimbing(true)}
              autoFocus={focus?.path === path}
              focusPath={focus?.path === path ? focus.from : undefined}
            />
          )}
        </section>
        {selection.size > 0 ? (
          tray
        ) : (
          <aside className="hidden rounded-lg border border-dashed px-4 py-5 text-[13px] text-muted-foreground @4xl:block">
            Tick files and folders to select them. The selection stays while you move between
            folders.
          </aside>
        )}
      </div>
    </div>
  );
}
