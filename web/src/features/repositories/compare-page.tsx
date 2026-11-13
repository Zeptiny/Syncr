import { useQuery } from "@connectrpc/connect-query";
import { Link, useNavigate } from "@tanstack/react-router";
import {
  ArrowLeftRight,
  ArrowRight,
  Equal,
  FolderOpen,
  RefreshCw,
  TriangleAlert,
  X,
} from "lucide-react";
import { useState } from "react";

import { EmptyState } from "@/components/app/empty-state";
import { ErrorState } from "@/components/app/error-state";
import { SearchInput } from "@/components/app/filter-bar";
import { WorkerChooser } from "@/components/app/file-browser/worker-chooser";
import { PageHeader } from "@/components/app/page-header";
import { Panel } from "@/components/app/panel";
import { StatCard, StatGrid } from "@/components/app/stat-card";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { WorkerAdminService } from "@/gen/syncr/v1/api_pb";
import type { ResticDiffChange } from "@/gen/syncr/v1/link_pb";
import { ResticService } from "@/gen/syncr/v1/restic_pb";
import { fieldViolations } from "@/lib/errors";
import { formatBytes, formatCount } from "@/lib/format";
import { scanErrorText } from "@/lib/scans";
import { cn } from "@/lib/utils";

import { DeletingBadge } from "./repositories-page";
import { isLocalRepository, shortId } from "./model";
import {
  changeKind,
  changePlace,
  filterChanges,
  kindCounts,
  kindLabel,
  type ChangeKind,
} from "./scans";
import type { CompareSearch } from "./search";
import { findSnapshot, previousInGroup } from "./snapshot-groups";
import { SnapshotMenu } from "./snapshot-menu";

const PAGE = 200;

const tone: Record<ChangeKind, string> = {
  added: "bg-success-soft text-success",
  removed: "bg-danger-soft text-danger",
  changed: "bg-warning-soft text-warning",
  type: "bg-live-soft text-live",
  metadata: "bg-neutral-soft text-muted-foreground",
  other: "bg-neutral-soft text-muted-foreground",
};

/** ChangeBadge names a change's kind, coloured by kind. */
export function ChangeBadge({ kind, className }: { kind: ChangeKind; className?: string }) {
  return (
    <span
      className={cn(
        "inline-flex w-24 shrink-0 justify-center rounded px-1.5 py-0.5 text-[11.5px] font-medium",
        tone[kind],
        className,
      )}
    >
      {kindLabel(kind)}
    </span>
  );
}

/**
 * ComparePage compares two snapshots with restic diff, whole or one
 * folder: restic's totals first (absent when the comparison stopped
 * early, which it says), then the changes, filtered by kind (only kinds
 * that occur) and by text. Each change opens its folder in the snapshot
 * that has it. The comparison is in the URL; without "from" the previous
 * snapshot of the same host and paths is proposed.
 */
