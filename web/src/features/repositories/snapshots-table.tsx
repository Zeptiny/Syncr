import { useMutation, useQuery } from "@connectrpc/connect-query";
import { useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { createColumnHelper } from "@tanstack/react-table";
import { Camera, RefreshCw } from "lucide-react";
import { useEffect, useId, useMemo, useRef, useState } from "react";

import { DataTable } from "@/components/app/data-table";
import { EmptyState } from "@/components/app/empty-state";
import { FilterBar, SearchInput } from "@/components/app/filter-bar";
import { RelativeTime } from "@/components/app/relative-time";
import { ShortId } from "@/components/app/short-id";
import { StatusPill } from "@/components/app/status-badge";
import { WorkerPicker } from "@/components/app/worker-picker";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { Switch } from "@/components/ui/switch";
import type { Worker } from "@/gen/syncr/v1/api_pb";
import { RemoteService } from "@/gen/syncr/v1/remotes_pb";
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

import {
  filterSnapshots,
  groupSnapshots,
  hostsOf,
  isFiltered,
  noFilter,
  tagsOf,
  type SnapshotFilter,
} from "./snapshot-groups";
import { RemoteBadge } from "./remote-badge";
import { remoteNames, remoteOrigin, tagLabel, userTags } from "./snapshot-origin";

const col = createColumnHelper<ResticSnapshot>();
const PAGE = 100;

/** SnapshotsTableProps configures SnapshotsTable. */
export interface SnapshotsTableProps {
  repository: Repository;
  /** workers are offered as the one that lists the snapshots. */
  workers: Worker[];
  /**
   * rowActions renders a row's actions in a last column, given the
   * snapshot before it in its group (same host and paths), e.g. "Compare
   * with previous". Without it there is no such column.
   */
  rowActions?: (snapshot: ResticSnapshot, previous: ResticSnapshot | undefined) => React.ReactNode;
}

const ALL = "__all__";

/**
 * SnapshotsTable lists a repository's snapshots from the controller's
 * cache and refreshes it through a worker: once by itself when the cache
 * is out of date, and whenever Refresh is pressed. Rows open the snapshot
 * browser. While the repository's data is being deleted the cache can
 * still be read, but not refreshed. The list can be filtered by text
 * (ID, host, tag, path), host and tag, and grouped by host and paths
 * like restic groups them. Snapshots of a remote show the remote ("From
 * Nextcloud") instead of their paths, which are the worker's internal
 * mirror or mount folder.
 */
export function SnapshotsTable({ repository, workers, rowActions }: SnapshotsTableProps) {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const [workerId, setWorkerId] = useState("");
  const [shown, setShown] = useState(PAGE);
  const [filter, setFilter] = useState<SnapshotFilter>(noFilter);
  const [grouped, setGrouped] = useState(false);
  const groupId = useId();
  const [expanded, setExpanded] = useState<ReadonlySet<string>>(new Set());
  const cached = useQuery(ResticService.method.listSnapshots, {
    repositoryId: repository.id,
    refresh: false,
    workerId: "",
  });
  const refresh = useMutation(ResticService.method.listSnapshots);
  const remotes = useQuery(RemoteService.method.listRemotes, {});
  const names = useMemo(() => remoteNames(remotes.data?.remotes ?? []), [remotes.data]);
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

  const all = data?.snapshots;
  // The previous snapshot of each one's group, over the whole list.
  const previous = useMemo(() => {
    const m = new Map<string, ResticSnapshot>();
    for (const g of groupSnapshots(all ?? []))
      g.snapshots.forEach((x, i) => {
        const before = g.snapshots[i + 1];
        if (before) m.set(x.id, before);
      });
    return m;
  }, [all]);
  const hosts = useMemo(() => hostsOf(all ?? []), [all]);
  const tags = useMemo(() => tagsOf(all ?? []), [all]);
  const matching = useMemo(() => filterSnapshots(all ?? [], filter, names), [all, filter, names]);
  const groups = useMemo(() => (grouped ? groupSnapshots(matching) : []), [grouped, matching]);
  const filtered = isFiltered(filter);

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
        cell: (c) => <span className="text-[13px] whitespace-nowrap">{c.getValue() || "–"}</span>,
      }),
      col.display({
        id: "paths",
        header: "Paths",
        cell: ({ row: { original: s } }) => (
          <RemoteBadge snapshot={s} names={names}>
            <span
              className="block max-w-[16rem] truncate font-mono text-[12.5px]"
              title={s.paths.join("\n")}
            >
              {s.paths.join(", ") || "–"}
            </span>
          </RemoteBadge>
        ),
      }),
      col.display({
        id: "tags",
        header: "Tags",
        cell: ({ row: { original: s } }) =>
          userTags(s).length ? (
            <span className="inline-flex flex-wrap gap-1">
              {userTags(s).map((t) => (
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
          <span className="tabular text-[13px] whitespace-nowrap">
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
      ...(rowActions
        ? [
            col.display({
              id: "actions",
              header: () => <span className="sr-only">Actions</span>,
              cell: ({ row: { original: s } }) => (
                <span onClick={(e) => e.stopPropagation()} onKeyDown={(e) => e.stopPropagation()}>
                  {rowActions(s, previous.get(s.id))}
                </span>
              ),
              meta: { className: "w-12 text-right" },
            }),
          ]
        : []),
    ],
    [rowActions, previous, names],
  );

  const rows = all ? matching.slice(0, shown) : undefined;
  const open = (s: ResticSnapshot) =>
    void navigate({
      to: "/repositories/$repositoryId/snapshots/$snapshotId",
      params: { repositoryId: repository.id, snapshotId: s.id },
    });
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
      {all && all.length > 0 && (
        <FilterBar className="mb-0" active={filtered} onClear={() => setFilter(noFilter)}>
          <SearchInput
            value={filter.text}
            onChange={(text) => setFilter((f) => ({ ...f, text }))}
            placeholder="Filter by ID, host, tag or path"
            className="sm:w-72"
          />
          {hosts.length > 1 && (
            <Select
              value={filter.host || ALL}
              onValueChange={(v) => setFilter((f) => ({ ...f, host: v === ALL ? "" : v }))}
            >
              <SelectTrigger aria-label="Host" className="w-full bg-card sm:w-44">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={ALL}>All hosts</SelectItem>
                {hosts.map((h) => (
                  <SelectItem key={h} value={h}>
                    {h}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}
          {tags.length > 1 && (
            <Select
              value={filter.tag || ALL}
              onValueChange={(v) => setFilter((f) => ({ ...f, tag: v === ALL ? "" : v }))}
            >
              <SelectTrigger aria-label="Tag" className="w-full bg-card sm:w-40">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value={ALL}>All tags</SelectItem>
                {tags.map((t) => (
                  <SelectItem key={t} value={t}>
                    {tagLabel(t, names)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          )}
          <span className="flex items-center gap-2">
            <Switch id={groupId} checked={grouped} onCheckedChange={setGrouped} />
            <label htmlFor={groupId} className="text-[13px]">
              Group by host and paths
            </label>
          </span>
          {filtered && (
            <span className="tabular text-xs text-muted-foreground sm:ml-auto">
              {formatCount(matching.length)} of {formatCount(all.length)}
            </span>
          )}
        </FilterBar>
      )}
      {grouped && all ? (
        groups.length === 0 ? (
          <EmptyState title="No snapshot matches the filter" className="rounded-lg border py-10" />
        ) : (
          groups.map((g) => {
            const from = remoteOrigin(g.first, names);
            const label = `${g.host || "Unknown host"}: ${from ?? (g.paths.join(", ") || "no paths")}`;
            const open_ = expanded.has(g.key);
            const list = open_ ? g.snapshots : g.snapshots.slice(0, PAGE);
            return (
              <section key={g.key} aria-label={label} className="flex flex-col gap-2">
                <h3 className="flex flex-wrap items-baseline gap-x-2 text-[13px] font-semibold">
                  {g.host || "Unknown host"}
                  <RemoteBadge snapshot={g.first} names={names}>
                    <span className="font-mono text-[12.5px] font-normal text-muted-foreground">
                      {g.paths.join(", ") || "–"}
                    </span>
                  </RemoteBadge>
                  <span className="tabular text-xs font-normal text-muted-foreground">
                    {g.snapshots.length === 1
                      ? "1 snapshot"
                      : `${formatCount(g.snapshots.length)} snapshots`}
                  </span>
                </h3>
                <DataTable
                  aria-label={`Snapshots of ${label}`}
                  columns={columns}
                  data={list}
                  getRowId={(s) => s.id}
                  rowLabel={(s) => `Snapshot ${s.id.slice(0, 8)}`}
                  onRowClick={open}
                />
                {list.length < g.snapshots.length && (
                  <Button
                    variant="outline"
                    size="sm"
                    className="self-end"
                    onClick={() => setExpanded((e) => new Set([...e, g.key]))}
                  >
                    Show all {formatCount(g.snapshots.length)}
                  </Button>
                )}
              </section>
            );
          })
        )
      ) : (
        <DataTable
          aria-label="Snapshots"
          columns={columns}
          data={rows}
          isLoading={cached.isPending && !fresh}
          error={!fresh ? (cached.error ?? undefined) : undefined}
          onRetry={() => void cached.refetch()}
          getRowId={(s) => s.id}
          rowLabel={(s) => `Snapshot ${s.id.slice(0, 8)}`}
          onRowClick={open}
          empty={
            filtered ? (
              <EmptyState title="No snapshot matches the filter" />
            ) : (
              <EmptyState
                icon={Camera}
                title={refresh.isPending ? "Listing the snapshots…" : "No snapshots yet"}
                description="A snapshot is one backup run. Back up some folders into this repository to get the first one."
              />
            )
          }
        />
      )}
      {!grouped && all && matching.length > shown && (
        <div className="flex items-center justify-between text-[13px] text-muted-foreground">
          <span className="tabular">
            Showing {shown} of {formatCount(matching.length)}
          </span>
          <Button variant="outline" size="sm" onClick={() => setShown((n) => n + PAGE)}>
            Show more
          </Button>
        </div>
      )}
    </div>
  );
}
