import { useQuery } from "@connectrpc/connect-query";
import { Link, useNavigate } from "@tanstack/react-router";
import { createColumnHelper } from "@tanstack/react-table";
import { Plus, Server } from "lucide-react";
import { useMemo } from "react";

import { DataTable } from "@/components/app/data-table";
import { EmptyState } from "@/components/app/empty-state";
import { FilterBar } from "@/components/app/filter-bar";
import { LabelChips } from "@/components/app/label-editor";
import { PageHeader } from "@/components/app/page-header";
import { RelativeTime } from "@/components/app/relative-time";
import { StatusBadge } from "@/components/app/status-badge";
import { sortWorkers } from "@/lib/workers";
import { Button } from "@/components/ui/button";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectSeparator,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select";
import { WorkerAdminService, type Worker } from "@/gen/syncr/v1/api_pb";
import { useSession } from "@/hooks/use-session";
import { tsDate } from "@/lib/format";
import { workerModeLabel } from "@/lib/status";

const col = createColumnHelper<Worker>();

/** platformLabel renders "linux/amd64". */
export function platformLabel(w: Worker): string {
  return w.info?.os ? `${w.info.os}/${w.info.arch}` : "–";
}

/** toolVersion returns a reported tool version, e.g. rclone's. */
export function toolVersion(w: Worker, tool: string): string | undefined {
  return w.info?.tools.find((t) => t.name === tool)?.version;
}

const ANY = "__any__";

/** WorkersPage lists the workers with their status and labels. */
export function WorkersPage({ label }: { label?: string }) {
  const { isAdmin } = useSession();
  const navigate = useNavigate();
  const q = useQuery(
    WorkerAdminService.method.listWorkers,
    { label: label ?? "" },
    { refetchInterval: 10_000, placeholderData: (prev) => prev },
  );
  // The label choices come from every worker, not just the filtered ones.
  const all = useQuery(WorkerAdminService.method.listWorkers, {}, { refetchInterval: 30_000 });
  const labels = [...new Set((all.data?.workers ?? []).flatMap((w) => w.labels))].sort();
  const columns = useMemo(
    () => [
      col.accessor("name", {
        header: "Worker",
        cell: ({ row: { original: w } }) => (
          <div className="flex min-w-0 flex-col">
            <span className="font-medium">{w.name}</span>
            <span className="truncate text-xs text-muted-foreground">
              {w.info?.hostname && w.info.hostname !== w.name
                ? w.info.hostname
                : workerModeLabel(w.mode) + " link"}
            </span>
          </div>
        ),
      }),
      col.accessor("status", {
        header: "Status",
        cell: (c) => <StatusBadge worker={c.getValue()} />,
      }),
      col.accessor("mode", {
        header: "Mode",
        cell: (c) => <span className="text-[13px]">{workerModeLabel(c.getValue())}</span>,
      }),
      col.accessor("labels", {
        header: "Labels",
        cell: (c) => (
          <LabelChips
            labels={c.getValue()}
            empty={<span className="text-[13px] text-muted-foreground">–</span>}
          />
        ),
      }),
      col.display({
        id: "platform",
        header: "Platform",
        cell: ({ row }) => (
          <span className="font-mono text-[12.5px]">{platformLabel(row.original)}</span>
        ),
      }),
      col.display({
        id: "version",
        header: "Versions",
        cell: ({ row: { original: w } }) =>
          w.info ? (
            <div className="flex flex-col text-[13px]">
              <span>{w.info.workerVersion}</span>
              <span className="text-xs text-muted-foreground">
                rclone {toolVersion(w, "rclone") ?? "missing"}
              </span>
            </div>
          ) : (
            <span className="text-[13px] text-muted-foreground">Not connected yet</span>
          ),
      }),
      col.accessor((w) => tsDate(w.lastSeen), {
        id: "lastSeen",
        header: "Last seen",
        cell: (c) => <RelativeTime date={c.getValue()} className="text-[13px]" />,
        meta: { className: "text-right" },
      }),
    ],
    [],
  );
  return (
    <>
      <PageHeader
        title="Workers"
        description="The machines that run transfers. Any user can run jobs on any worker; administrators add and remove them."
        actions={
          isAdmin && (
            <Button asChild>
              <Link to="/workers/new">
                <Plus aria-hidden />
                Add worker
              </Link>
            </Button>
          )
        }
      />
      {(labels.length > 0 || label) && (
        <FilterBar
          active={!!label}
          onClear={() => void navigate({ to: "/workers", search: {}, replace: true })}
        >
          <Select
            value={label ?? ANY}
            onValueChange={(v) =>
              void navigate({
                to: "/workers",
                search: v === ANY ? {} : { label: v },
                replace: true,
              })
            }
          >
            <SelectTrigger className="w-full bg-card sm:w-48" aria-label="Label">
              <SelectValue />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value={ANY}>Any label</SelectItem>
              <SelectSeparator />
              {labels.map((l) => (
                <SelectItem key={l} value={l}>
                  <span className="font-mono text-[13px]">{l}</span>
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
        </FilterBar>
      )}
      <DataTable
        aria-label="Workers"
        columns={columns}
        data={q.data ? sortWorkers(q.data.workers) : undefined}
        isLoading={q.isPending}
        error={q.error ?? undefined}
        onRetry={() => void q.refetch()}
        getRowId={(w) => w.id}
        rowLabel={(w) => w.name}
        onRowClick={(w) => void navigate({ to: "/workers/$workerId", params: { workerId: w.id } })}
        empty={
          label ? (
            <EmptyState
              icon={Server}
              title={`No worker has the label ${label}`}
              description="An administrator adds labels on a worker's page."
            />
          ) : (
            <EmptyState
              icon={Server}
              title="No workers yet"
              description="A worker is a small program on each machine that should send or receive files. Install one to run your first job."
              action={
                isAdmin && (
                  <Button asChild>
                    <Link to="/workers/new">Add worker</Link>
                  </Button>
                )
              }
            />
          )
        }
      />
    </>
  );
}
