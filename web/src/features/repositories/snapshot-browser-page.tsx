import { useQuery } from "@connectrpc/connect-query";
import { Link, useNavigate } from "@tanstack/react-router";
import {
  ArchiveRestore,
  FolderDown,
  GitCompareArrows,
  Layers,
  ListChecks,
  Search,
  Undo2,
} from "lucide-react";
import { useState } from "react";

import { ErrorState } from "@/components/app/error-state";
import { FileBrowser } from "@/components/app/file-browser/file-browser";
import { copyPathItem } from "@/components/app/file-browser/helpers";
import type { RowMenuItem } from "@/components/app/file-table";
import { useBrowserSelection } from "@/components/app/file-browser/use-selection";
import { KeyValueList } from "@/components/app/key-value-list";
import { PageHeader } from "@/components/app/page-header";
import { Panel } from "@/components/app/panel";
import { RelativeTime } from "@/components/app/relative-time";
import { ShortId } from "@/components/app/short-id";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import { ProbeEntryType, type ProbeEntry, type ResticSnapshot } from "@/gen/syncr/v1/link_pb";
import { ResticService } from "@/gen/syncr/v1/restic_pb";
import { useFolderMeasure } from "@/hooks/use-folder-measure";
import { sourceKey, type PathSource } from "@/hooks/use-path-listing";
import { formatBytes, formatCount, formatDateTime, formatDuration, tsDate } from "@/lib/format";
import { normalizePath } from "@/lib/paths";
import { isWindowsTree } from "@/lib/restic-paths";

import { DeletingBadge } from "./repositories-page";
import { FileVersionsSheet } from "./file-versions-sheet";
import { isLocalRepository, type RestoreWhat } from "./model";
import { RestoreSheet, type RestoreSheetProps } from "./restore-sheet";
import { runScan } from "./scan-run";
import { olderFirst } from "./scans";
import type { SnapshotSearch } from "./search";
import { findSnapshot } from "./snapshot-groups";
import { SnapshotMenu } from "./snapshot-menu";
import { SnapshotSwitcher } from "./snapshot-switcher";

/**
 * SnapshotBrowserPage walks one snapshot's files, a folder at a time, and
 * starts restores: of ticked items, of the open folder, or of everything,
 * and of items back to their original location (each row's menu and the
 * selection). The switcher moves to another snapshot and keeps the open
 * folder.
 */
