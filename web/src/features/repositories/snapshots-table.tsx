import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { createColumnHelper } from "@tanstack/react-table";
import { Camera, RefreshCw } from "lucide-react";
import { useEffect, useMemo, useRef, useState } from "react";

import { DataTable } from "@/components/app/data-table";
import { EmptyState } from "@/components/app/empty-state";
import { RelativeTime } from "@/components/app/relative-time";
import { ShortId } from "@/components/app/short-id";
import { StatusPill } from "@/components/app/status-badge";
import { WorkerPicker } from "@/components/app/worker-picker";
import { Button } from "@/components/ui/button";
import type { Worker } from "@/gen/syncr/v1/api_pb";
import type { ResticSnapshot } from "@/gen/syncr/v1/link_pb";
import {
  ResticService,
  type ListSnapshotsResponse,
  type Repository,
} from "@/gen/syncr/v1/restic_pb";
import { errorMessage } from "@/lib/errors";
import { formatBytes, formatCount, formatDateTime, tsDate } from "@/lib/format";
import { invalidateService } from "@/lib/invalidate";
import { cn } from "@/lib/utils";

const col = createColumnHelper<ResticSnapshot>();
const PAGE = 100;

/** SnapshotsTableProps configures SnapshotsTable. */
export interface SnapshotsTableProps {
  repository: Repository;
  /** workers are offered as the one that lists the snapshots. */
  workers: Worker[];
}

/**
 * SnapshotsTable lists a repository's snapshots from the controller's
 * cache and refreshes it through a worker: once by itself when the cache
 * is out of date, and whenever Refresh is pressed. Rows open the snapshot
 * browser. While the repository's data is being deleted the cache can
 * still be read, but not refreshed.
 */
