import { useQuery } from "@connectrpc/connect-query";
import { useNavigate } from "@tanstack/react-router";
import { ArchiveRestore, FolderDown, ListChecks } from "lucide-react";
import { useState } from "react";

import { ErrorState } from "@/components/app/error-state";
import { FileTable } from "@/components/app/file-table";
import { KeyValueList } from "@/components/app/key-value-list";
import { PageHeader } from "@/components/app/page-header";
import { Panel } from "@/components/app/panel";
import { PathBreadcrumbs } from "@/components/app/path-breadcrumbs";
import { RelativeTime } from "@/components/app/relative-time";
import { ShortId } from "@/components/app/short-id";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import { ResticService } from "@/gen/syncr/v1/restic_pb";
import { usePathListing } from "@/hooks/use-path-listing";
import { formatBytes, formatCount, formatDateTime, formatDuration, tsDate } from "@/lib/format";
import { normalizePath } from "@/lib/paths";
import { isWindowsTree } from "@/lib/restic-paths";

import { DeletingBadge } from "./repositories-page";
import type { RestoreWhat } from "./model";
import { RestoreSheet } from "./restore-sheet";

/**
 * SnapshotBrowserPage walks one snapshot's files, a folder at a time, and
 * starts restores: of ticked items, of the open folder, or of everything.
 */
export function SnapshotBrowserPage({
  repositoryId,
  snapshotId,
  path: searchPath,
}: {
  repositoryId: string;
  snapshotId: string;
  path?: string;
}) {
  const navigate = useNavigate();
  const path = normalizePath(searchPath ?? "/", "snapshot");
  const repoQ = useQuery(ResticService.method.getRepository, { repositoryId });
  const snaps = useQuery(ResticService.method.listSnapshots, {
    repositoryId,
    refresh: false,
    workerId: "",
  });
  const workers = useQuery(WorkerAdminService.method.listWorkers, {});
  const listing = usePathListing({ kind: "snapshot", repositoryId, snapshotId }, path);
  const [selected, setSelected] = useState<string[]>([]);
  const [restore, setRestore] = useState<RestoreWhat | undefined>();

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
  const snapshot = snaps.data?.snapshots.find(
    (s) => s.id === snapshotId || s.id.startsWith(snapshotId),
  );
  const sum = snapshot?.summary;
  const windows = isWindowsTree(snapshot?.paths ?? []);
  const go = (to: string) =>
    void navigate({
      to: "/repositories/$repositoryId/snapshots/$snapshotId",
      params: { repositoryId, snapshotId },
      search: { path: to === "/" ? undefined : to },
    });
  const duration =
    sum?.backupStart && sum.backupEnd
      ? (tsDate(sum.backupEnd)!.getTime() - tsDate(sum.backupStart)!.getTime()) / 1000
      : undefined;
  const locked = repo.deleting;

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
          <Button disabled={locked} onClick={() => setRestore({ mode: "snapshot" })}>
            <ArchiveRestore aria-hidden />
            Restore whole snapshot
          </Button>
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

        <Panel flush className="min-w-0">
          <div className="flex flex-col gap-3 border-b px-4 py-3">
            <PathBreadcrumbs
              path={path}
              kind="snapshot"
              rootLabel="Snapshot"
              driveRoot={windows}
              onNavigate={go}
            />
            <div className="flex flex-wrap items-center gap-2">
              <Button
                variant="outline"
                size="sm"
                disabled={locked || selected.length === 0}
                onClick={() => setRestore({ mode: "items", paths: selected })}
              >
                <ListChecks aria-hidden />
                Restore selected ({selected.length})
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
              {selected.length > 0 && (
                <Button variant="ghost" size="sm" onClick={() => setSelected([])}>
                  Clear selection
                </Button>
              )}
              {listing.worker?.name && (
                <span className="text-xs text-muted-foreground sm:ml-auto">
                  Listed by {listing.worker.name}
                </span>
              )}
            </div>
          </div>
          <FileTable
            key={path}
            aria-label={`Contents of ${path}`}
            entries={listing.entries}
            isLoading={listing.isLoading}
            error={listing.error}
            onRetry={listing.refetch}
            truncated={listing.truncated}
            onOpen={(e) => go(e.path)}
            selection={{ selected, onChange: setSelected, multiple: true }}
          />
        </Panel>
      </div>

      {restore && (
        <RestoreSheet
          open
          onOpenChange={(o) => !o && setRestore(undefined)}
          repository={repo}
          snapshot={snapshot ?? { id: snapshotId }}
          what={restore}
          workers={workers.data?.workers ?? []}
        />
      )}
    </>
  );
}
