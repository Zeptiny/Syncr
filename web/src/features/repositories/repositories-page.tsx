import { useQuery } from "@connectrpc/connect-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { createColumnHelper } from "@tanstack/react-table";
import {
  Archive,
  FolderSearch,
  MoreHorizontal,
  Play,
  Plus,
  Settings2,
  ShieldCheck,
  SquareArrowOutUpRight,
} from "lucide-react";
import { useMemo } from "react";

import { DataTable } from "@/components/app/data-table";
import { EmptyState } from "@/components/app/empty-state";
import { PageHeader } from "@/components/app/page-header";
import { RelativeTime } from "@/components/app/relative-time";
import { StatusBadge, StatusPill } from "@/components/app/status-badge";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { ResticService, type Repository, type RepositoryJobRef } from "@/gen/syncr/v1/restic_pb";
import { formatBytes, tsDate } from "@/lib/format";
import { cn } from "@/lib/utils";

import { BackendIcon } from "../remotes/backend-icon";

const col = createColumnHelper<Repository>();

/** DeletingBadge marks a repository whose backup data is being deleted. */
export function DeletingBadge({ className }: { className?: string }) {
  return (
    <StatusPill
      meta={{ label: "Deleting data…", tone: "warning", pulse: true }}
      className={className}
    />
  );
}

/** RepositoryLocation shows where a repository lives: the remote and the path in it. */
export function RepositoryLocation({ repo, className }: { repo: Repository; className?: string }) {
  return (
    <span className={cn("flex min-w-0 items-center gap-2", className)}>
      <BackendIcon id={repo.remote?.backendType ?? ""} className="size-6 shrink-0" />
      <span className="flex min-w-0 flex-col">
        <span
          className={cn(
            "truncate text-[13px] font-medium",
            repo.remote?.deleted && "text-muted-foreground line-through",
          )}
        >
          {repo.remote?.name || "Unknown remote"}
        </span>
        <span className="truncate font-mono text-[12px] text-muted-foreground">
          {repo.path || "/"}
        </span>
      </span>
    </span>
  );
}

function LastJob({ job, empty }: { job: RepositoryJobRef | undefined; empty: string }) {
  if (!job) return <span className="text-[13px] text-muted-foreground">{empty}</span>;
  return (
    <Link
      to="/jobs/$jobId"
      params={{ jobId: job.jobId }}
      onClick={(e) => e.stopPropagation()}
      className="flex w-fit flex-col items-start gap-1 hover:opacity-80"
    >
      <StatusBadge job={job.status} />
      <RelativeTime
        date={tsDate(job.finishedAt ?? job.createdAt)}
        className="text-xs text-muted-foreground"
      />
    </Link>
  );
}

