import { useQuery } from "@connectrpc/connect-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { createColumnHelper } from "@tanstack/react-table";
import { Archive, Cloud, FolderSearch, KeyRound, MoreHorizontal, Pencil, Plus } from "lucide-react";
import { useMemo } from "react";

import { DataTable } from "@/components/app/data-table";
import { EmptyState } from "@/components/app/empty-state";
import { PageHeader } from "@/components/app/page-header";
import { RelativeTime } from "@/components/app/relative-time";
import { Button } from "@/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu";
import { RemoteService, type Remote } from "@/gen/syncr/v1/remotes_pb";
import { useBackendMap } from "@/hooks/use-backends";
import { tsDate } from "@/lib/format";

import { BackendIcon } from "./backend-icon";
import { isUnverified, UnverifiedBadge } from "./host-keys";

const col = createColumnHelper<Remote>();

/**
 * remoteSummary picks what best identifies where a remote points: a
 * union's members, where a wrapper (encrypted) remote stores its data, or
 * an option.
 */
export function remoteSummary(r: Remote, names: Map<string, string>): string {
  if (r.members.length) return r.members.map((m) => names.get(m.remoteId) ?? "?").join(" + ");
  if (r.wrapped?.remoteId) {
    const name = names.get(r.wrapped.remoteId) ?? "?";
    return r.wrapped.path ? `in ${name} / ${r.wrapped.path}` : `in ${name}`;
  }
  for (const k of ["endpoint", "url", "host", "path", "bucket", "account"]) {
    const v = r.options[k];
    if (v) return k === "host" && r.options.port ? `${v}:${r.options.port}` : v;
  }
  return "";
}

/** usedBy lists the remotes that use r: the unions it is a member of, then the remotes that store their data in it. */
export function usedBy(r: Remote, names: Map<string, string>): string[] {
  return [...r.usedByUnions, ...r.wrappedBy].map((id) => names.get(id) ?? id);
}

/** WrappedLocation shows where a wrapper remote stores its data: "in B2 offsite / vault". */
function WrappedLocation({ name, path }: { name: string; path: string }) {
  return (
    <span className="flex max-w-[22rem] min-w-0 items-baseline gap-1 text-[13px] text-muted-foreground">
      <span className="shrink-0">in</span>
      <span className="truncate font-medium text-foreground">{name}</span>
      {path && <span className="truncate font-mono text-[12.5px]">/ {path}</span>}
    </span>
  );
}

/** RemotesPage lists the user's remotes. */
export function RemotesPage() {
  const navigate = useNavigate();
  const backends = useBackendMap();
  const q = useQuery(RemoteService.method.listRemotes, {});
  const names = useMemo(
    () => new Map((q.data?.remotes ?? []).map((r) => [r.id, r.name])),
    [q.data],
  );
  const columns = useMemo(
    () => [
      col.accessor("name", {
        header: "Name",
        cell: ({ row: { original: r } }) => (
          <div className="flex min-w-0 items-center gap-3">
            <BackendIcon id={r.backendType} />
            <div className="flex min-w-0 flex-col">
              <span className="truncate font-medium">{r.name}</span>
              <span className="flex min-w-0 flex-wrap items-center gap-x-2 text-xs text-muted-foreground">
                <span className="truncate">
                  {backends.get(r.backendType)?.label ?? r.backendType}
                </span>
                {isUnverified(r) && <UnverifiedBadge />}
              </span>
            </div>
          </div>
        ),
      }),
      col.display({
        id: "where",
        header: "Points to",
        cell: ({ row: { original: r } }) =>
          r.wrapped?.remoteId ? (
            <WrappedLocation name={names.get(r.wrapped.remoteId) ?? "?"} path={r.wrapped.path} />
          ) : (
            <span className="block max-w-[22rem] truncate font-mono text-[12.5px] text-muted-foreground">
              {remoteSummary(r, names) || "–"}
            </span>
          ),
      }),
      col.display({
        id: "secrets",
        header: "Credentials",
        cell: ({ row: { original: r } }) =>
          r.secretsSet.length ? (
            <span className="inline-flex items-center gap-1.5 text-[13px]">
              <KeyRound className="size-3.5 text-success" aria-hidden />
              {r.secretsSet.length === 1
                ? "1 secret saved"
                : `${r.secretsSet.length} secrets saved`}
            </span>
          ) : (
            <span className="text-[13px] text-muted-foreground">None</span>
          ),
      }),
      col.display({
        id: "used-by",
        header: "Used by",
        cell: ({ row }) => (
          <span className="text-[13px]">
            {usedBy(row.original, names).join(", ") || (
              <span className="text-muted-foreground">–</span>
            )}
          </span>
        ),
      }),
      col.accessor((r) => tsDate(r.updatedAt), {
        id: "updated",
        header: "Updated",
        cell: (c) => <RelativeTime date={c.getValue()} className="text-[13px]" />,
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
                <DropdownMenuItem asChild>
                  <Link to="/remotes/$remoteId/files" params={{ remoteId: r.id }}>
                    <FolderSearch aria-hidden />
                    Browse
                  </Link>
                </DropdownMenuItem>
                <DropdownMenuItem asChild>
                  <Link to="/remotes/$remoteId" params={{ remoteId: r.id }}>
                    <Pencil aria-hidden />
                    Edit
                  </Link>
                </DropdownMenuItem>
                {backends.get(r.backendType)?.capabilities?.resticRepository && (
                  <DropdownMenuItem asChild>
                    <Link to="/repositories/new" search={{ remote: r.id }}>
                      <Archive aria-hidden />
                      Add a repository here
                    </Link>
                  </DropdownMenuItem>
                )}
              </DropdownMenuContent>
            </DropdownMenu>
          </span>
        ),
        meta: { className: "w-12 text-right" },
      }),
    ],
    [backends, names],
  );
  return (
    <>
      <PageHeader
        title="Remotes"
        description="Storage that jobs read from and write to. Credentials are encrypted on the controller and never shown again."
        actions={
          <Button asChild>
            <Link to="/remotes/new">
              <Plus aria-hidden />
              Add remote
            </Link>
          </Button>
        }
      />
      <DataTable
        aria-label="Remotes"
        columns={columns}
        data={q.data?.remotes}
        isLoading={q.isPending}
        error={q.error ?? undefined}
        onRetry={() => void q.refetch()}
        getRowId={(r) => r.id}
        rowLabel={(r) => r.name}
        onRowClick={(r) => void navigate({ to: "/remotes/$remoteId", params: { remoteId: r.id } })}
        empty={
          <EmptyState
            icon={Cloud}
            title="No remotes yet"
            description="Add the storage you want to copy to or from: an S3 bucket, an SFTP server, a folder on a worker and more."
            action={
              <Button asChild>
                <Link to="/remotes/new">Add remote</Link>
              </Button>
            }
          />
        }
      />
    </>
  );
}