export function ComparePage({
  repositoryId,
  from,
  to,
  path,
  worker,
}: { repositoryId: string } & CompareSearch) {
  const navigate = useNavigate();
  const repoQ = useQuery(ResticService.method.getRepository, { repositoryId });
  const snaps = useQuery(ResticService.method.listSnapshots, {
    repositoryId,
    refresh: false,
    workerId: "",
  });
  const workers = useQuery(WorkerAdminService.method.listWorkers, {});
  const repo = repoQ.data?.repository;
  const local = isLocalRepository(repo);
  const list = snaps.data?.snapshots ?? [];
  const toSnap = to ? findSnapshot(list, to) : undefined;
  // Without "from", the previous snapshot of the newer one's host and paths.
  const fromId = from ?? (toSnap ? previousInGroup(list, toSnap)?.id : undefined);
  const fromSnap = fromId ? findSnapshot(list, fromId) : undefined;
  const ready = !!repo && !!fromId && !!to && fromId !== to && (!local || !!worker);
  const diff = useQuery(
    ResticService.method.compareSnapshots,
    {
      repositoryId,
      fromSnapshotId: fromId ?? "",
      toSnapshotId: to ?? "",
      path: path ?? "",
      workerId: worker ?? "",
    },
    { enabled: ready, retry: false, staleTime: Infinity },
  );

  const [kinds, setKinds] = useState<ReadonlySet<ChangeKind>>(new Set());
  const [text, setText] = useState("");
  const [shown, setShown] = useState(PAGE);
  const go = (next: CompareSearch) => {
    setKinds(new Set());
    setShown(PAGE);
    void navigate({
      to: "/repositories/$repositoryId/compare",
      params: { repositoryId },
      search: next,
    });
  };
  const current: CompareSearch = { from: fromId, to, path, worker };

  if (repoQ.error) return <ErrorState error={repoQ.error} onRetry={() => void repoQ.refetch()} />;
  if (!repo)
    return (
      <div className="flex flex-col gap-4">
        <Skeleton className="h-8 w-72" />
        <Skeleton className="h-64 w-full" />
      </div>
    );

  const data = diff.data;
  const changes = data?.changes ?? [];
  const counts = kindCounts(changes);
  const shownKinds = new Set([...kinds].filter((k) => counts.some((c) => c.kind === k)));
  const filtered = filterChanges(changes, shownKinds, text);
  const pathError = fieldViolations(diff.error).find((v) => v.field === "path");
  const toggleKind = (k: ChangeKind) => {
    setShown(PAGE);
    setKinds((s) => {
      const n = new Set(s);
      if (n.has(k)) n.delete(k);
      else n.add(k);
      return n;
    });
  };
  const open = (c: ResticDiffChange) => {
    const at = changePlace(c, fromId ?? "", to ?? "");
    void navigate({
      to: "/repositories/$repositoryId/snapshots/$snapshotId",
      params: { repositoryId, snapshotId: at.snapshotId },
      search: { path: at.path === "/" ? undefined : at.path, worker },
    });
  };
  const stats = data?.stats;

  return (
    <>
      <PageHeader
        back={{ to: "/repositories", label: "Repositories" }}
        title={
          <span className="flex flex-wrap items-center gap-3">
            Compare snapshots
            {repo.deleting && <DeletingBadge />}
          </span>
        }
        meta={
          <Link
            to="/repositories/$repositoryId"
            params={{ repositoryId }}
            className="text-[13px] font-medium text-primary hover:underline"
          >
            {repo.name}
          </Link>
        }
        description="What changed from an older snapshot to a newer one: added, removed and changed files."
      />
      <div className="flex flex-col gap-6">
        <Panel>
          <div className="flex flex-col gap-4">
            <div className="flex flex-wrap items-end gap-3">
              <div className="flex min-w-0 flex-col gap-1.5">
                <span className="text-xs font-medium text-muted-foreground">From (older)</span>
                <SnapshotMenu
                  snapshots={list}
                  value={fromId ?? ""}
                  onChange={(id) => go({ ...current, from: id })}
                  label="Compare from"
                  near={toSnap}
                  exclude={to}
                />
              </div>
              <ArrowRight
                className="mb-2.5 hidden size-4 text-muted-foreground sm:block"
                aria-hidden
              />
              <div className="flex min-w-0 flex-col gap-1.5">
                <span className="text-xs font-medium text-muted-foreground">To (newer)</span>
                <SnapshotMenu
                  snapshots={list}
                  value={to ?? ""}
                  onChange={(id) => go({ ...current, to: id })}
                  label="Compare to"
                  near={fromSnap}
                  exclude={fromId}
                />
              </div>
              <Button
                variant="ghost"
                disabled={!fromId || !to}
                onClick={() => go({ ...current, from: to, to: fromId })}
                aria-label="Swap the snapshots"
                title="Swap the snapshots"
              >
                <ArrowLeftRight aria-hidden />
                Swap
              </Button>
            </div>
            <div className="flex flex-wrap items-center gap-x-4 gap-y-2 text-[13px]">
              <span className="flex min-w-0 items-center gap-2">
                <span className="text-muted-foreground">Folder</span>
                {path ? (
                  <span className="font-mono break-all">{path}</span>
                ) : (
                  <span>the whole snapshot</span>
                )}
                {path && (
                  <Button
                    variant="ghost"
                    size="xs"
                    onClick={() => go({ ...current, path: undefined })}
                    aria-label="Compare the whole snapshots"
                    title="Compare the whole snapshots"
                  >
                    <X aria-hidden />
                    Whole snapshot
                  </Button>
                )}
              </span>
              {local && (
                <span className="flex items-center gap-1 text-muted-foreground">
                  Compared
                  <WorkerChooser
                    workers={workers.data?.workers ?? []}
                    op="restic_diff"
                    value={worker ?? ""}
                    onChange={(id) => go({ ...current, worker: id || undefined })}
                    required
                    label="Comparing worker"
                    menuLabel="Compared by"
                  />
                </span>
              )}
              {data?.worker?.name && !local && (
                <span className="text-xs text-muted-foreground">
                  Compared by {data.worker.name}
                </span>
              )}
            </div>
          </div>
        </Panel>

        {!fromId || !to ? (
          <EmptyState
            icon={ArrowLeftRight}
            title="Choose two snapshots"
            description={
              to && snaps.data
                ? "This snapshot is the oldest of its host and paths: choose one to compare it with."
                : "Choose the older and the newer snapshot above."
            }
            className="rounded-lg border py-10"
          />
        ) : fromId === to ? (
          <EmptyState
            icon={Equal}
            title="That is the same snapshot"
            description="Choose two different snapshots."
            className="rounded-lg border py-10"
          />
        ) : local && !worker ? (
          <EmptyState
            icon={ArrowLeftRight}
            title="Choose a worker first"
            description="This repository is on a worker's disk: only that worker can compare its snapshots."
            className="rounded-lg border py-10"
          />
        ) : diff.isFetching ? (
          <div className="flex flex-col gap-3" role="status">
            <span className="text-[13px] text-muted-foreground">
              Comparing {shortId(fromId)} with {shortId(to)}. This can take up to a minute.
            </span>
            <Skeleton className="h-24 w-full" />
            <Skeleton className="h-64 w-full" />
          </div>
        ) : diff.error ? (
          <div className="flex flex-col items-start gap-3 rounded-lg border px-5 py-4">
            <p role="alert" className="text-[13px] text-danger">
              {pathError
                ? `${path ?? "This folder"} is not a folder in both snapshots. Compare the whole snapshots instead.`
                : scanErrorText(diff.error)}
            </p>
            <Button variant="outline" size="sm" onClick={() => void diff.refetch()}>
              <RefreshCw aria-hidden />
              Try again
            </Button>
          </div>
        ) : data ? (
          <>
            {stats ? (
              <StatGrid className="grid-cols-2 md:grid-cols-5">
                <StatCard
                  label="Files added"
                  value={formatCount(stats.addedFiles)}
                  sub={`and ${formatCount(stats.addedDirs)} ${stats.addedDirs === 1n ? "folder" : "folders"}`}
                />
                <StatCard
                  label="Files removed"
                  value={formatCount(stats.removedFiles)}
                  sub={`and ${formatCount(stats.removedDirs)} ${stats.removedDirs === 1n ? "folder" : "folders"}`}
                />
                <StatCard label="Files changed" value={formatCount(stats.changedFiles)} />
                <StatCard label="Data added" value={formatBytes(stats.addedBytes)} />
                <StatCard label="Data removed" value={formatBytes(stats.removedBytes)} />
              </StatGrid>
            ) : (
              <p
                role="status"
                className="flex items-start gap-2 rounded-lg border border-warning/40 bg-warning-soft px-4 py-3 text-[13px] text-warning"
              >
                <TriangleAlert className="mt-0.5 size-4 shrink-0" aria-hidden />
                The comparison stopped at the time limit, so there are no totals, and the list holds
                only the changes found until then. Compare one folder to see all of its changes.
              </p>
            )}
            <Panel
              flush
              title="Changes"
              description={
                data.truncated
                  ? `Only the first ${formatCount(changes.length)} changes are listed${stats ? "; the totals above count them all" : ""}.`
                  : undefined
              }
            >
              {changes.length === 0 ? (
                <EmptyState
                  icon={Equal}
                  title="No changes"
                  description={`Nothing differs${path ? ` in ${path}` : ""} between these snapshots.`}
                  className="py-10"
                />
              ) : (
                <>
                  <div className="flex flex-wrap items-center gap-2 border-b px-4 py-3">
                    <div
                      role="group"
                      aria-label="Kinds of change"
                      className="flex flex-wrap gap-1.5"
                    >
                      {counts.map((c) => (
                        <Button
                          key={c.kind}
                          variant={shownKinds.has(c.kind) ? "secondary" : "outline"}
                          size="sm"
                          aria-pressed={shownKinds.has(c.kind)}
                          onClick={() => toggleKind(c.kind)}
                        >
                          {c.label}{" "}
                          <span className="tabular text-muted-foreground">
                            {formatCount(c.count)}
                          </span>
                        </Button>
                      ))}
                    </div>
                    <SearchInput
                      value={text}
                      onChange={(v) => {
                        setText(v);
                        setShown(PAGE);
                      }}
                      placeholder="Filter paths"
                      className="w-full sm:ml-auto sm:w-64"
                    />
                  </div>
                  {filtered.length === 0 ? (
                    <p className="px-4 py-8 text-center text-[13px] text-muted-foreground">
                      No change matches the filters.
                    </p>
                  ) : (
                    <ul aria-label="Changes" className="divide-y">
                      {filtered.slice(0, shown).map((c) => {
                        const k = changeKind(c);
                        return (
                          <li key={c.path} className="flex items-center gap-3 px-4 py-2">
                            <ChangeBadge kind={k} />
                            <span className="min-w-0 flex-1 font-mono text-[12.5px] break-all">
                              {c.path}
                            </span>
                            <Button
                              variant="ghost"
                              size="icon-sm"
                              onClick={() => open(c)}
                              aria-label={`Open the folder of ${c.path} in ${k === "removed" ? "the older" : "the newer"} snapshot`}
                              title={`Open its folder in ${k === "removed" ? "the older" : "the newer"} snapshot`}
                            >
                              <FolderOpen aria-hidden />
                            </Button>
                          </li>
                        );
                      })}
                    </ul>
                  )}
                  {filtered.length > shown && (
                    <div className="flex items-center justify-between gap-3 border-t px-4 py-2 text-[13px] text-muted-foreground">
                      <span className="tabular">
                        Showing {formatCount(shown)} of {formatCount(filtered.length)}
                      </span>
                      <Button variant="outline" size="sm" onClick={() => setShown((n) => n + PAGE)}>
                        Show {formatCount(Math.min(PAGE, filtered.length - shown))} more
                      </Button>
                    </div>
                  )}
                </>
              )}
            </Panel>
          </>
        ) : null}
      </div>
    </>
  );
}