/** RepositoriesPage lists the user's restic repositories. */
export function RepositoriesPage() {
  const navigate = useNavigate();
  const q = useQuery(
    ResticService.method.listRepositories,
    {},
    {
      // A deletion in progress changes the list when it ends.
      refetchInterval: (query) =>
        query.state.data?.repositories.some((r) => r.deleting) ? 5000 : false,
    },
  );
  const columns = useMemo(
    () => [
      col.accessor("name", {
        header: "Name",
        cell: ({ row: { original: r } }) => (
          <div className={cn("flex min-w-0 flex-col gap-0.5", r.deleting && "opacity-70")}>
            <span className="flex min-w-0 flex-wrap items-center gap-2">
              <span className="truncate font-medium">{r.name}</span>
              {r.deleting && <DeletingBadge />}
              {!r.deleting && r.deletionError && (
                <StatusPill meta={{ label: "Deletion failed", tone: "danger" }} />
              )}
            </span>
            {r.description && (
              <span className="truncate text-xs text-muted-foreground">{r.description}</span>
            )}
          </div>
        ),
        meta: { className: "min-w-[12rem] max-w-[20rem]" },
      }),
      col.display({
        id: "location",
        header: "Location",
        cell: ({ row: { original: r } }) => (
          <RepositoryLocation
            repo={r}
            className={cn("max-w-[18rem]", r.deleting && "opacity-60")}
          />
        ),
      }),
      col.display({
        id: "snapshots",
        header: "Snapshots",
        cell: ({ row: { original: r } }) => {
          const s = r.stats;
          return (
            <div className={cn("flex flex-col gap-0.5", r.deleting && "opacity-60")}>
              <span className="tabular text-[13px] font-medium">
                {s?.snapshotCount ?? 0}
                {s?.snapshotsTruncated ? "+" : ""}
              </span>
              <span className="text-xs whitespace-nowrap text-muted-foreground">
                {s?.latestSnapshotAt ? (
                  <>
                    latest <RelativeTime date={tsDate(s.latestSnapshotAt)} />
                  </>
                ) : s?.snapshotsRefreshedAt ? (
                  "none yet"
                ) : (
                  "not listed yet"
                )}
              </span>
            </div>
          );
        },
      }),
      col.display({
        id: "size",
        header: "Latest size",
        cell: ({ row: { original: r } }) => (
          <span className={cn("tabular text-[13px]", r.deleting && "opacity-60")}>
            {r.stats?.latestSnapshotAt ? formatBytes(r.stats.latestSnapshotBytes) : "–"}
          </span>
        ),
      }),
      col.display({
        id: "backup",
        header: "Last backup",
        cell: ({ row }) => <LastJob job={row.original.stats?.lastBackup} empty="Never" />,
      }),
      col.display({
        id: "check",
        header: "Last check",
        cell: ({ row }) => <LastJob job={row.original.stats?.lastCheck} empty="Never" />,
      }),
      col.display({
        id: "schedules",
        header: "Schedules",
        cell: ({ row }) => (
          <span className="tabular text-[13px]">{row.original.stats?.scheduleCount ?? 0}</span>
        ),
        meta: { className: "text-right" },
      }),
      col.display({
        id: "actions",
        header: () => <span className="sr-only">Actions</span>,
        cell: ({ row: { original: r } }) => (
          <span onClick={(e) => e.stopPropagation()} onKeyDown={(e) => e.stopPropagation()}>
            <DropdownMenu>
              <DropdownMenuTrigger asChild>
                <Button variant="ghost" size="icon-sm" aria-label={`Actions for ${r.name}`}>
                  <MoreHorizontal aria-hidden />
                </Button>
              </DropdownMenuTrigger>
              <DropdownMenuContent align="end">
                {r.deleting ? (
                  <DropdownMenuItem asChild disabled={!r.deletionJobId}>
                    <Link to="/jobs/$jobId" params={{ jobId: r.deletionJobId }}>
                      <SquareArrowOutUpRight aria-hidden />
                      View deletion job
                    </Link>
                  </DropdownMenuItem>
                ) : (
                  <>
                    <DropdownMenuItem asChild>
                      <Link to="/jobs/new" search={{ kind: "restic_backup", repository: r.id }}>
                        <Play aria-hidden />
                        Back up now
                      </Link>
                    </DropdownMenuItem>
                    <DropdownMenuItem asChild>
                      <Link to="/repositories/$repositoryId" params={{ repositoryId: r.id }}>
                        <FolderSearch aria-hidden />
                        Browse snapshots
                      </Link>
                    </DropdownMenuItem>
                    <DropdownMenuItem asChild>
                      <Link to="/jobs/new" search={{ kind: "restic_check", repository: r.id }}>
                        <ShieldCheck aria-hidden />
                        Check
                      </Link>
                    </DropdownMenuItem>
                    <DropdownMenuSeparator />
                    <DropdownMenuItem asChild>
                      <Link
                        to="/repositories/$repositoryId"
                        params={{ repositoryId: r.id }}
                        search={{ tab: "settings" }}
                      >
                        <Settings2 aria-hidden />
                        Settings
                      </Link>
                    </DropdownMenuItem>
                  </>
                )}
              </DropdownMenuContent>
            </DropdownMenu>
          </span>
        ),
        meta: { className: "w-12 text-right" },
      }),
    ],
    [],
  );
  return (
    <>
      <PageHeader
        title="Repositories"
        description="Restic repositories hold your backups as encrypted snapshots, inside one of your remotes. Jobs on the same repository run one at a time."
        actions={
          <Button asChild>
            <Link to="/repositories/new">
              <Plus aria-hidden />
              Add repository
            </Link>
          </Button>
        }
      />
      <DataTable
        aria-label="Repositories"
        columns={columns}
        data={q.data?.repositories}
        isLoading={q.isPending}
        error={q.error ?? undefined}
        onRetry={() => void q.refetch()}
        getRowId={(r) => r.id}
        rowLabel={(r) => r.name}
        onRowClick={(r) =>
          void navigate({ to: "/repositories/$repositoryId", params: { repositoryId: r.id } })
        }
        empty={
          <EmptyState
            icon={Archive}
            title="No repositories yet"
            description="A repository is where backups go: create a new one in a remote, or connect one you already have by its password."
            action={
              <Button asChild>
                <Link to="/repositories/new">Add a repository</Link>
              </Button>
            }
          />
        }
      />
    </>
  );
}