export function SnapshotBrowserPage({
  repositoryId,
  snapshotId,
  path: searchPath,
  worker,
}: {
  repositoryId: string;
  snapshotId: string;
} & SnapshotSearch) {
  const navigate = useNavigate();
  const path = normalizePath(searchPath ?? "/", "snapshot");
  const repoQ = useQuery(ResticService.method.getRepository, { repositoryId });
  const snaps = useQuery(ResticService.method.listSnapshots, {
    repositoryId,
    refresh: false,
    workerId: "",
  });
  const workers = useQuery(WorkerAdminService.method.listWorkers, {});
  const source: PathSource = { kind: "snapshot", repositoryId, snapshotId, workerId: worker };
  const [selection, setSelection] = useBrowserSelection(sourceKey(source));
  // A restore takes from this snapshot, or from another one a version is in.
  const [restoring, setRestoring] = useState<
    { what: RestoreWhat; from?: RestoreSheetProps["snapshot"] } | undefined
  >();
  const setRestore = (what: RestoreWhat | undefined) => setRestoring(what ? { what } : undefined);
  const [versionsOf, setVersionsOf] = useState<ProbeEntry | undefined>();
  const waiting = isLocalRepository(repoQ.data?.repository) && !worker;
  const measure = useFolderMeasure(source, path, { enabled: !!repoQ.data && !waiting });

  if (repoQ.error) return <ErrorState error={repoQ.error} onRetry={() => void repoQ.refetch()} />;
  const repo = repoQ.data?.repository;
  if (!repo) {
    return (
      <div className="flex flex-col gap-4">
        <Skeleton className="h-8 w-72" />
        <Skeleton className="h-96 w-full" />
      </div>
    );
  }
  const snapshot = findSnapshot(snaps.data?.snapshots ?? [], snapshotId);
  const sum = snapshot?.summary;
  const windows = isWindowsTree(snapshot?.paths ?? []);
  const open = (id: string, to: string, via = worker) =>
    void navigate({
      to: "/repositories/$repositoryId/snapshots/$snapshotId",
      params: { repositoryId, snapshotId: id },
      search: { path: to === "/" ? undefined : to, worker: via },
    });
  const go = (to: string) => open(snapshotId, to);
  const local = isLocalRepository(repo);
  const duration =
    sum?.backupStart && sum.backupEnd
      ? (tsDate(sum.backupEnd)!.getTime() - tsDate(sum.backupStart)!.getTime()) / 1000
      : undefined;
  const locked = repo.deleting;
  // Each row's menu: restore it, put it back where it was, a file's
  // versions in every snapshot, copy its path.
  const rowMenu = (e: ProbeEntry): RowMenuItem[] => [
    {
      label: "Restore…",
      icon: ArchiveRestore,
      disabled: locked,
      onSelect: () => setRestore({ mode: "items", paths: [e.path] }),
    },
    {
      label: "Restore to original location…",
      icon: Undo2,
      disabled: locked,
      onSelect: () => setRestore({ mode: "original", paths: [e.path] }),
    },
    // Versions of files and links only: a folder's mean little, and its
    // whole subtree would use up the search's matches.
    ...(e.type === ProbeEntryType.DIR
      ? [{ ...copyPathItem(e), separated: true }]
      : [
          {
            label: "Show versions",
            icon: Layers,
            separated: true,
            onSelect: () => setVersionsOf(e),
          },
          copyPathItem(e),
        ]),
  ];
  const list = snaps.data?.snapshots ?? [];
  // "Compare with…": the older of the two is "from"; the open folder stays.
  const compareWith = (other: ResticSnapshot) => {
    const { from, to } = olderFirst(other, snapshot ?? other);
    void navigate({
      to: "/repositories/$repositoryId/compare",
      params: { repositoryId },
      search: { from: from.id, to: to.id, path: path === "/" ? undefined : path, worker },
      state: runScan,
    });
  };

  return (
    <>
      <PageHeader
        back={{ to: "/repositories", label: "Repositories" }}
        title={
          <span className="flex flex-wrap items-center gap-3">
            Snapshot {snapshotId.slice(0, 8)}
            {locked && <DeletingBadge />}
          </span>
        }
        meta={
          <span className="text-[13px] text-muted-foreground">
            <button
              type="button"
              className="font-medium text-primary hover:underline"
              onClick={() =>
                void navigate({ to: "/repositories/$repositoryId", params: { repositoryId } })
              }
            >
              {repo.name}
            </button>
            {snapshot?.time && (
              <>
                , taken <RelativeTime date={tsDate(snapshot.time)} />
              </>
            )}
          </span>
        }
        actions={
          <>
            <SnapshotSwitcher
              snapshots={snaps.data?.snapshots ?? []}
              current={snapshot}
              onSwitch={(id) => open(id, path)}
            />
            <SnapshotMenu
              snapshots={list}
              value=""
              onChange={(id) => {
                const other = list.find((s) => s.id === id);
                if (other) compareWith(other);
              }}
              label="Compare with"
              near={snapshot}
              exclude={snapshot?.id}
              disabled={!snapshot}
              trigger={
                <>
                  <GitCompareArrows aria-hidden />
                  Compare with…
                </>
              }
            />
            <Button variant="outline" asChild>
              <Link
                to="/repositories/$repositoryId/search"
                params={{ repositoryId }}
                search={{ worker }}
              >
                <Search aria-hidden />
                Search
              </Link>
            </Button>
            <Button disabled={locked} onClick={() => setRestore({ mode: "snapshot" })}>
              <ArchiveRestore aria-hidden />
              Restore whole snapshot
            </Button>
          </>
        }
      />
      <div className="grid items-start gap-6 lg:grid-cols-[19rem_minmax(0,1fr)]">
        <Panel title="Snapshot">
          {snaps.isPending ? (
            <Skeleton className="h-40 w-full" />
          ) : (
            <KeyValueList
              className="sm:grid-cols-[6.5rem_1fr]"
              items={[
                { label: "ID", value: <ShortId id={snapshot?.id ?? snapshotId} copy /> },
                {
                  label: "Time",
                  value: snapshot?.time ? formatDateTime(tsDate(snapshot.time)) : "–",
                },
                { label: "Host", value: snapshot?.host || "–" },
                { label: "User", value: snapshot?.username || "–" },
                {
                  label: "Paths",
                  value: snapshot?.paths.length ? (
                    <span className="flex flex-col gap-0.5">
                      {snapshot.paths.map((p) => (
                        <span key={p} className="break-all">
                          {p}
                        </span>
                      ))}
                    </span>
                  ) : (
                    "–"
                  ),
                  mono: true,
                },
                { label: "Tags", value: snapshot?.tags.join(", ") || "–" },
                ...(sum
                  ? [
                      { label: "Files", value: formatCount(sum.totalFilesProcessed) },
                      { label: "Size", value: formatBytes(sum.totalBytesProcessed) },
                      { label: "Data added", value: formatBytes(sum.dataAddedPackedBytes) },
                      {
                        label: "Duration",
                        value: duration === undefined ? "–" : formatDuration(duration),
                      },
                    ]
                  : []),
              ]}
            />
          )}
          {!snaps.isPending && !snapshot && (
            <p className="mt-3 text-xs leading-5 text-muted-foreground">
              This snapshot is not in the last listing. Refresh the snapshots on the repository's
              page to see its details.
            </p>
          )}
        </Panel>

        <FileBrowser
          className="min-w-0"
          source={source}
          path={path}
          onNavigate={go}
          selection={selection}
          onSelectionChange={setSelection}
          rootLabel="Snapshot"
          driveRoot={windows}
          rowMenu={rowMenu}
          measure={measure}
          selectionActions={(ctx) => (
            <Button
              size="sm"
              variant="outline"
              disabled={locked}
              onClick={() => setRestore({ mode: "original", paths: ctx.items.map((i) => i.path) })}
            >
              <Undo2 aria-hidden />
              Restore to original location…
            </Button>
          )}
          workerChoice={{
            workers: workers.data?.workers ?? [],
            op: "restic_ls",
            required: local,
            onChange: (id) => open(snapshotId, path, id || undefined),
          }}
          waitingForWorker={
            local && !worker
              ? "This repository is on a worker's disk: choose that worker above to list the snapshot."
              : undefined
          }
          toolbarActions={(ctx) => (
            <>
              <Button
                variant="outline"
                size="sm"
                disabled={locked || selection.size === 0}
                onClick={() => setRestore({ mode: "items", paths: ctx.items.map((i) => i.path) })}
              >
                <ListChecks aria-hidden />
                Restore selected ({selection.size})
              </Button>
              <Button
                variant="outline"
                size="sm"
                disabled={locked || path === "/"}
                onClick={() => setRestore({ mode: "folder", folder: path })}
              >
                <FolderDown aria-hidden />
                Restore this folder…
              </Button>
            </>
          )}
        />
      </div>

      <FileVersionsSheet
        entry={versionsOf}
        onClose={() => setVersionsOf(undefined)}
        repositoryId={repositoryId}
        snapshots={list}
        workerId={worker}
        locked={locked}
        onOpen={(id, folder) => {
          setVersionsOf(undefined);
          open(id, folder);
        }}
        onRestore={(id, p) => {
          setVersionsOf(undefined);
          setRestoring({
            what: { mode: "items", paths: [p] },
            from: list.find((s) => s.id === id) ?? { id },
          });
        }}
      />
      {restoring && (
        <RestoreSheet
          open
          onOpenChange={(o) => !o && setRestore(undefined)}
          repository={repo}
          snapshot={restoring.from ?? snapshot ?? { id: snapshotId }}
          what={restoring.what}
          workers={workers.data?.workers ?? []}
        />
      )}
    </>
  );
}