export function SnapshotsTable({ repository, workers }: SnapshotsTableProps) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [workerId, setWorkerId] = useState("");
  const [shown, setShown] = useState(PAGE);
  const cached = useQuery(ResticService.method.listSnapshots, {
    repositoryId: repository.id,
    refresh: false,
    workerId: "",
  });
  const refresh = useMutation(ResticService.method.listSnapshots);
  // The answer of the last refresh replaces the cached list on screen.
  const [fresh, setFresh] = useState<ListSnapshotsResponse | undefined>();
  const data = fresh ?? cached.data;
  const canRefresh = !repository.deleting;

  const { mutateAsync } = refresh;
  const doRefresh = useMemo(
    () => async (worker: string) => {
      try {
        const res = await mutateAsync({
          repositoryId: repository.id,
          refresh: true,
          workerId: worker,
        });
        setFresh(res);
        // The repository's counts and "latest snapshot" come from the same cache.
        await invalidateService(queryClient, ResticService);
      } catch {
        // The mutation keeps the error; it is shown above the table.
      }
    },
    [mutateAsync, repository.id, queryClient],
  );

  // An out-of-date cache is refreshed once when the list opens.
  const auto = useRef(false);
  const stale = cached.data?.stale ?? false;
  useEffect(() => {
    if (!stale || auto.current || !canRefresh) return;
    auto.current = true;
    void doRefresh("");
  }, [stale, canRefresh, doRefresh]);

  const columns = useMemo(
    () => [
      col.accessor((s) => tsDate(s.time), {
        id: "time",
        header: "Time",
        cell: (c) => (
          <div className="flex flex-col">
            <span className="text-[13px] whitespace-nowrap">{formatDateTime(c.getValue())}</span>
            <RelativeTime date={c.getValue()} className="text-xs text-muted-foreground" />
          </div>
        ),
      }),
      col.accessor("id", {
        header: "ID",
        cell: (c) => <ShortId id={c.getValue()} copy />,
      }),
      col.accessor("host", {
        header: "Host",
        cell: (c) => <span className="text-[13px]">{c.getValue() || "–"}</span>,
      }),
      col.display({
        id: "paths",
        header: "Paths",
        cell: ({ row: { original: s } }) => (
          <span
            className="block max-w-[16rem] truncate font-mono text-[12.5px]"
            title={s.paths.join("\n")}
          >
            {s.paths.join(", ") || "–"}
          </span>
        ),
      }),
      col.display({
        id: "tags",
        header: "Tags",
        cell: ({ row: { original: s } }) =>
          s.tags.length ? (
            <span className="inline-flex flex-wrap gap-1">
              {s.tags.map((t) => (
                <span
                  key={t}
                  className="rounded bg-secondary px-1.5 py-0.5 font-mono text-[11.5px] text-secondary-foreground"
                >
                  {t}
                </span>
              ))}
            </span>
          ) : (
            <span className="text-muted-foreground">–</span>
          ),
      }),
      col.display({
        id: "files",
        header: "Files",
        cell: ({ row: { original: s } }) => (
          <span className="tabular text-[13px]">
            {s.summary ? formatCount(s.summary.totalFilesProcessed) : "–"}
          </span>
        ),
        meta: { className: "text-right" },
      }),
      col.display({
        id: "size",
        header: "Size",
        cell: ({ row: { original: s } }) => (
          <span className="tabular text-[13px]">
            {s.summary ? formatBytes(s.summary.totalBytesProcessed) : "–"}
          </span>
        ),
        meta: { className: "text-right" },
      }),
      col.display({
        id: "added",
        header: "Data added",
        cell: ({ row: { original: s } }) => (
          <span className="tabular text-[13px] text-muted-foreground">
            {s.summary ? formatBytes(s.summary.dataAddedPackedBytes) : "–"}
          </span>
        ),
        meta: { className: "text-right" },
      }),
    ],
    [],
  );

  const all = data?.snapshots;
  const rows = all?.slice(0, shown);
  const refreshError = refresh.error ? errorMessage(refresh.error) : data?.refreshError;
  return (
    <div className="flex flex-col gap-3">
      <div className="flex flex-wrap items-center gap-2">
        <Button
          type="button"
          variant="outline"
          disabled={refresh.isPending || !canRefresh}
          onClick={() => void doRefresh(workerId)}
        >
          <RefreshCw className={cn(refresh.isPending && "animate-spin")} aria-hidden />
          {refresh.isPending ? "Refreshing…" : "Refresh"}
        </Button>
        {data?.stale && !refresh.isPending && (
          <StatusPill
            meta={{ label: "Out of date", tone: "warning" }}
            title="A job changed the repository since the snapshots were listed."
          />
        )}
        <div className="flex items-center gap-2">
          <span className="text-[13px] text-muted-foreground" id="snapshots-via">
            via
          </span>
          <div className="w-52">
            <WorkerPicker
              aria-label="Worker that lists the snapshots"
              workers={workers}
              value={workerId}
              onChange={setWorkerId}
              autoLabel="Automatic"
              disabled={!canRefresh}
              className="bg-card"
            />
          </div>
        </div>
        <span className="text-xs text-muted-foreground sm:ml-auto">
          {data?.refreshedAt ? (
            <>
              Listed <RelativeTime date={tsDate(data.refreshedAt)} />
              {data.worker?.name ? ` by ${data.worker.name}` : ""}
            </>
          ) : (
            data && "Not listed yet"
          )}
        </span>
      </div>
      {!canRefresh && (
        <p className="text-[13px] text-muted-foreground">
          The list below is the last one taken; it cannot be refreshed while the data is being
          deleted.
        </p>
      )}
      {refreshError && (
        <p role="alert" className="rounded-md bg-danger-soft px-3 py-2 text-[13px] text-danger">
          Couldn't refresh the snapshots: {refreshError}
          {all && all.length > 0 ? " The list below is the last one taken." : ""}
        </p>
      )}
      {data?.truncated && (
        <p role="status" className="rounded-md bg-warning-soft px-3 py-2 text-[13px] text-warning">
          This repository has more snapshots than can be listed. The newest{" "}
          {formatCount(all?.length ?? 0)} are shown.
        </p>
      )}
      <DataTable
        aria-label="Snapshots"
        columns={columns}
        data={rows}
        isLoading={cached.isPending && !fresh}
        error={!fresh ? (cached.error ?? undefined) : undefined}
        onRetry={() => void cached.refetch()}
        getRowId={(s) => s.id}
        rowLabel={(s) => `Snapshot ${s.id.slice(0, 8)}`}
        onRowClick={(s) =>
          void navigate({
            to: "/repositories/$repositoryId/snapshots/$snapshotId",
            params: { repositoryId: repository.id, snapshotId: s.id },
          })
        }
        empty={
          <EmptyState
            icon={Camera}
            title={refresh.isPending ? "Listing the snapshots…" : "No snapshots yet"}
            description="A snapshot is one backup run. Back up some folders into this repository to get the first one."
          />
        }
      />
      {all && all.length > shown && (
        <div className="flex items-center justify-between text-[13px] text-muted-foreground">
          <span className="tabular">
            Showing {shown} of {formatCount(all.length)}
          </span>
          <Button variant="outline" size="sm" onClick={() => setShown((n) => n + PAGE)}>
            Show more
          </Button>
        </div>
      )}
    </div>
  );
}
