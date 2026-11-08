import { useQuery } from "@connectrpc/connect-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { createColumnHelper } from "@tanstack/react-table";
import { Cloud, KeyRound, Plus } from "lucide-react";
import { useMemo } from "react";

import { DataTable } from "@/components/app/data-table";
import { EmptyState } from "@/components/app/empty-state";
import { PageHeader } from "@/components/app/page-header";
import { RelativeTime } from "@/components/app/relative-time";
import { Button } from "@/components/ui/button";
import { RemoteService, type Remote } from "@/gen/syncr/v1/remotes_pb";
import { useBackendMap } from "@/hooks/use-backends";
import { tsDate } from "@/lib/format";

import { BackendIcon } from "./backend-icon";

const col = createColumnHelper<Remote>();

/** remoteSummary picks the option that best identifies where a remote points. */
export function remoteSummary(r: Remote, names: Map<string, string>): string {
  if (r.members.length) return r.members.map((m) => names.get(m.remoteId) ?? "?").join(" + ");
  for (const k of ["endpoint", "url", "host", "path", "bucket", "account"]) {
    const v = r.options[k];
    if (v) return k === "host" && r.options.port ? `${v}:${r.options.port}` : v;
  }
  return "";
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
              <span className="truncate text-xs text-muted-foreground">
                {backends.get(r.backendType)?.label ?? r.backendType}
              </span>
            </div>
          </div>
        ),
      }),
      col.display({
        id: "where",
        header: "Points to",
        cell: ({ row }) => (
          <span className="block max-w-[22rem] truncate font-mono text-[12.5px] text-muted-foreground">
            {remoteSummary(row.original, names) || "–"}
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
        id: "unions",
        header: "In unions",
        cell: ({ row }) => (
          <span className="text-[13px]">
            {row.original.usedByUnions.map((id) => names.get(id) ?? id).join(", ") || (
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
